package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A prefetch can read a page another load is already reading: a fault's own
// read of its page is not in flight for prefetches to see. The load that lands
// first is the page every memory region maps; the prefetch's copy goes back.
// Here one fork's fault reads page 3 alone, a fault at random, while the other
// fork's first fault prefetches the rest of the same run, page 3 among it.
// The fault's read lands first and the prefetch, landing after it, finds page
// 3 resident and drops its copy.
func TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		br, bm := f.attach(f.slowBacking(64))
		ar, am := f.attach(f.slowBacking(64))
		// The other fork's first fault is in the last run, so its fault on
		// page 3 follows none of its faults and reads that page alone.
		if err := br.Fault(f.ctx, 60, false); err != nil {
			t.Fatal(err)
		}
		read := make(chan error, 1)
		go func() { read <- br.Fault(f.ctx, 3, false) }()
		synctest.Wait()
		if err := ar.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
		before := hostStats(t, f)
		if err := ar.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := br.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		s := hostStats(t, f)
		if got := s.PrefetchDropped - before.PrefetchDropped; got != 1 {
			t.Fatalf("the prefetches dropped %d pages, want page 3 alone", got)
		}
		if probes := sim.RuntimeFrom(f.ctx).Probes(); probes[vmmemory.ProbePrefetchDuplicate] != 1 {
			t.Fatalf("probes %v, want one duplicate", probes)
		}
		requirePage(t, bm, 3)
		if got := accessUnder(f.ctx, t, ar, am, 3, false)[0]; got != 4 {
			t.Fatalf("page 3 reads %d, want 4", got)
		}
		bp, _ := bm.mappedPage(3)
		ap, _ := am.mappedPage(3)
		if bp.place != ap.place {
			t.Fatalf("the two forks map page 3 at %+v and %+v, want one copy", bp.place, ap.place)
		}
	})
}
