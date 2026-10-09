package vmmemory_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// readAheadConfig is a pager of 4 KiB pages in 64-page read-ahead windows with room for
// every page its tests read.
func readAheadConfig() vmmemory.Config {
	return vmmemory.Config{PageSize: checkpoint.PageSize4KiB, ResidentPages: 256, LogicalPages: 256, DirtyPages: 16,
		ReadAheadPages: 64, PrefetchRuns: 16}
}

// faultSettled faults page in where it is not mapped, and waits for the
// prefetch the fault started, so the next fault finds what it read mapped.
func faultSettled(t *testing.T, f *fixture, r *vmmemory.MemoryRegion, m *mapping, page uint64) {
	t.Helper()
	if _, ok := m.mappedPage(page); !ok {
		if err := r.Fault(f.ctx, page, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SettlePrefetches(f.ctx); err != nil {
		t.Fatal(err)
	}
	requirePage(t, m, page)
}

// A 4 KiB pager sees a guest's read of 8 KiB as two faults, the second on the
// page after the first. That is no guest reading forwards: each read reads
// its own two pages and nothing ahead of them (readahead.go). A read ahead of
// a stream's second fault (pager-read-ahead-on-a-second-fault) prefetches
// behind every such read.
func TestAGuestReadingTwoPagesAtRandomReadsNothingAhead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, readAheadConfig())
		b := f.slowBacking(256)
		r, m := f.attach(b)
		// The memory region's first fault reads its window whole.
		faultSettled(t, f, r, m, 255)
		for _, page := range []uint64{70, 140, 20, 100, 170} {
			faultSettled(t, f, r, m, page)
			faultSettled(t, f, r, m, page+1)
		}
		if reads := b.readsOf(true); !slices.Equal(reads, []slowRead{{192, 63, true}}) {
			t.Fatalf("the prefetches read %v, want the first fault's window alone", reads)
		}
		want := []slowRead{{20, 1, false}, {21, 1, false}, {70, 1, false}, {71, 1, false}, {100, 1, false},
			{101, 1, false}, {140, 1, false}, {141, 1, false}, {170, 1, false}, {171, 1, false}, {255, 1, false}}
		if reads := b.readsOf(false); !slices.Equal(reads, want) {
			t.Fatalf("the faults read %v, want each page alone", reads)
		}
	})
}

// A guest reading forwards earns its read-ahead: its first two faults read
// their pages alone, its third prefetches four pages, and each fault after
// that four times as many as the one before, until one reads its whole window
// (readahead.go). A stream that earns its window at once
// (pager-read-ahead-a-window-at-once) prefetches it from its third fault.
func TestAGuestReadingForwardsReadsFurtherAheadEachFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, readAheadConfig())
		b := f.slowBacking(256)
		r, m := f.attach(b)
		faultSettled(t, f, r, m, 255)
		for page := range uint64(64) {
			faultSettled(t, f, r, m, page)
		}
		if reads := b.readsOf(false); !slices.Equal(reads, []slowRead{{0, 1, false}, {1, 1, false}, {2, 1, false},
			{7, 1, false}, {24, 1, false}, {255, 1, false}}) {
			t.Fatalf("the faults read %v, want pages 0, 1, 2, 7 and 24", reads)
		}
		if reads := b.readsOf(true); !slices.Equal(reads, []slowRead{{3, 4, true}, {8, 16, true}, {25, 39, true},
			{192, 63, true}}) {
			t.Fatalf("the prefetches read %v, want 4 pages after page 2, 16 after 7, and the rest of the window "+
				"after 24", reads)
		}
	})
}

// A guest's threads reading at random start a stream a fault, and push none
// out that has gone on, which a thread reading forwards has: the stream goes
// on growing between them (readahead.go). Where every new stream took the
// place of the one used least recently (pager-random-fault-replaces-a-stream),
// eight faults at random between two of a stream's would end it. A stream of
// one fault is a fault at random as far as anything can tell.
func TestFaultsAtRandomLeaveAStreamItsReadAhead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, readAheadConfig())
		b := f.slowBacking(256)
		r, m := f.attach(b)
		faultSettled(t, f, r, m, 255)
		faultSettled(t, f, r, m, 0)
		random := uint64(64)
		for _, page := range []uint64{1, 2, 7} {
			faultSettled(t, f, r, m, page)
			for range 10 {
				faultSettled(t, f, r, m, random)
				random += 3
			}
		}
		if reads := b.readsOf(true); !slices.Equal(reads, []slowRead{{3, 4, true}, {8, 16, true},
			{192, 63, true}}) {
			t.Fatalf("the prefetches read %v, want 4 pages after page 2 and 16 after page 7", reads)
		}
	})
}
