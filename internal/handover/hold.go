package handover

import (
	"context"
	"errors"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// Hold is how long a handoff's source keeps the pages no checkpoint has, as
// whoever carries the handoff can count it.
type Hold struct{ ends time.Time }

// Held is the hold a source reported with its handoff, counted from now: the
// moment the handoff arrived. The source armed its deadline before it
// answered, so its promise ends no later than this hold does. A hold of zero is
// a source that promised nothing.
func Held(now time.Time, hold time.Duration) Hold {
	if hold <= 0 {
		return Hold{}
	}
	return Hold{ends: now.Add(hold)}
}

// Over reports whether the hold has ended by now. A source that promised
// nothing never is.
func (h Hold) Over(now time.Time) bool { return !h.ends.IsZero() && !now.Before(h.ends) }

// Ends is when the hold is over, and false for a source that promised nothing.
func (h Hold) Ends() (time.Time, bool) { return h.ends, !h.ends.IsZero() }

// Look is what one look at a handoff's source found.
type Look struct {
	// Listed is a source the deployment still has: a pod the Kubernetes API
	// lists.
	Listed bool
	// Answered is a source that answered this look, and Holding one that said
	// it still runs the VM or serves its pages.
	Answered, Holding bool
}

var (
	// ErrUnlisted is a source the deployment no longer lists.
	ErrUnlisted = errors.New("the deployment no longer lists it")
	// ErrLetGo is a source that answered and holds nothing of the VM: it let
	// the pages go, or came back without them.
	ErrLetGo = errors.New("it answers and no longer holds them")
	// ErrHoldOver is a source whose hold is over.
	ErrHoldOver = errors.New("its hold is over")
)

// Gone reports what shows that a handoff's source no longer has the pages no
// checkpoint holds, and nil while nothing does. It is the one rule that ends a
// handover for want of its source, for a receive in flight and for one about
// to be tried again.
//
// Those pages exist nowhere else, and ending the handover tears its
// destination's guest down, so silence never counts: a source that did not
// answer may be serving the pages right now. Three things do count. The
// deployment no longer lists the source. The source answered and neither runs
// the VM nor serves its pages. Or its hold is over: the source promised to hold
// the pages that long and no longer, so from then on they are gone whether or
// not anything can reach it. That last one is what ends a handover whose
// source is listed and unreachable.
func (h Hold) Gone(ctx context.Context, look Look, now time.Time) error {
	switch {
	case !look.Listed:
		return ErrUnlisted
	case look.Answered && !look.Holding:
		return ErrLetGo
	case h.Over(now) && !sim.Bug(ctx, "migration-ignore-source-hold"):
		return ErrHoldOver
	}
	return nil
}
