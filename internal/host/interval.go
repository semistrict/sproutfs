package host

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// checkpointing checkpoints one VM every interval for as long as this host runs
// it, and again whenever the pager asks for one out of that turn. Each
// checkpoint pauses the vCPUs only for the VMM state capture and the seal; the
// upload runs behind the resumed guest, and the next interval is measured from
// its completion, so a VM whose checkpoint takes longer than the interval is
// checkpointed back to back rather than piling captures up.
//
// A failure is logged and retried. Where the VM is still inside its loss window
// that retry is the next interval, and there is nothing else to do with one: the
// guest is running, its writes are in this host's memory, and the only cost of a
// checkpoint that did not land is that a host loss would rewind the VM further.
// Where the window is already exceeded the guest is paying that cost now — the
// pager is holding its stores back until a checkpoint of it lands — so the next
// attempt comes at an eighth of the interval, doubling to the interval, rather
// than a whole interval later. The exception to both is a handle a later writer
// has fenced: the checkpoint is where a fenced host finds out, because a running
// VM writes nothing else, and the loop closes the machine rather than leaving a
// guest running whose writes can never be published.
func (h *Host) checkpointing(ctx context.Context, vmID string, entry *registration) {
	failures := 0
	for {
		wait, onRequest := h.nextAttempt(entry, failures)
		if !waitForCheckpoint(ctx, entry, h.clock, wait, onRequest) {
			return
		}
		vm := h.vm(vmID)
		if vm == nil {
			// The handle is gone: this host no longer writes for that VM.
			continue
		}
		if vm.Status().Sealed {
			// A fork point holds this VM's pages, and one checkpoint of a region is
			// outstanding at a time. The next interval takes the checkpoint, once the
			// child that was forked from here has the pages it inherited.
			continue
		}
		// An explicit capture, a fork's, holds the VM's publication lock, so the
		// two serialize rather than checkpointing the same guest twice.
		checkpoint, err := Capture(ctx, vm, entry.runtime, h.clock)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			slog.ErrorContext(ctx, "host: the interval checkpoint failed", "vm", vmID, "error", err)
			if errors.Is(err, volume.ErrNeedsRecovery) {
				h.fenced(ctx, vmID, entry, err)
				return
			}
			continue
		}
		// The wait is the host's, not this loop's. A checkpoint in flight owns the
		// guest's sealed regions until it lands, and end() — which a migration
		// and a removal both stop this loop through — has to return with those
		// regions back in the guest's hands: a handoff that found one still
		// sealed would have to give the migration up and resume the guest. Only
		// the host closing cuts the wait short, and a host that is closing is
		// migrating nothing.
		if err := checkpoint.Wait(h.ctx); err != nil {
			if h.ctx.Err() != nil {
				return
			}
			failures++
			slog.ErrorContext(ctx, "host: publishing the interval checkpoint failed",
				"vm", vmID, "checkpoint", checkpoint.Ref().String(), "error", err)
			// A later writer holds the control record: nothing this handle
			// carries can ever be published, so there is no checkpoint left to
			// take.
			if errors.Is(err, volume.ErrNeedsRecovery) {
				h.fenced(ctx, vmID, entry, err)
				return
			}
		} else {
			failures = 0
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// nextAttempt is how long the loop waits for its next turn at one VM, and
// whether a request out of turn may cut that wait short: the jittered interval,
// which one may, or the backoff of a VM whose last attempt failed and whose
// stores the loss window is already holding back, which one may not. The window
// is read where the wait is chosen, because that is where the choice matters —
// a VM that crossed it while the last publication was in flight is one to come
// back to at once.
func (h *Host) nextAttempt(entry *registration, failures int) (time.Duration, bool) {
	if failures == 0 || !h.overLossWindow(entry) {
		return jittered(h.entropy, h.checkpointInterval), true
	}
	return backoff(h.checkpointInterval, failures), false
}

// waitForCheckpoint waits for this VM's next checkpoint and reports whether one
// is due. That is the jittered interval, or — while onRequest — the pager asking
// for one before it: a guest that fills the host's dirty budget between
// intervals is stalled until a checkpoint releases the reservations its pages
// hold, so the checkpoint it waits for is taken out of turn rather than on the
// clock.
//
// A backoff is the one wait a request may not cut short. It is the wait after a
// publication that failed while the VM was already past its loss window, and the
// store held back by that window asks again every time this loop signals —
// while a capture that cannot be published gives it nothing. Answering each of
// those asks would spin a host that cannot reach the store. The request stays in
// the channel and is answered by the attempt the backoff schedules.
func waitForCheckpoint(ctx context.Context, entry *registration, clock platform.Clock,
	interval time.Duration, onRequest bool) bool {
	timer := clock.NewTimer(interval)
	defer timer.Stop()
	requested := entry.now
	if !onRequest {
		requested = nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-requested:
		return true
	case <-timer.C():
		return true
	}
}

// jittered spreads one wait uniformly within an eighth of the interval either
// side of it, so the VMs a host runs do not checkpoint in lockstep once their
// loops have started together. It is symmetric because only shortening moves
// every VM's mean wait below the interval it was configured with, which makes
// a deployment checkpoint more often than it was asked to; the spread is what
// breaks the lockstep, and it does that either way.
//
// An interval too short to divide has no jitter, which is the whole of what a
// test driving ten-millisecond intervals needs; dividing it anyway would ask
// for a random number below one.
//
// The draw comes from the host's entropy rather than from math/rand, because a
// simulation that cannot reproduce the spread cannot reproduce which VM
// checkpointed first — and which VM checkpointed first is what decides whose
// publication a fencing takeover lands in the middle of.
func jittered(entropy platform.Entropy, interval time.Duration) time.Duration {
	spread := interval / 8
	if spread <= 0 {
		return interval
	}
	return interval - spread + time.Duration(entropy.Uint64()%uint64(2*spread))
}

// checkpointNow answers the pager's request for an immediate checkpoint of one
// region: the VM that maps it is checkpointed out of the interval's turn, which
// is what releases the dirty reservations a stalled store is waiting for. It
// reports whether that checkpoint will be taken — a region belonging to a VM
// this host does not run, whose loop is off, or whose volume a fork hold has
// sealed gets none, and the pager offers another region instead.
//
// It runs on the goroutine of the store that is waiting, so it only signals:
// the capture stays the loop's, as it is on the interval.
func (h *Host) checkpointNow(region *vmmemory.Region) bool {
	vmID, entry := h.machineFor(region)
	if entry == nil || entry.now == nil {
		return false
	}
	vm := h.vm(vmID)
	if vm == nil || vm.Status().Sealed {
		return false
	}
	select {
	case entry.now <- struct{}{}:
	default:
		// The loop already owes this VM a checkpoint.
	}
	return true
}

// stopStalled stops a VM whose stores the pager's dirty budget can no longer
// admit and no checkpoint can relieve. It is the deliberate end of a guest that
// would otherwise die of a failed fault with nothing recorded: the loop stops,
// the fork points taken on the VM are retired, a last checkpoint takes whatever the
// VMM can still be paused for, and then the VM is given up exactly as a fenced
// or a dead one is.
//
// Dropping the registration and claiming the close come first, both here: a
// stall, a takeover and the watcher finding the same process dead can arrive
// together, and the VM must be closed — and the supervisor told — once between
// them.
//
// The pager calls it on the goroutine of the store that stalled, which must not
// wait for any of that, so the stop runs on one of this host's own.
func (h *Host) stopStalled(region *vmmemory.Region, cause error) {
	vmID, entry := h.machineFor(region)
	if entry == nil {
		slog.ErrorContext(h.ctx, "host: a stalled region belongs to no VM this host runs",
			"error", cause)
		return
	}
	if !h.forget(vmID, entry) || !h.claimFence(vmID) {
		return
	}
	go h.stopped(vmID, entry, cause)
}
