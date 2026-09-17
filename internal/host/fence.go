package host

import (
	"context"
	"errors"
	"log/slog"

	"github.com/semistrict/sproutfs/internal/volume"
)

// watching re-reads the control record of every VM this host holds, on the
// host's epoch interval. A checkpoint is not where a host can be relied on to
// learn it has been taken over: a VM is up to one interval from its next
// checkpoint, and a VM a fork point has sealed is never checkpointed at all, so
// a host fenced by a recovery would otherwise go on running a guest — and
// serving its pages — for a whole interval or for good.
//
// A record that cannot be read changes nothing. Only the record itself, naming
// an epoch this host does not hold, is evidence of a takeover.
//
// The reads go out a few at a time rather than one after another: a host runs
// tens of VMs, and a round of one small read each, serialised, takes as long as
// the slowest of them times their number — which on a store having a bad minute
// is longer than the interval itself, so the check falls behind exactly when a
// takeover is most likely. The concurrency is small because this is background
// work competing with the checkpoints for the same store.
func (h *Host) watching(ctx context.Context) {
	for {
		if err := h.clock.Sleep(ctx, jittered(h.entropy, h.epochInterval)); err != nil {
			return
		}
		held := h.volumes.VMs()
		ids := make([]string, 0, len(held))
		byID := make(map[string]*volume.VM, len(held))
		for _, vm := range held {
			ids = append(ids, vm.ID())
			byID[vm.ID()] = vm
		}
		failures := eachVM(ctx, ids, epochConcurrency, func(ctx context.Context, vmID string) error {
			return byID[vmID].Confirm(ctx)
		})
		// The takeovers are acted on here rather than in the fan-out: giving a
		// VM up waits for its checkpoint loop, and these goroutines are what a
		// round of reads is bounded by.
		for index, err := range failures {
			if err == nil || ctx.Err() != nil {
				continue
			}
			switch {
			case errors.Is(err, volume.ErrNeedsRecovery):
				h.fence(ctx, ids[index], err)
			case errors.Is(err, volume.ErrClosed), errors.Is(err, volume.ErrHandedOff):
			default:
				slog.WarnContext(ctx, "host: reading a VM's control record failed",
					"vm", ids[index], "error", err)
			}
		}
	}
}

// epochConcurrency is how many control records one round of the epoch watch
// reads at once.
const epochConcurrency = 8

// fence closes a VM the epoch timer found this host fenced out of. The VM's
// checkpoint loop is stopped first — it may have a capture in flight that still
// holds the guest's regions sealed — and then everything the host holds of that
// VM is given up, with the record's own refusal as the cause.
func (h *Host) fence(ctx context.Context, vmID string, cause error) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	delete(h.machines.running, vmID)
	h.machines.mu.Unlock()
	if entry != nil {
		entry.end()
	}
	if !h.claimFence(vmID) {
		return
	}
	h.discard(ctx, vmID, entry, fencedMessage, cause)
}

// fenced is the same close from the checkpoint loop, which found the takeover in
// a publication the control record refused. It runs on that loop's own
// goroutine, which is why it removes its entry itself rather than stopping the
// loop: end() would wait for this.
func (h *Host) fenced(ctx context.Context, vmID string, entry *registration, cause error) {
	h.forget(vmID, entry)
	if !h.claimFence(vmID) {
		return
	}
	h.discard(ctx, vmID, entry, fencedMessage, cause)
}

// claimFence admits one closer of a VM this host has lost. The checkpoint loop
// and the epoch timer can both find one takeover, and the watcher on the VMM
// can find the process dead while they do, and the VM must be closed, and the
// supervisor told, exactly once.
func (h *Host) claimFence(vmID string) bool {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if h.machines.fenced[vmID] {
		return false
	}
	h.machines.fenced[vmID] = true
	return true
}

// fencedMessage is what a VM another writer has taken the control record of is
// given up as. Its guest may still be running and its VMM still hold frames, but
// nothing either of them produces can ever be published, so holding on would
// burn this host's memory on writes with nowhere to go — and serving any of it
// would hand another VM state this one's writer never had.
const fencedMessage = "host: closed a VM a later writer fenced this host out of"

// stalledMessage accounts for a VM stopped deliberately because the pager's
// dirty budget could no longer admit its stores and no checkpoint could relieve
// it. It is the end of a guest that would otherwise have died of a failed fault
// with nothing recorded and nothing said.
const stalledMessage = "host: stopped a VM whose stores the dirty budget could not admit"

// stoppedMigrationMessage accounts for a VM whose migration stopped the guest
// and then failed: the handoff never happened, and neither did the resume.
const stoppedMigrationMessage = "host: closed a VM whose migration stopped it and could not hand it over"
