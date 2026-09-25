package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A store gives its memory region up while it takes a slot for its copy, and
// the memory region can fail meanwhile: another of its faults had a mapping
// command fail, as a session that has ended does. The store then ends there,
// and the slot it took is given back. Nothing else holds it, so nothing else
// ever would, and the pager would count a page that nothing holds for as long
// as it ran.
func TestAStoreWhoseMemoryRegionFailsWhileItReclaimsGivesItsSlotBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 16, 4)
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, false)
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		vmmemory.SetReclaimSeam(t, func(uint64) {
			m.failMap = true
			if err := r.Fault(t.Context(), 2, false); err == nil {
				t.Error("a fault whose mapping command failed succeeded")
			}
		})
		if err := r.Fault(t.Context(), 0, true); err == nil {
			t.Fatal("a store whose memory region failed while it reclaimed succeeded")
		}
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.ResidentPages != before.ResidentPages+1 || after.DirtyPages != before.DirtyPages {
			t.Fatalf("after the failed store the pager holds %d resident and %d dirty pages, want %d and %d: "+
				"the page it held and the page the failed fault loaded",
				after.ResidentPages, after.DirtyPages, before.ResidentPages+1, before.DirtyPages)
		}
	})
}

// A store fills its copy and then takes the page it copies from, which it can
// wait for. The session it serves can end in that wait. The copy is then
// reachable from no binding, so it is given back there or never.
func TestAStoreCancelledAfterItsCopyGivesTheCopyBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 16, 4)
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, false)
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.a.onWrite = func(int) { cancel() }
		if err := r.Fault(ctx, 0, true); !errors.Is(err, context.Canceled) {
			t.Fatalf("a store cancelled after its copy was filled = %v, want the cancellation", err)
		}
		f.a.onWrite = nil
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.ResidentPages != before.ResidentPages || after.DirtyPages != before.DirtyPages {
			t.Fatalf("after the cancelled store the pager holds %d resident and %d dirty pages, want the %d and %d it held before",
				after.ResidentPages, after.DirtyPages, before.ResidentPages, before.DirtyPages)
		}
	})
}
