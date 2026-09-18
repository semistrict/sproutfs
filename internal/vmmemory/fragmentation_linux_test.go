//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

func TestNativeMappingBudgetRejectsBeforeKernelMutation(t *testing.T) {
	const pages = 256
	h := kernelHost(t, pages, 2*pages)
	a := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: 16, MaxVMAs: 128, CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour})
	region := a.region(0)
	for page := uint64(0); page < pages; page += 2 {
		before, _ := h.Stats(t.Context())
		if err := region.Fault(t.Context(), page, true); err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("mapping budget returned unexpected error: %v", err)
			}
			after, _ := h.Stats(t.Context())
			if after.RemapEvents != before.RemapEvents {
				t.Fatal("rejected mapping mutated the address space")
			}
			if after.ResidentPages < before.ResidentPages {
				t.Fatal("mapping rejection released possibly live slots")
			}
			return
		}
	}
	t.Fatal("fragmented private mappings exceeded the configured VMA admission budget")
}

// A refused mapping command is a failed operation, not a failed session. The
// client admits a command against its mapping budget before it touches
// anything, so a refusal changed nothing: the pages are not mapped, the region
// goes on serving its guest, and the fault is served again once revocation has
// freed the budget. Ending the session here would kill a VMM over a command
// that did nothing, and recording the refused pages as mapped would resolve a
// later fault against a mapping the client never installed.
func TestNativeRefusedMappingLeavesTheSessionServing(t *testing.T) {
	const pages = 256
	h := kernelHost(t, pages, 2*pages)
	a := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: 16, MaxVMAs: 128, CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour})
	region := a.region(0)
	refused, found := uint64(0), false
	for page := uint64(0); page < pages && !found; page += 2 {
		err := region.Fault(t.Context(), page, true)
		if err == nil {
			continue
		}
		if !errors.Is(err, vmmemory.ErrMappingRefused) || !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("faulting page %d = %v, want the client's refusal", page, err)
		}
		refused, found = page, true
	}
	if !found {
		t.Fatal("fragmented private mappings exceeded the configured VMA admission budget")
	}
	if err := region.Verify(t.Context()); err != nil {
		t.Fatalf("a refused mapping command left the region unable to serve: %v", err)
	}
	// The refused page is refused again, rather than resolved against a mapping
	// the client never installed, which is what recording it as mapped would
	// leave for the fault that comes back to it.
	if err := region.Fault(t.Context(), refused, true); !errors.Is(err, vmmemory.ErrMappingRefused) {
		t.Fatalf("faulting the refused page again = %v, want the refusal again", err)
	}
	// The session still serves: its control path answers a seal, which is
	// page-table work and costs the client's budget nothing.
	a.seal(0)
	if s, err := h.Stats(t.Context()); err != nil {
		t.Fatalf("the host after a refused mapping command: %v", err)
	} else if s.RefusedMappings != 0 {
		t.Fatalf("%d faults were deferred for a refusal, want none: these were the pager's own, not a client's access", s.RefusedMappings)
	}
	// A guest's own access is refused the same way, and the worker serving it
	// queues that fault again rather than ending the session: the guest waits
	// for the budget, as it waits for the dirty budget, and the VM stays alive
	// to be checkpointed or migrated off this host.
	if _, err := fmt.Fprintf(a.input, "fill 0 %d 1 42\n", vmmemory.PageSize); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := h.Stats(t.Context())
		if err != nil {
			t.Fatalf("the host while a client's access was refused: %v", err)
		}
		if s.RefusedMappings > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client's refused access was never deferred: the session ended on it instead")
		}
		time.Sleep(time.Millisecond)
	}
	if err := region.Verify(t.Context()); err != nil {
		t.Fatalf("a client's refused access left the region unable to serve: %v", err)
	}
	// Revocation is what frees the budget, and the client admits one whatever
	// its budget holds: the range a revocation replaces becomes a trap that
	// merges with the traps around it, so it installs nothing. Abandoning the
	// seal revokes every page the guest had, and the deferred fault is served
	// as soon as it lands.
	if err := region.Unseal(t.Context()); err != nil {
		t.Fatalf("abandoning the seal to revoke the guest's mappings: %v", err)
	}
	if got := a.line(); got != "filled" {
		t.Fatalf("the deferred access answered %q, want it served once the revocations freed the budget", got)
	}
	a.request(fmt.Sprintf("read 0 %d 1", vmmemory.PageSize), "data 2a")
	if err := region.Fault(t.Context(), refused, true); err != nil {
		t.Fatalf("the page whose mapping was refused, faulted again after the revocations: %v", err)
	}
}

