package slots_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory/internal/slots"
)

// A new set holds every slot free, and the lowest of them is the first.
func TestNewSetIsEntirelyFree(t *testing.T) {
	s := slots.New(100)
	if s.Total() != 100 || s.Free() != 100 {
		t.Fatalf("a new set of 100 holds %d slots, %d free", s.Total(), s.Free())
	}
	for slot := range 100 {
		if !s.IsFree(slot) {
			t.Fatalf("slot %d of a new set is not free", slot)
		}
	}
	if s.First() != 0 {
		t.Fatalf("the first free slot of a new set is %d, want 0", s.First())
	}
}

// Taking and returning slots moves exactly those slots, and a returned slot
// becomes the first again however late it is returned.
func TestTakeAndPutMoveExactlyTheirSlots(t *testing.T) {
	s := slots.New(10)
	s.Take(0, 3)
	if s.Free() != 7 {
		t.Fatalf("after taking three of ten, %d are free, want 7", s.Free())
	}
	for slot := range 3 {
		if s.IsFree(slot) {
			t.Fatalf("slot %d is free after being taken", slot)
		}
	}
	if !s.IsFree(3) {
		t.Fatal("slot 3 was taken along with the run below it")
	}
	if s.First() != 3 {
		t.Fatalf("the first free slot is %d, want 3", s.First())
	}
	s.Put(1)
	if s.Free() != 8 || !s.IsFree(1) {
		t.Fatalf("after returning slot 1, %d are free and its bit is %v", s.Free(), s.IsFree(1))
	}
	if s.First() != 1 {
		t.Fatalf("the first free slot is %d, want the returned 1", s.First())
	}
}

// An exhausted set has no first slot and no run.
func TestExhaustedSetOffersNothing(t *testing.T) {
	s := slots.New(64)
	s.Take(0, 64)
	if s.Free() != 0 {
		t.Fatalf("%d slots are free after taking every one", s.Free())
	}
	if s.First() != -1 {
		t.Fatalf("the first free slot of an exhausted set is %d, want -1", s.First())
	}
	start, length := s.LongestRun(1)
	if start != -1 || length != 0 {
		t.Fatalf("LongestRun on an exhausted set = %d, %d, want -1 and 0", start, length)
	}
}

// A run stops at want, so a caller asking for four pages is not charged a scan
// of the whole arena, and it never crosses a taken slot.
func TestLongestRunStopsAtWantAndAtTakenSlots(t *testing.T) {
	s := slots.New(200)
	start, length := s.LongestRun(4)
	if start != 0 || length != 4 {
		t.Fatalf("LongestRun(4) on a free set = %d, %d, want 0 and 4", start, length)
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
		t.Fatalf("LongestRun(4) over two isolated slots = %d, %d, want 5 and 1", start, length)
	}
}

// The longest run available is reported when it is shorter than want, which is
// what lets an allocation shrink to what a fragmented arena has.
func TestLongestRunReportsAShortRun(t *testing.T) {
	s := slots.New(128)
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
// the window rather than a walk of every slot.
func TestLongestRunScanIsBounded(t *testing.T) {
	s := slots.New(1 << 17)
	start, length := s.LongestRun(1 << 17)
	if start != 0 || length != 1<<16 {
		t.Fatalf("LongestRun over a set of 131,072 = %d, %d, want 0 and 65,536", start, length)
	}
}
