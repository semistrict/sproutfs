//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

const (
	// scatterRanges is how many 2 MiB ranges of a memory region a guest writes into,
	// which is how many extents the placement rule hands out for it.
	scatterRanges = 8
	// scatterGuests is how many guests write at once, which is what the fan-out
	// the workload runs is: several children of one point, storing into their
	// own memory on one pager and checkpointed on an interval while they do.
	scatterGuests = 3
	// scatterInterval is how often each guest's memory region is checkpointed, which is
	// short enough that a round of stores spans several of them.
	scatterInterval = 15 * time.Millisecond
	// scatterVMAs is the mapping budget each guest's client admits replacements
	// against, as a deployment gives one. A guest alternating private and shared
	// pages reaches it, which is what puts the refusal and the backstop that
	// answers it on this path.
	scatterVMAs = 4096
)

// scatterRounds is how many times each guest writes its whole memory region. A run of
// the suite takes the few that say whether the page-table work holds together
// at all; a hunt for something rarer raises it.
func scatterRounds(t *testing.T) int {
	if raw := os.Getenv("SPROUTFS_SCATTER_ROUNDS"); raw != "" {
		rounds, err := strconv.Atoi(raw)
		if err != nil || rounds < 1 {
			t.Fatalf("SPROUTFS_SCATTER_ROUNDS=%q is not a count of rounds", raw)
		}
		return rounds
	}
	return 8
}

// A guest storing into scattered pages of a 4 KiB memory region while a checkpoint of
// it is taken on an interval is the whole of what the pager's page-table work
// has to survive, and it is what the workload's fork fan-out does: every page
// the guest writes is placed at the offset it has within its range, the rules
// copy the pages around it, a batch's contiguous runs are staged as one span, a
// seal write-protects the runs of the dirty set, and a settle hands back what
// the guest never really wrote — all of it against a real client, a real arena
// and a real userfaultfd.
//
// What it asserts is that nothing in that ends the session: a mapping the
// client refuses, a run the kernel will not install, wake or protect, or a page
// the pager resolves as memory where it left the guest a trap all come back
// here as a failed fault, a failed seal or a session that is gone. The bytes
// are checked too, because a mapping that lands from the wrong arena offset
// fails no command at all.
func TestManagedPagerScatteredStoresUnderIntervalCheckpoints(t *testing.T) {
	const size = checkpoint.PageSize4KiB
	const pages = scatterRanges * rangePages
	h := kernelHostConfigured(t, vmmemory.Config{PageSize: size,
		// The arena holds one extent per range of every guest and an ordinary
		// run besides, and its capacity is a fraction of what the guests write,
		// so reclaim, spill and revocation run throughout.
		ResidentPages: 2 * rangePages, ArenaOffsets: 2*rangePages + scatterGuests*scatterRanges*rangePages,
		LogicalPages: 4 * scatterGuests * pages, DirtyPages: rangePages,
		ReadAheadPages: rangePages, WriteAheadPages: 16, SettleWorkers: 2})
	config := vmmemory.ConnectionConfig{Name: "ram", QueuePages: pages, MaxVMAs: scatterVMAs,
		CommandTimeout: 30 * time.Second, VerifyInterval: time.Hour}
	rounds := scatterRounds(t)

	guests := make([]*nativeProcess, scatterGuests)
	for i := range guests {
		// Two volumes of one page, as a VMM has. Each is its own object, so a
		// page one guest publishes is named by that guest's own checkpoint and
		// never shared with another's under a name they both claim.
		backings := []vmmemory.Backing{
			newPagedKernelBacking(byte(1+2*i), pages*size, size),
			newPagedKernelBacking(byte(2+2*i), pages*size, size),
		}
		for _, b := range backings {
			kernel := b.(*kernelBacking)
			for page := range uint64(pages) {
				// Every fourth page is a hole, which is what a guest's free
				// memory is: the pager maps zeros for it and a store into one
				// takes a fresh private page and its write-ahead run rather than
				// copying anything.
				if page%4 == 3 {
					kernel.hole(page)
					continue
				}
				for j := range size {
					kernel.data[int(page)*size+j] = pageByte(0, int(page))
				}
			}
		}
		guests[i] = startNativeWithConfig(t, h, pages, config, backings...)
	}

	stop := make(chan struct{})
	var checkpoints sync.WaitGroup
	failures := make([]error, scatterGuests)
	for i, guest := range guests {
		checkpoints.Add(1)
		go func() {
			defer checkpoints.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(scatterInterval):
				}
				if err := checkpointMemoryRegion(t.Context(), guest, 1); err != nil {
					failures[i] = errors.Join(failures[i], err)
					return
				}
			}
		}()
	}

	var writers sync.WaitGroup
	stores := make([]error, scatterGuests)
	for i, guest := range guests {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for round := range rounds {
				// Every round writes on a different stride, so the private pages
				// of a range alternate with shared ones differently each time:
				// one page in four, then every page, then one in three. That is
				// what makes a store close a gap, a range reach half its own
				// pages, and a batch carry runs that are next to each other in
				// the memory region and scattered through the arena.
				stride := size * (1 + round%4)
				value := byte(1 + round)
				// Each round starts lower than the one before it and sweeps to
				// the end, so a store always has pages an interval checkpoint is
				// holding both below it and above it. A rule that reached past
				// one of those is what leaves a page private and unmapped.
				offset := (rounds - 1 - round) * pages * size / rounds
				length := pages*size - offset
				if err := guest.ask(fmt.Sprintf("stridefill 1 %d %d %d %d", offset, length, stride, value), "strided"); err != nil {
					stores[i] = err
					return
				}
				if err := guest.ask(fmt.Sprintf("stridescan 1 %d %d %d %d", offset, length, stride, value), "strided"); err != nil {
					stores[i] = err
					return
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	checkpoints.Wait()
	for i := range guests {
		if err := errors.Join(stores[i], failures[i]); err != nil {
			t.Fatalf("guest %d: %v", i, err)
		}
	}
	for i, guest := range guests {
		if err := guest.connections[1].Wait(timeoutContext(t)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("guest %d's RAM session ended: %v", i, err)
		}
	}
}

// timeoutContext is a context that is done almost at once, so a session that is
// still serving reports the deadline rather than a failure.
func timeoutContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// checkpointMemoryRegion is one interval checkpoint of one memory region: the seal the guest
// asks for over the control protocol, the settle behind its pause, the
// publication of what is left, and the retirement that makes those pages clean.
func checkpointMemoryRegion(ctx context.Context, p *nativeProcess, memoryRegion int) error {
	r := p.memoryRegion(memoryRegion)
	if err := r.Seal(ctx); err != nil {
		if errors.Is(err, vmmemory.ErrSealed) {
			return nil
		}
		return fmt.Errorf("sealing memory region %d: %w", memoryRegion, err)
	}
	ckpt := r.Checkpoint()
	if _, err := ckpt.Settle(ctx); err != nil {
		return fmt.Errorf("settling memory region %d: %w", memoryRegion, err)
	}
	published, err := p.backing[memoryRegion].publish(ctx, ckpt)
	if err != nil {
		return fmt.Errorf("publishing memory region %d: %w", memoryRegion, err)
	}
	if err := ckpt.Retire(ctx, published); err != nil {
		return fmt.Errorf("retiring memory region %d: %w", memoryRegion, err)
	}
	return nil
}
