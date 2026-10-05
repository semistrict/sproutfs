package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A region's pages are a mosaic of checkpoints, and a fault maps the resident
// page of whichever checkpoint its page is: here a fault on a page of one
// checkpoint, then one on a page of another that a sibling has read, which
// maps the sibling's page and reads nothing.
func TestAFaultMapsTheResidentPageOfWhicheverCheckpointItsPageIs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 0)
		f := newConfiguredFixture(t, aroundConfig())
		other := control.Ref{VM: vmName(t) + "-other", Sequence: 1}
		sibling, sm, _ := aroundRegion(t, f, other, nil)
		fault(t, f, sibling, 8)
		b := &slowBacking{backing: f.newBacking(aroundPages)}
		b.sources = make(map[uint64]control.Ref)
		for page := uint64(8); page < 16; page++ {
			b.sources[page] = other
		}
		r, m := f.attach(b)
		fault(t, f, r, 0)
		before := hostStats(t, f)
		if err := r.Fault(f.ctx, 8, false); err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); s.Loads != before.Loads {
			t.Fatalf("a fault on a page of the other checkpoint, which a sibling holds, read %d times, want none",
				s.Loads-before.Loads)
		}
		mine, _ := m.mappedPage(8)
		theirs, _ := sm.mappedPage(8)
		if mine.place != theirs.place {
			t.Fatalf("page 8 is mapped at %+v, want the sibling's page at %+v", mine.place, theirs.place)
		}
	})
}

// A fault does not prefetch a page a prefetch is reading already: that read
// is its to bring in. And past the bound on prefetches reading at once, a
// fault prefetches nothing and the run is counted refused.
func TestAPrefetchLeavesThePagesAnotherIsReadingAndTheBoundRefusesTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := aroundConfig()
		cfg.PrefetchRuns = 1
		f := newConfiguredFixture(t, cfg)
		held := make(chan struct{})
		first, _, fb := aroundRegion(t, f, f.source, held)
		if err := first.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		// The first region's prefetch of pages 1 to 7 is reading, and holds
		// the bound. A second region of the same checkpoint faults page 0,
		// resident now, and leaves the rest to that prefetch.
		second, sm, sb := aroundRegion(t, f, f.source, nil)
		if err := second.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		requirePage(t, sm, 0)
		if s := hostStats(t, f); s.Prefetches != 1 || s.PrefetchRefused != 0 {
			t.Fatalf("with the window's rest being read, prefetches %d and refused %d, want 1 and 0",
				s.Prefetches, s.PrefetchRefused)
		}
		// A fault in the next window, which follows its region's fault in
		// this one, finds the bound held and prefetches nothing.
		if err := second.Fault(f.ctx, 8, false); err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); s.Prefetches != 1 || s.PrefetchRefused != 1 {
			t.Fatalf("past the bound, prefetches %d and refused %d, want 1 and 1", s.Prefetches, s.PrefetchRefused)
		}
		if probes := sim.RuntimeFrom(f.ctx).Probes(); probes[vmmemory.ProbePrefetchRefused] != 1 {
			t.Fatalf("probes %v, want one refused prefetch", probes)
		}
		close(held)
		if err := f.h.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if reads := sb.readsOf(true); len(reads) != 0 {
			t.Fatalf("the second region prefetched %v, want nothing", reads)
		}
		if reads := fb.readsOf(true); len(reads) != 1 || reads[0] != (slowRead{first: 1, pages: 7, prefetch: true}) {
			t.Fatalf("the first region prefetched %v, want pages 1 to 7 in one read", reads)
		}
		if err := second.Fault(f.ctx, 5, false); err != nil {
			t.Fatal(err)
		}
		requirePage(t, sm, 5)
	})
}

// A region that read holes tells the pager there are zeros to populate, and
// detaching it takes that back: a region attached after it, with nothing
// resident anywhere, populates nothing and reads no metadata before its guest
// runs.
func TestAnAttachAfterTheRegionOfZerosDetachedReadsNoMetadata(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 4, LogicalPages: 8, DirtyPages: 4,
			ReadAheadPages: 4}, 4)
		access(t, r, m, 0, false)
		clear(m.pages)
		if err := r.Detach(f.ctx); err != nil {
			t.Fatal(err)
		}
		backing := &unavailableLocate{Backing: f.newBacking(4)}
		cold, cm := f.attach(backing)
		if len(cm.pages) != 0 {
			t.Fatalf("an attach with nothing resident mapped %d pages", len(cm.pages))
		}
		backing.ready = true
		if got := access(t, cold, cm, 0, false)[0]; got != 1 {
			t.Fatalf("its first fault read %d, want 1", got)
		}
	})
}
