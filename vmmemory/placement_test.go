package vmmemory_test

import (
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// What a range costs a VMM in mappings should be how often it alternates
// between shared and private, not how many of its pages are private. That is
// true exactly when private pages adjacent in the guest are adjacent in the
// arena, which is what the placement rule is for: each 2 MiB-aligned range that
// holds a private page owns an extent of the offset space, and a private page
// of that range sits at the offset within it that the page has within the
// range.
//
// These are the counts. Every one of them is a 4 KiB pager whatever page the
// suite is running at, because a pager whose page is the whole range has one
// page per range and nothing to place.

// rangePages is the pages of a 4 KiB pager one 2 MiB-aligned range holds, and
// so the offsets its extent owns.
const rangePages = (2 << 20) / checkpoint.PageSize4KiB

// placedFixture is a 4 KiB pager with an extent for every range its memory regions may
// write into — the offset space a production RAM pager is given, one extent per
// logical page's worth of range, and the pages it may hold at once beside them.
// Read-ahead and write-ahead are one page, so what a test stores into is the
// whole of what it makes private.
func placedFixture(t *testing.T, logical int) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: logical, ArenaOffsets: logical + logical, LogicalPages: logical,
		DirtyPages: logical, ReadAheadPages: 1, WriteAheadPages: 1})
}

// placedMemoryRegion is one memory region of that pager, as large as everything it admits.
func placedMemoryRegion(t *testing.T, pages int) (*fixture, *vmmemory.MemoryRegion, *mapping, *backing) {
	t.Helper()
	f := placedFixture(t, pages)
	r, m, b := f.memoryRegion(pages)
	return f, r, m, b
}

// copied is the pages one store leaves in the arena: the private copy, and the
// page it was copied from, which stays under its published identity so that the
// settle can compare the two and any memory region inheriting that identity maps it.
const copied = 2

// mappings counts the mappings a memory region's arena-backed pages are to its VMM: a
// run of consecutive pages at consecutive arena offsets, with the same write
// access, is one mapping, and every break in either is another. A zero mapping
// owns no arena offset and is left out — a range of zeros is one mapping
// wherever they are, and what the placement rule governs is the pages.
func mappings(m *mapping) int { return mappingsOf(m, false) }

// privateMappings counts only the mappings of the pages the guest may store
// into where they are, which is what the placement rule and the two rules
// behind it govern. The shared pages between them are mappings too, and are
// somewhere else in the arena entirely.
func privateMappings(m *mapping) int { return mappingsOf(m, true) }

func mappingsOf(m *mapping, privateOnly bool) int {
	numbers := make([]uint64, 0, len(m.pages))
	for page, mp := range m.pages {
		if mp.slot >= 0 && (!privateOnly || mp.writable) {
			numbers = append(numbers, page)
		}
	}
	slices.Sort(numbers)
	count := 0
	for i, page := range numbers {
		if i > 0 {
			previous, current := m.pages[numbers[i-1]], m.pages[page]
			if numbers[i-1]+1 == page && current.slot == previous.slot+1 &&
				previous.writable == current.writable {
				continue
			}
		}
		count++
	}
	return count
}

// A private page is at the offset it has within its range, so its arena offset
// and its page number agree modulo the extent — which is the whole of the rule,
// read off one page.
func TestAPrivatePageIsPlacedAtItsOwnOffsetInItsRangesExtent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedMemoryRegion(t, 2*rangePages)
		const page = 300
		access(t, r, m, page, true)[0] = 7
		slot := m.pages[page].slot
		if slot%rangePages != page%rangePages {
			t.Fatalf("page %d is private at offset %d, want an offset congruent to %d modulo %d",
				page, slot, page%rangePages, rangePages)
		}
		if got := mappings(m); got != 1 {
			t.Fatalf("one private page is %d mappings, want 1", got)
		}
		if s := hostStats(t, f); s.PrivateExtents != 1 || s.ResidentPages != copied {
			t.Fatalf("one private page owns %d extents and %d pages, want 1 and %d",
				s.PrivateExtents, s.ResidentPages, copied)
		}
	})
}

