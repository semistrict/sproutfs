package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
)

// ErrNotPulling reports a VM this host does not run, or runs without the mark
// that pulls its whole memory.
var ErrNotPulling = errors.New("host: the VM is not pulling its memory here")

// pulling copies every page of the checkpoint a VM marked to pull started from
// onto this host's disk. It runs beside the checkpoint loop and ends with it: a
// stop, a migration away, a fence or the VMM's death ends the copying. What
// the VM publishes is kept on the disk until its handle closes, so the last
// checkpoint of a stop or of the host's shutdown is kept too. The copy stays
// on the disk, which gives it back only when it needs the space.
//
// The guest runs while the copy is made, and a fault never waits for it. Once
// it is complete, a fault on a page of that checkpoint which is not resident —
// never loaded, or evicted since — reads this host's disk and makes no request
// of the object store, while the disk holds the page. What the guest wrote
// since that checkpoint is not in it and needs no copy: it is this host's
// already, in the pager, and a later checkpoint keeps its pages on the same
// disk as it uploads them, so they are read from there too once evicted
// (volume.VM.Pull).
//
// A VM whose checkpoint is larger than the disk can hold, or a host that keeps
// no disk, starts no pull. The VM runs all the same, and its faults read
// the store as any other VM's do.
func (h *Host) pulling(ctx context.Context, vmID string, entry *registration) {
	entry.mu.Lock()
	fetched := entry.fetched
	entry.mu.Unlock()
	vm := h.vm(vmID)
	if vm == nil {
		close(fetched)
		return
	}
	// A fork's child publishes its root right after it starts here, and that
	// root is the checkpoint it runs on from then: the parent's pages and the
	// ones the parent held that no checkpoint had, republished as the child's.
	// Pulling the parent's alone would leave the second kind to the store.
	select {
	case <-vm.Rooted():
	case <-ctx.Done():
		close(fetched)
		return
	}
	pull, err := vm.Pull(ctx)
	entry.mu.Lock()
	entry.pulled, entry.refused = pull, err
	entry.mu.Unlock()
	if err != nil {
		close(fetched)
		slog.WarnContext(ctx, "host: a VM marked to pull its memory reads it from object storage instead",
			"vm", vmID, "error", err)
		return
	}
	// The fetching ends with the machine, and the keeping with the VM's
	// handle: a stop ends the machine before it publishes the VM's last
	// checkpoint, and that checkpoint is kept on the disk like every other.
	defer func() {
		if sim.Bug(ctx, "host-pull-closed-with-the-machine") {
			pull.Close()
			return
		}
		pull.StopFetching()
	}()
	err = pull.Wait(ctx)
	close(fetched)
	if err == nil {
		slog.InfoContext(ctx, "host: pulled a VM's memory onto this host's disk",
			"vm", vmID, "bytes", pull.Stats().Bytes)
	}
	<-ctx.Done()
}

// Pulled reports how far the pull of a VM this host runs has come, and false
// for a VM that is not marked to pull or that this host does not run. A pull
// this host refused reports Done with the refusal as its Err.
func (h *Host) Pulled(vmID string) (checkpoint.PullStats, bool) {
	entry := h.pulls(vmID)
	if entry == nil {
		return checkpoint.PullStats{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return pullStats(entry), true
}

// WaitPulled returns once the pull of a VM this host runs has stopped fetching:
// complete, stopped short, or refused, which the stats' Err says.
func (h *Host) WaitPulled(ctx context.Context, vmID string) (checkpoint.PullStats, error) {
	entry := h.pulls(vmID)
	if entry == nil {
		return checkpoint.PullStats{}, fmt.Errorf("%w: %s", ErrNotPulling, vmID)
	}
	entry.mu.Lock()
	fetched := entry.fetched
	entry.mu.Unlock()
	select {
	case <-fetched:
	case <-ctx.Done():
		return checkpoint.PullStats{}, context.Cause(ctx)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return pullStats(entry), nil
}

// pulls is the registration of a VM this host runs marked to pull, nil for any
// other.
func (h *Host) pulls(vmID string) *registration {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if entry := h.machines.running[vmID]; entry != nil && entry.terms.Pull {
		return entry
	}
	return nil
}

// pullStats is how far one registration's pull has come. The caller holds its
// mu.
func pullStats(entry *registration) checkpoint.PullStats {
	switch {
	case entry.refused != nil:
		return checkpoint.PullStats{Done: true, Err: entry.refused}
	case entry.pulled != nil:
		return entry.pulled.Stats()
	}
	return checkpoint.PullStats{}
}
