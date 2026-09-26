package host

import (
	"context"
	"log/slog"

	"github.com/semistrict/sproutfs/checkpoint"
)

// pulling copies every page of the checkpoint a VM marked to pull started from
// onto this host's disk, and holds the copy for as long as this host runs the
// VM. It runs beside the checkpoint loop and ends with it: a stop, a migration
// away, a fence or the VMM's death gives the copy up.
//
// The guest runs while the copy is made, and a fault never waits for it. Once
// it is complete, a fault on a page of that checkpoint which is not resident —
// never loaded, or evicted since — reads this host's disk and makes no request
// of the object store. What the guest wrote since that checkpoint is not in it
// and needs no copy: it is this host's already, in the pager, and a later
// checkpoint's pages go to the store and are read from it when evicted.
//
// A VM whose checkpoint does not fit in what the disk has left, or a host that
// keeps no disk, starts no pull. The VM runs all the same, and its faults read
// the store as any other VM's do.
func (h *Host) pulling(ctx context.Context, vmID string, entry *registration) {
	vm := h.vm(vmID)
	if vm == nil {
		return
	}
	pull, err := vm.Pull(ctx)
	entry.mu.Lock()
	entry.pulled, entry.refused = pull, err
	entry.mu.Unlock()
	if err != nil {
		slog.WarnContext(ctx, "host: a VM marked to pull its memory reads it from object storage instead",
			"vm", vmID, "error", err)
		return
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err == nil {
		slog.InfoContext(ctx, "host: pulled a VM's memory onto this host's disk",
			"vm", vmID, "bytes", pull.Stats().Bytes)
	}
	<-ctx.Done()
}

// Pulled reports how far the pull of a VM this host runs has come, and false
// for a VM that is not marked to pull or that this host does not run. A pull
// this host refused reports Done with the refusal as its Err.
func (h *Host) Pulled(vmID string) (checkpoint.PullStats, bool) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil || !entry.pull {
		return checkpoint.PullStats{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	switch {
	case entry.refused != nil:
		return checkpoint.PullStats{Done: true, Err: entry.refused}, true
	case entry.pulled != nil:
		return entry.pulled.Stats(), true
	}
	return checkpoint.PullStats{}, true
}