// Two adjacent pages of one range are one mapping whichever order the guest
// wrote them in: the second lands beside the first because its offset was
// always going to be beside the first's, not because it was allocated after it.
func TestTwoAdjacentPrivatePagesAreOneMappingInEitherOrder(t *testing.T) {
	for _, order := range [][2]uint64{{300, 301}, {301, 300}} {
		t.Run(fmt.Sprintf("%d-then-%d", order[0], order[1]), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, r, m, _ := placedMemoryRegion(t, 2*rangePages)
				for _, page := range order {
					access(t, r, m, page, true)[0] = 7
				}
				if got := mappings(m); got != 1 {
					t.Fatalf("two adjacent private pages written %d then %d are %d mappings, want 1",
						order[0], order[1], got)
				}
				if got := m.pages[301].slot - m.pages[300].slot; got != 1 {
					t.Fatalf("page 301 is %d offsets past page 300, want 1", got)
				}
				if s := hostStats(t, f); s.PrivateExtents != 1 || s.ResidentPages != 2*copied {
					t.Fatalf("two pages of one range own %d extents and %d pages, want 1 and %d",
						s.PrivateExtents, s.ResidentPages, 2*copied)
				}
			})
		})
	}
}

// Pages far enough apart to alternate between shared and private are a mapping
// each, which is the cost placement alone cannot take away: the shared pages
// between them are somewhere else in the arena, so every private page is its
// own run. Stores nearer than that are the gap rule's, in rules_test.go.
func TestAlternatingPrivatePagesAreAMappingEach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stores, stride = 8, 32
		f, r, m, _ := placedMemoryRegion(t, 2*rangePages)
		for i := range uint64(stores) {
			access(t, r, m, 20+stride*i, true)[0] = 7
		}
		if got := mappings(m); got != stores {
			t.Fatalf("%d private pages %d apart are %d mappings, want %d", stores, stride, got, stores)
		}
		if s := hostStats(t, f); s.PrivateExtents != 1 || s.ResidentPages != stores*copied {
			t.Fatalf("%d alternating pages of one range own %d extents and %d pages, want 1 and %d",
				stores, s.PrivateExtents, s.ResidentPages, stores*copied)
		}
	})
}

// A range is the unit: pages of two ranges are two extents, however adjacent
// the pages are in the guest. The offset space hands its extents out in order,
// so two ranges written in order do come out as one mapping — which is a
// property of the free list and not of the rule, and the extents are still two.
func TestPagesOfTwoRangesAreTwoExtents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedMemoryRegion(t, 4*rangePages)
		for _, page := range []uint64{rangePages - 1, rangePages} {
			access(t, r, m, page, true)[0] = 7
		}
		s := hostStats(t, f)
		if s.PrivateExtents != 2 || s.ResidentPages != 2*copied {
			t.Fatalf("one page either side of a range boundary owns %d extents and %d pages, want 2 and %d",
				s.PrivateExtents, s.ResidentPages, 2*copied)
		}
		for _, page := range []uint64{rangePages - 1, rangePages} {
			if got := m.pages[page].slot % rangePages; got != int(page%rangePages) {
				t.Fatalf("page %d is at offset %d, want one congruent to %d modulo %d",
					page, m.pages[page].slot, page%rangePages, rangePages)
			}
		}
	})
}

// An extent is the range's for as long as the range holds a page, and goes back
// to the offset space whole when it holds none. Detaching the memory region is what
// ends the last of them here; an eviction or a settle ends one the same way.
func TestARangeGivesItsExtentBackWhenItHoldsNoPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := placedFixture(t, 2*rangePages)
		second, m := f.attach(f.newBacking(2 * rangePages))
		for _, page := range []uint64{5, rangePages + 5} {
			access(t, second, m, page, true)[0] = 7
		}
		if s := hostStats(t, f); s.PrivateExtents != 2 {
			t.Fatalf("a page in each of two ranges owns %d extents, want 2", s.PrivateExtents)
		}
		clear(m.pages)
		if err := second.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); s.PrivateExtents != 0 || s.ResidentPages != 0 {
			t.Fatalf("after the memory region detached it owns %d extents and %d pages, want 0 and 0",
				s.PrivateExtents, s.ResidentPages)
		}
	})
}

// A pager whose page is the whole range places nothing: it has one page per
// range, so there is nothing for an extent to arrange, and its offsets and its
// pages stay one number.
func TestAPagerWhosePageIsTheRangePlacesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize2MiB,
			ResidentPages: 8, ArenaOffsets: 4096, LogicalPages: 16, DirtyPages: 8,
			ReadAheadPages: 1, WriteAheadPages: 1})
		r, m, _ := f.memoryRegion(8)
		access(t, r, m, 3, true)[0] = 7
		if s := hostStats(t, f); s.PrivateExtents != 0 || s.ResidentPages != copied {
			t.Fatalf("a 2 MiB pager's store owns %d extents and %d pages, want 0 and %d",
				s.PrivateExtents, s.ResidentPages, copied)
		}
	})
}
