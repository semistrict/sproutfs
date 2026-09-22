package slots_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory/internal/slots"
)

// flat is a space whose offsets and pages are one number, which is what a pager
// whose private pages need no placement of their own runs: every offset it has
// may hold a page.
func flat(n int) *slots.Space { return slots.New(n, n) }

// A new space holds every offset free, and the lowest of them is the first.
func TestNewSpaceIsEntirelyFree(t *testing.T) {
	s := flat(100)
	if s.Offsets() != 100 || s.Pages() != 100 || s.Held() != 0 || s.Free() != 100 {
		t.Fatalf("a new space of 100 has %d offsets, %d pages, %d held, %d free",
			s.Offsets(), s.Pages(), s.Held(), s.Free())
	}
	for slot := range 100 {
		if !s.IsFree(slot) {
			t.Fatalf("offset %d of a new space is not free", slot)
		}
	}
	if s.First() != 0 {
		t.Fatalf("the first free offset of a new space is %d, want 0", s.First())
	}
}

// Taking and returning offsets moves exactly those offsets, and a returned one
// becomes the first again however late it is returned.
func TestTakeAndPutMoveExactlyTheirSlots(t *testing.T) {
	s := flat(10)
	s.Take(0, 3)
	if s.Free() != 7 || s.Held() != 3 {
		t.Fatalf("after taking three of ten, %d are free and %d held, want 7 and 3", s.Free(), s.Held())
	}
	for slot := range 3 {
		if s.IsFree(slot) {
			t.Fatalf("offset %d is free after being taken", slot)
		}
	}
	if !s.IsFree(3) {
		t.Fatal("offset 3 was taken along with the run below it")
	}
	if s.First() != 3 {
		t.Fatalf("the first free offset is %d, want 3", s.First())
	}
	s.Put(1)
	if s.Free() != 8 || !s.IsFree(1) {
		t.Fatalf("after returning offset 1, %d are free and its bit is %v", s.Free(), s.IsFree(1))
	}
	if s.First() != 1 {
		t.Fatalf("the first free offset is %d, want the returned 1", s.First())
	}
}

// An exhausted space has no first offset and no run.
func TestExhaustedSpaceOffersNothing(t *testing.T) {
	s := flat(64)
	s.Take(0, 64)
	if s.Free() != 0 {
		t.Fatalf("%d offsets are free after taking every one", s.Free())
	}
	if s.First() != -1 {
		t.Fatalf("the first free offset of an exhausted space is %d, want -1", s.First())
	}
	start, length := s.LongestRun(1)
	if start != -1 || length != 0 {
		t.Fatalf("LongestRun on an exhausted space = %d, %d, want -1 and 0", start, length)
	}
}

// A run stops at want, so a caller asking for four pages is not charged a scan
// of the whole arena, and it never crosses a taken offset.
func TestLongestRunStopsAtWantAndAtTakenSlots(t *testing.T) {
	s := flat(200)
	start, length := s.LongestRun(4)
	if start != 0 || length != 4 {
		t.Fatalf("LongestRun(4) on a free space = %d, %d, want 0 and 4", start, length)
	}
	s.Take(0, 2)
	start, length = s.LongestRun(3)
	if start != 2 || length != 3 {
		t.Fatalf("LongestRun(3) above a taken pair = %d, %d, want 2 and 3", start, length)
	}
	s.Take(2, 198)
	s.Put(5)
	s.Put(7)
	start, length = s.LongestRun(4)
	if start != 5 || length != 1 {
		t.Fatalf("LongestRun(4) over two isolated offsets = %d, %d, want 5 and 1", start, length)
	}
}

// The longest run available is reported when it is shorter than want, which is
// what lets an allocation shrink to what a fragmented arena has.
func TestLongestRunReportsAShortRun(t *testing.T) {
	s := flat(128)
	s.Take(0, 128)
	s.Put(64)
	s.Put(65)
	s.Put(66)
	start, length := s.LongestRun(8)
	if start != 64 || length != 3 {
		t.Fatalf("LongestRun(8) over a run of three = %d, %d, want 64 and 3", start, length)
	}
}

// The scan is bounded: an arena far larger than the scan window costs a run of
// the window rather than a walk of every offset.
func TestLongestRunScanIsBounded(t *testing.T) {
	s := flat(1 << 17)
	start, length := s.LongestRun(1 << 17)
	if start != 0 || length != 1<<16 {
		t.Fatalf("LongestRun over a space of 131,072 = %d, %d, want 0 and 65,536", start, length)
	}
}

// An offset is an address and a page is memory, and a space may have far more
// of the first than of the second: what bounds an allocation is then the pages
// left, whatever the offsets say.
func TestPagesBoundWhatAnOffsetSpaceGivesOut(t *testing.T) {
	s := slots.New(4096, 8)
	if s.Offsets() != 4096 || s.Pages() != 8 || s.Free() != 8 {
		t.Fatalf("offsets=%d pages=%d free=%d; want 4096, 8 and 8", s.Offsets(), s.Pages(), s.Free())
	}
	// A run never exceeds the pages left, however many offsets are free.
	start, length := s.LongestRun(64)
	if start != 0 || length != 8 {
		t.Fatalf("LongestRun(64) with eight pages left = %d, %d, want 0 and 8", start, length)
	}
	s.Take(1000, 8)
	if s.Held() != 8 || s.Free() != 0 {
		t.Fatalf("held=%d free=%d after taking eight, want 8 and 0", s.Held(), s.Free())
	}
	// Every other offset is still unoccupied, and none of them may be taken.
	if !s.IsFree(0) || !s.IsFree(4095) {
		t.Fatal("an offset that holds no page reads as taken")
	}
	if s.First() != -1 {
		t.Fatalf("the first free offset with no page left is %d, want -1", s.First())
	}
	if start, length := s.LongestRun(1); start != -1 || length != 0 {
		t.Fatalf("LongestRun with no page left = %d, %d, want -1 and 0", start, length)
	}
	s.Put(1003)
	if s.Held() != 7 || s.Free() != 1 || s.First() != 0 {
		t.Fatalf("held=%d free=%d first=%d after returning one page, want 7, 1 and 0",
			s.Held(), s.Free(), s.First())
	}
}

// A space cannot be built with more pages than offsets: a page has to have
// somewhere to be.
func TestMorePagesThanOffsetsIsRefused(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a space of 4 offsets and 8 pages was built")
		}
	}()
	slots.New(4, 8)
}
