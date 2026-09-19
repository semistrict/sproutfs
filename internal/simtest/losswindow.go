package simtest

import (
	"context"
	"fmt"
	"testing/synctest"
	"time"
)

// The loss window is what a host loss costs one VM in time, and a simulation is
// where that can be measured: the world knows when every store was made, and it
// knows exactly which of them a recovery took back. What it requires of every
// recovery is the guarantee itself — the writes it rewound span at most the
// window plus one checkpoint attempt.

// Advance moves every host's clock on together. The deployment's clocks are
// separate, because a host keeps its own deadlines, but an age that crosses
// hosts is a comparison between two of them: a destination dates the pages it
// received from the source's own measurement, and a world whose clocks drifted
// would be measuring the drift.
//
// What the advance released has run by the time it returns, which is what lets
// a caller act on the world the advance produced rather than beside it. A
// clock's own Settle waits for the AfterFunc callbacks that advance started and
// for nothing else, and a host's checkpoint loop wakes on a timer rather than
// through one of those: a caller that had only settled would go on with an
// interval checkpoint of its own running beside it, and the next thing it asked
// a guest for would land in the middle of that checkpoint's pause — where a
// guest's vCPUs are stopped and it stores nothing. The quiescent point is what
// says the advance is over, so this is a bubble operation: every scenario that
// moves a world's clocks runs inside testing/synctest.
func (w *World) Advance(d time.Duration) {
	for _, h := range w.hosts {
		h.clock.Advance(d)
	}
	for _, h := range w.hosts {
		h.clock.Settle()
	}
	synctest.Wait()
}

// LoseStore takes object storage away from one host, or gives it back. Every
// other host still has it, which is what makes it a host in the dark rather
// than an outage: its guests go on running out of its pages and nothing it
// holds can be published. It is what a scenario about the loss window needs,
// because the window is about a VM whose checkpoints are not landing.
func (w *World) LoseStore(index int, lost bool) {
	w.hosts[index].objects.setFailed(lost)
}

// StoreInBackground has one VM's guest store into one page of its memory on a
// goroutine of its own, and reports what that store did. A scenario about the
// loss window needs it: the store it is about does not return until a
// checkpoint of its VM lands, so a test that made it inline would hang instead
// of asserting. A VM that is running nowhere stores nothing and reports nil at
// once.
// The store runs under the caller's context rather than the guest's, because
// the guest's outlives the fault: a scenario that could not cancel one would
// have to leave it waiting for a checkpoint that is never coming.
func (w *World) StoreInBackground(ctx context.Context, id string, page uint64) <-chan error {
	done := make(chan error, 1)
	in, g := w.runningVM(id)
	if g == nil {
		done <- nil
		return done
	}
	go func() {
		before := g.stored()
		err := g.storeIn(ctx, MemoryVolume, page)
		w.noteWrites(in, g, before)
		done <- err
	}()
	return done
}

// noteWrites dates the stores one call made, at the moment it ended on the
// clock of the host that took them. The host clocks are the simulation's own
// and stand still between advances, so that moment is exact rather than
// approximate.
func (w *World) noteWrites(in *instance, g *guest, before int64) {
	if in == nil || g == nil {
		return
	}
	made := g.stored() - before
	if made <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.hosts[in.host].clock.Now()
	for range made {
		in.writes = append(in.writes, at)
	}
}

// rewind records the writes a recovery took back: everything this VM and its
// ancestors stored past the checkpoint it came back at. Caller holds the world lock.
func (w *World) rewind(in *instance, at durableState) {
	in.rewound = nil
	if at.writes < 0 || int(at.writes) >= len(in.writes) {
		return
	}
	in.rewound = append(in.rewound, in.writes[at.writes:]...)
}

// lossWindow is the bound this world's hosts keep, and allowance what one
// checkpoint attempt may add to it. A store past the window waits for a seal,
// and the seal comes from the checkpoint the pager asked for, so a VM can hold
// writes for the window plus however long the loop is from taking one — which
// is its interval. A world with no loop has no attempt to allow for.
func (w *World) lossWindow() (window, allowance time.Duration) {
	window = w.config.Knobs.LossWindow
	if interval := w.config.CheckpointInterval; interval > 0 {
		allowance = interval
	}
	return window, allowance
}

// VerifyLossWindow requires that the writes the last recovery of one VM rewound
// span no more than the loss window plus one checkpoint attempt. It is the
// other half of what a recovery has to leave true: VerifyDurable says the VM
// came back at bytes a writer of it published, and this says how much of its
// guest's work that cost.
//
// A VM that has not been recovered, and a world with the bound turned off, have
// nothing to require.
func (w *World) VerifyLossWindow(_ context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	in := w.instances[id]
	if in == nil {
		return nil
	}
	return w.spanHolds(in)
}

// spanHolds is the requirement itself. Caller holds the world lock.
func (w *World) spanHolds(in *instance) error {
	window, allowance := w.lossWindow()
	if window <= 0 || len(in.rewound) == 0 {
		return nil
	}
	span := in.rewound[len(in.rewound)-1].Sub(in.rewound[0])
	if span <= window+allowance {
		return nil
	}
	return fmt.Errorf("%s rewound %d writes spanning %s, and its loss window is %s plus one checkpoint attempt of %s",
		in.spec.ID, len(in.rewound), span, window, allowance)
}
