package vmmemory_test

import (
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// The bounds of what one store makes private, which neither checkpoints nor
// evicts: write-ahead's run, the half-private rule and the gap rule.

// A store into fresh zeros makes the zero pages after it private up to the end
// of its read-ahead window, then those before it, at most WriteAheadPages of
// them together, and no more than the dirty reservations free at that moment.
func TestAWriteAheadRunGrowsForwardThenBackAsFarAsItsReservations(t *testing.T) {
	for _, c := range []struct {
		dirty int
		run   []uint64
	}{
		// Room for the whole run: pages 6 and 7 after the store, then 5 and 4
		// before it, four in all.
		{dirty: 8, run: []uint64{4, 5, 6, 7}},
		// Three reservations: the store's own and two free, so the run keeps
		// the pages after the store first.
		{dirty: 3, run: []uint64{5, 6, 7}},
	} {
		t.Run(fmt.Sprintf("dirty=%d", c.dirty), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 16,
					DirtyPages: c.dirty, ReadAheadPages: 8, WriteAheadPages: 4}, 16)
				access(t, r, m, 6, true)[0] = 6
				var mapped []uint64
				for page, p := range m.pages {
					if !p.writable {
						t.Fatalf("page %d of the run is not the guest's to store into", page)
					}
					mapped = append(mapped, page)
				}
				slices.Sort(mapped)
				if !slices.Equal(mapped, c.run) {
					t.Fatalf("a store into page 6 made %v private, want %v", mapped, c.run)
				}
				s := hostStats(t, f)
				if s.WriteAheadPages != uint64(len(c.run)-1) || s.DirtyPages != len(c.run) || s.Faults != 1 {
					t.Fatalf("write-ahead %d, dirty %d, faults %d; want %d, %d and 1",
						s.WriteAheadPages, s.DirtyPages, s.Faults, len(c.run)-1, len(c.run))
				}
				if got := access(t, r, m, 6, false)[0]; got != 6 {
					t.Fatalf("page 6 reads %d after the store, want 6", got)
				}
			})
		})
	}
}

// A range is filled exactly when half of it is private: one page short of
// half, nothing is copied by a rule, and the store that makes it half copies
// the rest, nothing private twice, and maps the whole range writable in one
// run.
func TestARangeIsMadeWholeAtHalfPrivateAndNotBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const half = rangePages / 2
		f := placedFixture(t, 4*rangePages)
		r, m, _ := f.memoryRegion(2 * rangePages)
		held(t, r, m, 0, rangePages)
		for page := range uint64(half - 1) {
			access(t, r, m, page, true)[0] = 7
		}
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("a range one page short of half private copied %d pages by a rule, want none", got)
		}
		access(t, r, m, half-1, true)[0] = 7
		if got := hostStats(t, f).RuleCopies; got != half {
			t.Fatalf("the store that made the range half private copied %d pages, want %d", got, half)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("a whole range is %d private mappings, want 1", got)
		}
		for page := range uint64(rangePages) {
			if !m.pages[page].writable {
				t.Fatalf("page %d of a whole range is not the guest's to store into", page)
			}
		}
		// Page i holds i+1 in every byte, so the range's last page holds
		// rangePages, of which a byte keeps the low eight bits.
		const last = rangePages - 1
		if got, want := access(t, r, m, last, false)[0], byte((last+1)%256); got != want {
			t.Fatalf("the last page of the filled range reads %d, want the %d it held", got, want)
		}
	})
}

// A gap of exactly the bound is closed whichever side of it the guest stores
// into second, and from the first page of a range as from any other.
func TestAGapOfTheBoundIsClosedFromEitherSide(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second uint64
	}{
		{"forward", 100, 100 + gap},
		{"backward", 100 + gap, 100},
		{"from a range's first page", 0, gap},
		{"back to a range's first page", gap, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, r, m, _ := placedMemoryRegion(t, 2*rangePages)
				r.PressMappings()
				low, high := min(c.first, c.second), max(c.first, c.second)
				held(t, r, m, low, high+1)
				access(t, r, m, c.first, true)[0] = 7
				access(t, r, m, c.second, true)[0] = 7
				if got := hostStats(t, f).RuleCopies; got != gap-1 {
					t.Fatalf("closing a gap of %d pages copied %d, want %d", gap, got, gap-1)
				}
				if got := privateMappings(m); got != 1 {
					t.Fatalf("stores %d pages apart are %d private mappings, want 1", gap, got)
				}
			})
		})
	}
}
