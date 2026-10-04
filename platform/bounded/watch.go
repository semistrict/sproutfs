package bounded

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// bound names which of the two bounds an attempt waits under.
type bound int

const (
	firstByte bound = iota
	stall
)

func (b bound) String() string {
	if b == stall {
		return "stall"
	}
	return "first_byte"
}

// errExpired is the cause a watch cancels its attempt with. It never leaves
// the package: a caller sees the attempt made again, or ErrTimedOut.
var errExpired = errors.New("bounded: the attempt outlived its bound")

// watch is one attempt's deadline. While the attempt waits on the store, it
// cancels the attempt once the store has said nothing for longer than the
// bound the attempt waits under; while the caller holds a body without
// reading it, nothing is waited on and nothing is cancelled.
//
// It keeps at most one timer. Progress only moves the moment the wait is
// measured from, and a timer that fires early for that reason arms itself
// again for what is left.
type watch struct {
	clock  platform.Clock
	bounds Bounds
	cancel context.CancelCauseFunc
	// off is BugUnbounded: the watch never arms.
	off bool

	mu sync.Mutex
	// waiting says the attempt waits on the store, under which bound and
	// since when.
	waiting bool
	under   bound
	since   time.Time
	// timer is the pending timer and due when it fires, and armed counts
	// the timers armed, so a timer stopped too late to keep it from running
	// knows it is not the pending one.
	timer platform.Stopper
	due   time.Time
	armed uint64
	// fired says the attempt was cancelled, under which bound; stopped says
	// the attempt ended and nothing more is to be cancelled.
	fired   bool
	which   bound
	stopped bool
}

func (w *watch) limit(under bound) time.Duration {
	if under == stall {
		return w.bounds.Stall
	}
	return w.bounds.FirstByte
}

// wait starts a wait on the store under one bound.
func (w *watch) wait(under bound) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.off || w.stopped || w.fired {
		return
	}
	w.waiting, w.under, w.since = true, under, w.clock.Now()
	w.deadline()
}

// deadline has the timer fire by the moment the wait, measured from now,
// outlives its bound: a timer due later is armed again, and one due sooner
// checks then and arms itself again for what is left. Caller holds the lock
// and has just set since.
func (w *watch) deadline() {
	limit := w.limit(w.under)
	if w.timer != nil && !w.since.Add(limit).Before(w.due) {
		return
	}
	w.disarm()
	w.arm(limit)
}

// arm sets the timer to check the wait after d. Caller holds the lock.
func (w *watch) arm(d time.Duration) {
	w.armed++
	armed := w.armed
	w.due = w.clock.Now().Add(d)
	w.timer = w.clock.AfterFunc(d, func() { w.check(armed) })
}

// progress is the store having taken or given a byte: the wait goes on, now
// under the bound named, measured from now.
func (w *watch) progress(under bound) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.waiting {
		return
	}
	w.under, w.since = under, w.clock.Now()
	w.deadline()
}

// idle ends a wait: the caller holds what the store gave it.
func (w *watch) idle() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting = false
	w.disarm()
}

// stop ends the attempt's watch for good.
func (w *watch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped, w.waiting = true, false
	w.disarm()
}

// disarm stops the pending timer. Caller holds the lock.
func (w *watch) disarm() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// check runs when the timer armed as the armed'th fires: it cancels an
// attempt that has waited out its bound, and arms the timer again for one
// that made progress since.
func (w *watch) check(armed uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if armed != w.armed || w.timer == nil {
		return
	}
	w.timer = nil
	if !w.waiting || w.stopped || w.fired {
		return
	}
	limit := w.limit(w.under)
	if waited := w.clock.Since(w.since); waited < limit {
		w.arm(limit - waited)
		return
	}
	w.fired, w.which = true, w.under
	w.cancel(errExpired)
}

// expired reports whether the watch cancelled its attempt, and under which
// bound.
func (w *watch) expired() (bound, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.which, w.fired
}
