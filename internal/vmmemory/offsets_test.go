package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// An offset is an address and a page is memory. An arena may have far more of
// the first than of the second — it is a sparse file, so an offset costs
// nothing until a page is put there — and what bounds a pager is the pages: the
// memory it holds, what it evicts at, and what it reports. These are the
// numbers that must not move when the address space grows.

// A pager whose arena has sixty-four times the addresses of its pages holds
// exactly the pages its budget allows, evicts at the same point, and reports
// the same residency as one whose addresses and pages are one number.
func TestAnArenaWithMoreOffsetsThanPagesHoldsTheSamePages(t *testing.T) {
	for _, offsets := range []int{4, 256} {
		synctest.Test(t, func(t *testing.T) {
			const pages = 4
			f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: pages, ArenaOffsets: offsets,
				LogicalPages: 16, DirtyPages: pages, ReadAheadPages: 1})
			r, m, b := f.memoryRegion(8)
			for page := range uint64(8) {
				access(t, r, m, page, false)
			}
			s := hostStats(t, f)
			if s.ResidentPages != pages || s.PeakResidentPages != pages {
				t.Fatalf("offsets=%d: resident %d, peak %d; want %d and %d",
					offsets, s.ResidentPages, s.ResidentPages, pages, pages)
			}
			if f.a.held != pages || f.a.peak != pages {
				t.Fatalf("offsets=%d: the arena holds %d pages and held %d at its peak, want %d of each",
					offsets, f.a.held, f.a.peak, pages)
			}
			// Eight pages read through an arena of four: every page after the
			// first four evicted one, and the volume was read once per page.
			if s.Evictions != 4 || b.loads != 8 {
				t.Fatalf("offsets=%d: %d evictions and %d volume reads, want 4 and 8",
					offsets, s.Evictions, b.loads)
			}
			// Nothing was put at an address the budget does not cover: the arena
			// refuses an offset it does not have, and the pager never asked for
			// one it could not hold.
			if len(f.a.slots) != pages {
				t.Fatalf("offsets=%d: the arena has pages at %d addresses, want %d",
					offsets, len(f.a.slots), pages)
			}
		})
	}
}

// A page released gives its memory back: the arena punches the offset, so what
// it holds falls while the address it was at stays exactly where it was. The
// retire of an untouched write-ahead page is the path that does it — the volume
// holds no object for the page, so the pager takes it away — and here it is in
// the arena's own count.
func TestReleasingAPageGivesTheArenasMemoryBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages, run = 8, 4
		f, r, m, b := zeroAheadMemoryRegion(t, pages, run)
		access(t, r, m, 0, true)[0] = 42
		if f.a.held != run || f.a.peak != run {
			t.Fatalf("the arena holds %d pages after one store and held %d at its peak, want %d of each",
				f.a.held, f.a.peak, run)
		}
		f.mustCheckpoint(r, b)
		// One page was stored into and published; the other three of the run
		// read back as zeros, so the volume holds no object for them and the
		// retire released each one.
		if f.a.held != 1 || len(f.a.slots) != 1 {
			t.Fatalf("the arena holds %d pages at %d addresses after the checkpoint, want 1 of each",
				f.a.held, len(f.a.slots))
		}
		if s := hostStats(t, f); s.ResidentPages != 1 || s.PeakResidentPages != run {
			t.Fatalf("the pager reports %d resident pages and a peak of %d, want 1 and %d",
				s.ResidentPages, s.PeakResidentPages, run)
		}
	})
}

// A pager cannot be built with fewer addresses than pages: a page has to have
// somewhere to be.
func TestFewerOffsetsThanPagesIsRefused(t *testing.T) {
	if _, err := newBrokenFixture(t, vmmemory.Config{PageSize: uint64(pageSize),
		ResidentPages: 8, ArenaOffsets: 4, LogicalPages: 16, DirtyPages: 4}); err == nil {
		t.Fatal("a pager with four addresses for eight pages was built")
	}
}