func TestNativeAbandonedCheckpointCoalescesRevokesAcrossGenerationBoundaries(t *testing.T) {
	const pages = 128
	h := kernelHost(t, 2*pages, 2*pages)
	a := startNative(t, h, pages)
	region := a.region(0)
	// Odd pages first acquire a clean generation; every page then becomes private.
	for page := uint64(1); page < pages; page += 2 {
		if err := region.Fault(t.Context(), page, false); err != nil {
			t.Fatal(err)
		}
	}
	for page := range uint64(pages) {
		if err := region.Fault(t.Context(), page, true); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := h.Stats(t.Context())
	// An abandoned checkpoint is what revokes a whole dirty set: it takes the
	// read-only mappings the seal installed away, in one command over the
	// contiguous run.
	if err := region.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := region.Unseal(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := h.Stats(t.Context())
	if after.Revocations-before.Revocations != 1 || after.RemapEvents-before.RemapEvents != 1 {
		t.Fatalf("one contiguous dirty checkpoint used %d commands and %d kernel remaps", after.Revocations-before.Revocations, after.RemapEvents-before.RemapEvents)
	}
	// Read the actual native address after the checkpoint; mapping generations
	// still distinguish the former clean and initially missing pages.
	a.request("read 0 0 1", "data 01")
	a.request("read 0 "+fmt.Sprint(vmmemory.PageSize)+" 1", "data 02")
}

// Stores the exact workload sparsely: each write changes only the first byte
// of a page. It validates every other byte before retaining that compact value.
// The pager still writes/spills full pages through actual files and mappings.
type patternBacking struct {
	size            uint64
	vm              string
	mu              sync.Mutex
	values          map[uint64]byte
	writes, batches uint64
}

func (b *patternBacking) Size() uint64                     { return b.size }
func (b *patternBacking) Verify(ctx context.Context) error { return context.Cause(ctx) }
func (b *patternBacking) Load(ctx context.Context, offset uint64, data []byte) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(data)
	size := uint64(vmmemory.PageSize)
	for pos := (offset + size - 1) / size * size; pos < offset+uint64(len(data)); pos += size {
		data[pos-offset] = b.values[pos/size]
	}
	return nil
}
func (b *patternBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.values) == 0 {
		return []control.Extent{{Offset: offset, Length: length, Identity: control.Identity{Zero: true}}}, nil
	}
	var result []control.Extent
	size := uint64(vmmemory.PageSize)
	for pos := offset; pos < offset+length; {
		_, written := b.values[pos/size]
		stop := min(offset+length, (pos/checkpoint.PageSize+1)*checkpoint.PageSize)
		id := control.Identity{Zero: true}
		if written {
			id = control.Identity{Ref: control.Ref{VM: b.vm, Sequence: 2}, Volume: "v", Page: pos / checkpoint.PageSize}
		}
		if n := len(result); n > 0 && result[n-1].Identity == id {
			result[n-1].Length += stop - pos
		} else {
			result = append(result, control.Extent{Offset: pos, Length: stop - pos, Identity: id})
		}
		pos = stop
	}
	return result, nil
}

// checkpoint publishes a region's sealed pages, which is the only way this
// backing's values change. Every page is validated as it arrives: a private
// page must hold exactly the one byte the guest stored.
func (b *patternBacking) checkpoint(ctx context.Context, r *vmmemory.Region) error {
	if err := r.Seal(ctx); err != nil {
		return err
	}
	ckpt := r.Checkpoint()
	page := make([]byte, vmmemory.PageSize)
	var failure error
	for _, number := range ckpt.DirtyPages() {
		if err := ckpt.ReadDirty(ctx, number, page); err != nil {
			failure = err
			break
		}
		for _, value := range page[1:] {
			if value != 0 {
				failure = errors.New("private page changed outside the written byte")
				break
			}
		}
		if failure != nil {
			break
		}
		b.mu.Lock()
		b.values[number] = page[0]
		b.writes++
		b.mu.Unlock()
	}
	b.mu.Lock()
	b.batches++
	b.mu.Unlock()
	return errors.Join(failure, ckpt.Retire(ctx, failure == nil))
}

func nativeVMAs(t *testing.T, p *nativeProcess) int {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", p.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(raw, []byte{'\n'})
}

