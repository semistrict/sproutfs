package vmmemory_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A capture write-protects only the pages the guest still maps when it holds
// the region's protection. An eviction takes a page's mapping away under that
// page's lock and the protection shared, not the region, so one between the
// capture taking its pages and its protection leaves a page the guest no
// longer maps; a write-protect of it is refused, and the region was terminal.
// The unscheduled soak found it.
func TestACaptureProtectsNoPageAnEvictionUnmappedAfterItTookThePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0xee)
		evicted := false
		vmmemory.SetCaptureSeam(t, func(taken []uint64) {
			if evicted || !slices.Contains(taken, 0) {
				return
			}
			evicted = true
			went, err := vmmemory.EvictPage(f.ctx, r, 0)
			if err != nil || !went {
				t.Errorf("evicting page 0 under the capture = %v, %v; want it gone", went, err)
			}
		})
		c, err := r.Capture(f.ctx, r.Unjournaled())
		if err != nil {
			t.Fatalf("a capture beside an eviction of its page = %v, want it to take the page", err)
		}
		if want := []uint64{blockOf(0, 0)}; !slices.Equal(c.Blocks, want) || c.Data[0] != 0xee {
			t.Fatalf("the capture took blocks %v starting %#x, want %v starting 0xee", c.Blocks, c.Data[0], want)
		}
		f.storeAt(r, m, 0, 1, 0xef)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after a store into the evicted page, want [0]", got)
		}
		writableIsUnjournaled(t, r, m)
	})
}
