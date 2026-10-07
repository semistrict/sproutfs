package host

import (
	"testing"

	"github.com/semistrict/sproutfs/platform/sim"
)

// The covered position is the highest position of the captures that began
// before the pause ended, whether the journal had placed them by then or
// places them after, and never one of a capture that began after.
func TestTheCoveredPositionIsTheLastCaptureBeforeThePause(t *testing.T) {
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	for _, placedFirst := range []bool{true, false} {
		s := &vmJournal{}
		s.captures++
		if placedFirst {
			s.placedOne([]uint64{100, 140})
		}
		c := &cover{state: s, resolved: make(chan struct{})}
		c.paused(ctx)
		s.captures++
		if !placedFirst {
			s.placedOne([]uint64{100, 140})
		}
		s.placedOne([]uint64{200})
		if got, err := c.position(ctx); err != nil || got != 140 {
			t.Fatalf("placed before the pause %v: the cover is at %d (%v), want 140", placedFirst, got, err)
		}
	}
}