// Opt in with SPROUTFS_FRAGMENT_MIB=2048: two 2 GiB regions, 2 GiB of
// alternating dirty pages, a 4 MiB arena, and two write/flush epochs.
func TestNativeFragmentedWritebackMeasurements(t *testing.T) {
	raw := os.Getenv("SPROUTFS_FRAGMENT_MIB")
	if raw == "" {
		t.Skip("set SPROUTFS_FRAGMENT_MIB to logical MiB per region")
	}
	mib, err := strconv.Atoi(raw)
	if err != nil || mib < 2 || mib > 2048 {
		t.Fatal("SPROUTFS_FRAGMENT_MIB must be 2..2048")
	}
	size := vmmemory.PageSize
	length := mib << 20
	pages := length / size
	h := kernelHostBudget(t, 2, 2*pages, pages)
	first := &patternBacking{size: uint64(length), vm: "fragment-a", values: make(map[uint64]byte)}
	second := &patternBacking{size: uint64(length), vm: "fragment-b", values: make(map[uint64]byte)}
	a := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: min(4096, 2*pages), CommandTimeout: 30 * time.Second, VerifyInterval: time.Hour}, first, second)
	a.request("read 0 0 1", "data 00")
	if err := a.populate(t.Context()); err != nil {
		t.Fatal(err)
	}
	initialVMAs := nativeVMAs(t, a)
	peakVMAs := initialVMAs
	var epochs []map[string]any
	for epoch, value := range []byte{51, 73} {
		before, _ := h.Stats(t.Context())
		start := time.Now()
		for region := range 2 {
			for offset := 0; offset < length; offset += 16 << 20 {
				count := min(length-offset, 16<<20)
				a.request(fmt.Sprintf("stridefill %d %d %d %d %d", region, offset, count, 2*size, value), "strided")
				current := nativeVMAs(t, a)
				peakVMAs = max(peakVMAs, current)
				t.Logf("epoch=%d region=%d written_mib=%d vmas=%d", epoch+1, region, (offset+count)>>20, current)
			}
		}
		writeNS := time.Since(start).Nanoseconds()
		beforeCheckpoint, _ := h.Stats(t.Context())
		dirtyVMAs := nativeVMAs(t, a)
		start = time.Now()
		// The checkpoint is what moves the dirty set: it seals every region, reads
		// the sealed pages and retires them.
		backings := []*patternBacking{first, second}
		for region := range 2 {
			if err := backings[region].checkpoint(t.Context(), a.region(region)); err != nil {
				t.Fatal(err)
			}
		}
		checkpointNS := time.Since(start).Nanoseconds()
		after, _ := h.Stats(t.Context())
		for _, backing := range []*patternBacking{first, second} {
			backing.mu.Lock()
			if len(backing.values) != pages/2 {
				t.Errorf("only %d dirty pages persisted, want %d", len(backing.values), pages/2)
			}
			for page := 0; page < pages; page += 2 {
				if backing.values[uint64(page)] != value {
					t.Errorf("persisted page %d lost epoch %d", page, epoch)
					break
				}
			}
			backing.mu.Unlock()
		}
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		epochs = append(epochs, map[string]any{"epoch": epoch + 1, "write_ns": writeNS, "checkpoint_ns": checkpointNS, "before": before, "before_checkpoint": beforeCheckpoint, "after_checkpoint": after, "dirty_vmas": dirtyVMAs, "after_checkpoint_vmas": nativeVMAs(t, a), "pager_heap_bytes": mem.HeapAlloc, "vmm_memory": processMemory(t, a.cmd.Process.Pid), "pager_memory": processMemory(t, os.Getpid())})
		// Sample distant native reads after the complete checkpoint, including
		// untouched neighboring zeros; earlier tests cover exhaustive spill/refault
		// bytes.
		for region := range 2 {
			for _, page := range []int{0, (pages / 2) &^ 1, pages - 2} {
				a.request(fmt.Sprintf("read %d %d 1", region, page*size), fmt.Sprintf("data %02x", value))
				a.request(fmt.Sprintf("read %d %d 1", region, (page+1)*size), "data 00")
			}
		}
	}
	result := map[string]any{"logical_bytes": 2 * length, "dirty_bytes_per_epoch": length, "arena_bytes": 2 * size, "initial_vmas": initialVMAs, "peak_sampled_vmas": peakVMAs, "page_size": size, "epochs": epochs}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FRAGMENT_MEASUREMENT %s", data)
}

func TestNativeRepeatedPrivateEvictionKeepsVMAsBounded(t *testing.T) {
	const pages, arenaPages = 128, 32
	size := vmmemory.PageSize
	h := kernelHostBudget(t, arenaPages, 2*pages, pages)
	first := &patternBacking{size: uint64(pages * size), vm: "evict-a", values: make(map[uint64]byte)}
	second := &patternBacking{size: uint64(pages * size), vm: "evict-b", values: make(map[uint64]byte)}
	a := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: 128, MaxVMAs: 8192, CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour}, first, second)
	a.request("read 0 0 1", "data 00")
	if err := a.populate(t.Context()); err != nil {
		t.Fatal(err)
	}
	baseline := nativeVMAs(t, a)
	for _, value := range []int{51, 73} {
		for offset := 0; offset < pages*size; offset += 32 * size {
			a.request(fmt.Sprintf("stridefill 0 %d %d %d %d", offset, 32*size, 2*size, value), "strided")
			if count := nativeVMAs(t, a); count > baseline+2*arenaPages+64 {
				t.Fatalf("private eviction retained %d VMAs above baseline %d with only %d arena pages", count, baseline, arenaPages)
			}
		}
	}
	a.request("read 0 0 1", "data 49")
	a.request(fmt.Sprintf("read 0 %d 1", size), "data 00")
}
