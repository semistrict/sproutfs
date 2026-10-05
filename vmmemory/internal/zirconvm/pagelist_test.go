// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/vmpl_unittest.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
)

// Each case runs at the two page sizes a pager runs at, in a synctest bubble,
// as every ported case does. Zircon's kPageSize is ps here.

// testPage is a page. Zircon's cases allocate vm_page_t from the pmm.
type testPage struct{ id int }

var pageSizes = []uint64{4 << 10, 2 << 20}

func forEachPageSize(t *testing.T, body func(t *testing.T, ps uint64)) {
	t.Helper()
	for _, ps := range pageSizes {
		name := fmt.Sprintf("%dKiB", ps>>10)
		if ps >= 1<<20 {
			name = fmt.Sprintf("%dMiB", ps>>20)
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { body(t, ps) })
		})
	}
}

// getPages is GetPages.
func getPages(count int) []*testPage {
	pages := make([]*testPage, count)
	for i := range pages {
		pages[i] = &testPage{id: i}
	}
	return pages
}

// addPage is AddPage: it puts page at offset, splitting any interval there,
// and reports whether it could.
func addPage(t *testing.T, pl *PageList[testPage], page *testPage, offset uint64) bool {
	t.Helper()
	return addContent(t, pl, Page(page), offset)
}

// addMarker is AddMarker.
func addMarker(t *testing.T, pl *PageList[testPage], offset uint64) bool {
	t.Helper()
	return addContent(t, pl, Marker[testPage](), offset)
}

// addReference is AddReference.
func addReference(t *testing.T, pl *PageList[testPage], ref ReferenceValue, offset uint64) bool {
	t.Helper()
	return addContent(t, pl, Reference[testPage](ref), offset)
}

func addContent(t *testing.T, pl *PageList[testPage], content PageOrMarker[testPage], offset uint64) bool {
	t.Helper()
	slot, isInterval := pl.LookupOrAllocate(offset, SplitInterval)
	if slot == nil {
		return false
	}
	if !slot.IsEmpty() && !slot.IsIntervalSlot() {
		return false
	}
	if !slot.IsEmpty() && !isInterval {
		t.Fatalf("the slot at %#x is an interval slot outside an interval", offset)
	}
	slot.Set(content)
	return true
}

// testReference is TestReference: a reference value with its low bits clear.
func testReference(v uint32) uint32 { return v << ReferenceAlignBits }

// freedContent is a Freer that keeps what it is given.
type freedContent struct {
	pages []*testPage
	refs  []ReferenceValue
}

func (f *freedContent) FreePage(page *testPage)          { f.pages = append(f.pages, page) }
func (f *freedContent) FreeReference(ref ReferenceValue) { f.refs = append(f.refs, ref) }

// moveSlot is the merge function Zircon's take cases pass AddPagesFrom.
func moveSlot(src, dst *PageOrMarker[testPage], _ uint64) { dst.Set(src.Take()) }

// releasePages is the RemoveAllContent callback that keeps pages and drops
// the rest.
func releasePages(freed *[]*testPage) func(PageOrMarker[testPage]) {
	return func(p PageOrMarker[testPage]) {
		if p.IsPage() {
			*freed = append(*freed, p.ReleasePage())
		}
	}
}

func dropContent(PageOrMarker[testPage]) {}

var (
	errInternal    = errors.New("internal")
	errOutOfRange  = errors.New("out of range")
	errInvalidArgs = errors.New("invalid args")
)

func expect[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func expectNoError(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

func mustNotFail(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// expectOffsets checks offsets against offsets in pages.
func expectOffsets(t *testing.T, what string, got []uint64, wantPages []uint64, ps uint64) {
	t.Helper()
	want := make([]uint64, len(wantPages))
	for i, p := range wantPages {
		want[i] = p * ps
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %#x, want %#x", what, got, want)
	}
}

// vmpl_append_to_splice_list_test
func TestPagesInsertedIntoASpliceListPopInOrder(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		const numPages = 5
		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(numPages * ps)

		pages := getPages(numPages)
		for i, page := range pages {
			expectNoError(t, "insert", splice.Insert(uint64(i)*ps, Page(page)))
		}

		splice.Finalize()
		expect(t, "finalized", splice.IsFinalized(), true)

		for i := range numPages {
			page := splice.Pop()
			expect(t, "popped page", page.Page(), pages[i])
			page.ReleasePage()
		}
	})
}

// vmpl_add_remove_page_test
func TestAPageAddedAtAnOffsetIsFoundAndRemovedThere(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		page := getPages(1)[0]

		expect(t, "added", addPage(t, pl, page, 0), true)

		expect(t, "page at 0", pl.Lookup(0).Page(), page)
		expect(t, "empty", pl.IsEmpty(), false)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), false)

		removed := pl.RemoveContent(0)
		expect(t, "removed page", removed.ReleasePage(), page)
		expect(t, "removed again", pl.RemoveContent(0).IsEmpty(), true)

		expect(t, "empty", pl.IsEmpty(), true)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), true)
	})
}

// vmpl_basic_marker_test
func TestAMarkerIsContentButNoPage(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)

		expect(t, "empty", pl.IsEmpty(), true)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), true)

		expect(t, "added", addMarker(t, pl, 0), true)

		expect(t, "marker at 0", pl.Lookup(0).IsMarker(), true)

		expect(t, "empty", pl.IsEmpty(), false)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), true)

		removed := pl.RemoveContent(0)
		expect(t, "removed a marker", removed.IsMarker(), true)

		expect(t, "no page or ref", pl.HasNoPageOrRef(), true)
		expect(t, "empty", pl.IsEmpty(), true)
	})
}

// vmpl_basic_reference_test
func TestReferencesIncludingZeroAreHeldAndRemoved(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)

		expect(t, "empty", pl.IsEmpty(), true)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), true)

		// The zero reference is valid.
		ref0 := MakeReferenceValue(0)
		expect(t, "added ref0", addReference(t, pl, ref0, 0), true)

		expect(t, "empty", pl.IsEmpty(), false)
		expect(t, "no page or ref", pl.HasNoPageOrRef(), false)

		// A nonzero reference.
		ref1 := MakeReferenceValue(testReference(1))
		expect(t, "added ref1", addReference(t, pl, ref1, ps), true)

		removed := pl.RemoveContent(0)
		expect(t, "ref0", removed.ReleaseReference().Value(), ref0.Value())

		expect(t, "empty", pl.IsEmpty(), false)
		expect(t, "no page, ref or marker", pl.HasNoPageRefOrMarker(), false)

		removed = pl.RemoveContent(ps)
		expect(t, "ref1", removed.ReleaseReference().Value(), ref1.Value())

		expect(t, "empty", pl.IsEmpty(), true)
		expect(t, "no page, ref or marker", pl.HasNoPageRefOrMarker(), true)
	})
}

// vmpl_free_pages_test
func TestRemovingPagesFromARangeLeavesThoseOutsideIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const count = 3 * pageFanOut
		testPages := getPages(count)

		// Pages and markers alternate.
		for i := range uint64(count) {
			expect(t, "added page", addPage(t, pl, testPages[i], i*2*ps), true)
			expect(t, "added marker", addMarker(t, pl, (i*2+1)*ps), true)
		}

		var list []*testPage
		err := pl.RemovePages(func(p *PageOrMarker[testPage], _ uint64) error {
			if p.IsPage() {
				list = append(list, p.ReleasePage())
			}
			p.Set(Empty[testPage]())
			return nil
		}, 2*ps, (count-1)*2*ps)
		expectNoError(t, "remove pages", err)
		// Zircon checks each page is on its free list.
		if !slices.Equal(list, testPages[1:count-1]) {
			t.Errorf("removed pages %v, want %v", list, testPages[1:count-1])
		}

		for i := range uint64(count) {
			removePage := pl.RemoveContent(i * 2 * ps)
			removeMarker := pl.RemoveContent((i*2 + 1) * ps)
			if i == 0 || i == count-1 {
				expect(t, "page kept", removePage.IsPage(), true)
				expect(t, "marker kept", removeMarker.IsMarker(), true)
				expect(t, "kept page", removePage.ReleasePage(), testPages[i])
			} else {
				expect(t, "page removed", removePage.IsEmpty(), true)
				expect(t, "marker removed", removeMarker.IsEmpty(), true)
			}
		}
	})
}

// vmpl_free_pages_last_page_test
func TestRemovingAllContentHandsOverTheLastPage(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		page := getPages(1)[0]

		pl := NewPageList[testPage](ps)
		expect(t, "added", addPage(t, pl, page, 0), true)

		expect(t, "page at 0", pl.Lookup(0).Page(), page)

		var list []*testPage
		pl.RemoveAllContent(releasePages(&list))
		expect(t, "empty", pl.IsEmpty(), true)

		if !slices.Equal(list, []*testPage{page}) {
			t.Errorf("removed %v, want the one page", list)
		}
	})
}

// vmpl_near_last_offset_free
func TestPagesNearTheTopOfTheOffsetsAreAddableUpToMaxSize(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		page := getPages(1)[0]

		// Zircon starts at 0xfffffffffff00000, 256 pages of 4 KiB below the
		// top; this starts 256 pages below at either size.
		atLeastOne := false
		for addr := 0 - 256*ps; addr != 0; addr += ps {
			pl := NewPageList[testPage](ps)
			if addPage(t, pl, page, addr) {
				atLeastOne = true
				expect(t, "page at addr", pl.Lookup(addr).Page(), page)

				var list []*testPage
				pl.RemoveAllContent(releasePages(&list))

				if !slices.Equal(list, []*testPage{page}) {
					t.Errorf("removed %v at %#x, want the one page", list, addr)
				}
				expect(t, "empty", pl.IsEmpty(), true)
			}
		}
		expect(t, "some offset was addable", atLeastOne, true)

		// Zircon's 0xffffffffffff0000 is MAX_SIZE at 4 KiB.
		pl2 := NewPageList[testPage](ps)
		slot, _ := pl2.LookupOrAllocate(pl2.MaxSize(), NoIntervals)
		expect(t, "slot at MaxSize", slot, nil)
	})
}

// vmpl_take_single_page_even_test
func TestTakingTheFirstPageOfANodeLeavesTheNext(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(2)

		expect(t, "added", addPage(t, pl, pages[0], 0), true)
		expect(t, "added", addPage(t, pl, pages[1], ps), true)

		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(ps)
		splice.AddPagesFrom(moveSlot, pl, 0)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "popped", splice.Pop().Page(), pages[0])
		expect(t, "processed", splice.IsProcessed(), true)
		expect(t, "nothing left at 0", pl.Lookup(0) == nil || pl.Lookup(0).IsEmpty(), true)

		expect(t, "removed", pl.RemoveContent(ps).Page(), pages[1])
	})
}

// vmpl_take_single_page_odd_test
func TestTakingASecondPageOfANodeLeavesTheFirst(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(2)

		expect(t, "added", addPage(t, pl, pages[0], 0), true)
		expect(t, "added", addPage(t, pl, pages[1], ps), true)

		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(ps)
		splice.AddPagesFrom(moveSlot, pl, ps)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "popped", splice.Pop().Page(), pages[1])
		expect(t, "processed", splice.IsProcessed(), true)
		expect(t, "nothing left at ps", pl.Lookup(ps) == nil || pl.Lookup(ps).IsEmpty(), true)

		expect(t, "removed", pl.RemoveContent(0).Page(), pages[0])
	})
}

// vmpl_take_all_pages_test
func TestTakingEveryPageEmptiesTheList(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const count = 3 * pageFanOut
		testPages := getPages(count)

		for i := range uint64(count) {
			expect(t, "added page", addPage(t, pl, testPages[i], i*2*ps), true)
			expect(t, "added marker", addMarker(t, pl, (i*2+1)*ps), true)
		}

		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(count * 2 * ps)
		splice.AddPagesFrom(moveSlot, pl, 0)
		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "list empty", pl.IsEmpty(), true)

		for i := range count {
			expect(t, "popped page", splice.Pop().Page(), testPages[i])
			expect(t, "popped marker", splice.Pop().IsMarker(), true)
		}
		expect(t, "processed", splice.IsProcessed(), true)
	})
}

// vmpl_take_middle_pages_test
func TestTakingPagesAcrossNodesLeavesThoseOutsideTheRange(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const count = 3 * pageFanOut
		testPages := getPages(count)

		for i := range uint64(count) {
			expect(t, "added", addPage(t, pl, testPages[i], i*ps), true)
		}

		const takeOffset = pageFanOut - 1
		const takeCount = pageFanOut + 2
		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(takeCount * ps)
		splice.AddPagesFrom(moveSlot, pl, takeOffset*ps)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "list empty", pl.IsEmpty(), false)

		for i := range uint64(count) {
			if takeOffset <= i && i < takeOffset+takeCount {
				expect(t, "popped", splice.Pop().Page(), testPages[i])
			} else {
				expect(t, "removed", pl.RemoveContent(i*ps).Page(), testPages[i])
			}
		}
		expect(t, "processed", splice.IsProcessed(), true)
	})
}

// vmpl_take_gap_test
func TestTakingARangeKeepsItsGaps(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const count = pageFanOut
		const gapSize = 2
		testPages := getPages(count)

		for i := range uint64(count) {
			offset := i * (gapSize + 1) * ps
			expect(t, "added", addPage(t, pl, testPages[i], offset), true)
		}

		listStart := ps
		listLen := (count*(gapSize+1) - 2) * ps
		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(listLen)
		splice.AddPagesFrom(moveSlot, pl, listStart)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "removed", pl.RemoveContent(0).Page(), testPages[0])
		expect(t, "nothing at the end", pl.Lookup(listLen) == nil || pl.Lookup(listLen).IsEmpty(), true)

		for offset := listStart; offset < listStart+listLen; offset += ps {
			pageIdx := offset / ps
			if pageIdx%(gapSize+1) == 0 {
				expect(t, "popped", splice.Pop().Page(), testPages[pageIdx/(gapSize+1)])
			} else {
				expect(t, "gap popped", splice.Pop().IsEmpty(), true)
			}
		}
		expect(t, "processed", splice.IsProcessed(), true)
	})
}

// vmpl_take_empty_test
func TestTakingFromAnEmptyRangeGivesAnEmptySpliceList(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)

		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(ps)
		splice.AddPagesFrom(moveSlot, pl, ps)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "processed", splice.IsProcessed(), false)
		expect(t, "popped", splice.Pop().IsEmpty(), true)
		expect(t, "processed", splice.IsProcessed(), true)
	})
}

// vmpl_take_cleanup_test
func TestFreeingASpliceListGivesBackItsPages(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		page := getPages(1)[0]

		pl := NewPageList[testPage](ps)
		expect(t, "added", addPage(t, pl, page, 0), true)

		freed := &freedContent{}
		splice := NewPageSpliceList(ps, freed)
		splice.Initialize(ps)
		splice.AddPagesFrom(moveSlot, pl, 0)

		expect(t, "finalized", splice.IsFinalized(), true)
		expect(t, "processed", splice.IsProcessed(), false)

		// Zircon's destructor frees the page; Go has none, so Free does.
		splice.Free()
		if !slices.Equal(freed.pages, []*testPage{page}) {
			t.Errorf("freed %v, want the one page", freed.pages)
		}
		expect(t, "processed", splice.IsProcessed(), true)
	})
}

// pageGapIterBody is vmpl_page_gap_iter_test_body: it builds a list from
// pages, where nil is a gap, and walks its pages and gaps, stopping at the
// stopIdx-th entry.
func pageGapIterBody(t *testing.T, ps uint64, pages []*testPage, stopIdx uint64) {
	t.Helper()
	list := NewPageList[testPage](ps)
	for i, page := range pages {
		if page != nil {
			expect(t, "added", addPage(t, list, page, uint64(i)*ps), true)
		}
	}

	idx := uint64(0)
	err := list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if off != idx*ps || !p.IsPage() || pages[idx] != p.Page() {
				return errInternal
			}
			if idx == stopIdx {
				return ErrStop
			}
			idx++
			return nil
		},
		func(gapStart, gapEnd uint64) error {
			for o := gapStart; o < gapEnd; o += ps {
				if o != idx*ps || pages[idx] != nil {
					return errInternal
				}
				if idx == stopIdx {
					return ErrStop
				}
				idx++
			}
			return nil
		},
		0, uint64(len(pages))*ps)
	if err != nil {
		t.Fatalf("pages %v stopping at %d: %v", pages, stopIdx, err)
	}
	if idx != stopIdx {
		t.Fatalf("pages %v stopping at %d: stopped at %d", pages, stopIdx, idx)
	}

	var freeList []*testPage
	list.RemoveAllContent(func(p PageOrMarker[testPage]) { freeList = append(freeList, p.ReleasePage()) })
	if !list.IsEmpty() {
		t.Fatalf("pages %v: the list is not empty", pages)
	}
}

// vmpl_page_gap_iter_test
func TestWalkingPagesAndGapsStopsWhereAskedForEveryLayoutOfFourPages(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		const count = 4
		pages := getPages(count)

		list := make([]*testPage, count)
		for i := range uint64(count) {
			for j := range 1 << count {
				for k := range count {
					if j&(1<<k) != 0 {
						list[k] = pages[k]
					} else {
						list[k] = nil
					}
				}
				pageGapIterBody(t, ps, list, i)
			}
		}
	})
}

// vmpl_for_every_page_test
func TestWalkingEveryPageVisitsPagesAndMarkersInOrder(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const count = 5
		testPages := getPages(count)

		offsets := []uint64{
			0,
			ps,
			pageFanOut*ps - ps,
			pageFanOut * ps,
			pageFanOut*ps + ps,
		}

		for i := range count {
			if i%2 == 1 {
				expect(t, "added page", addPage(t, list, testPages[i], offsets[i]), true)
			} else {
				expect(t, "added marker", addMarker(t, list, offsets[i]), true)
			}
		}

		idx := 0
		iterFn := func(p *PageOrMarker[testPage], off uint64) error {
			expect(t, "offset", off, offsets[idx])

			if idx%2 == 1 {
				expect(t, "is a page", p.IsPage(), true)
				expect(t, "page", p.Page(), testPages[idx])
			} else {
				expect(t, "is a marker", p.IsMarker(), true)
			}

			idx++
			return nil
		}

		expectNoError(t, "every page", list.ForEveryPage(iterFn))
		if idx != len(offsets) {
			t.Fatalf("visited %d, want %d", idx, len(offsets))
		}

		idx = 1
		expectNoError(t, "a range", list.ForEveryPageInRange(iterFn, offsets[1], offsets[len(testPages)-1]))
		if idx != len(offsets)-1 {
			t.Fatalf("visited up to %d, want %d", idx, len(offsets)-1)
		}

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
	})
}

// vmpl_skip_last_gap_test
func TestAWalkStoppedAtAPageReportsNoGapAfterIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)
		page := getPages(1)[0]

		expect(t, "added", addPage(t, list, page, ps), true)

		var sawGapStart, sawGapEnd uint64
		gapsSeen := 0
		err := list.ForEveryPageAndGapInRange(
			func(*PageOrMarker[testPage], uint64) error { return ErrStop },
			func(gapStart, gapEnd uint64) error {
				sawGapStart = gapStart
				sawGapEnd = gapEnd
				gapsSeen++
				return nil
			},
			0, ps*3)
		expectNoError(t, "walk", err)

		// One gap, the right one.
		expect(t, "gaps seen", gapsSeen, 1)
		expect(t, "gap start", sawGapStart, 0)
		expect(t, "gap end", sawGapEnd, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
	})
}

// runRecorder is the contiguous run callback of Zircon's run cases: it notes
// each run that is not an interval and refuses intervals.
func runRecorder(ranges *[]uint64, result error) func(start, end uint64, isInterval bool) error {
	return func(start, end uint64, isInterval bool) error {
		if isInterval {
			return errBadState
		}
		*ranges = append(*ranges, start, end)
		return result
	}
}

func always(*PageOrMarker[testPage], uint64) bool { return true }

// vmpl_contiguous_run_test
func TestContiguousRunsSplitAtGapsAndNodeBoundariesDoNot(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const count = 6
		testPages := getPages(count)

		// Some pages in one node and some in others, so that pages land in
		// new nodes.
		if pageFanOut <= 4 {
			t.Fatalf("a node has %d slots, want more than 4", pageFanOut)
		}
		// A page, then a gap.
		expect(t, "added", addPage(t, list, testPages[0], 0), true)
		// A gap in the same node, then two pages.
		expect(t, "added", addPage(t, list, testPages[1], 2*ps), true)
		expect(t, "added", addPage(t, list, testPages[2], 3*ps), true)
		// A gap to the next node, then three pages across a node boundary.
		expect(t, "added", addPage(t, list, testPages[3], (pageFanOut*2-1)*ps), true)
		expect(t, "added", addPage(t, list, testPages[4], pageFanOut*2*ps), true)
		expect(t, "added", addPage(t, list, testPages[5], (pageFanOut*2+1)*ps), true)

		// A plain walk lists the runs.
		var rangeOffsets []uint64
		err := list.ForEveryPageAndContiguousRunInRange(always,
			func(*PageOrMarker[testPage], uint64) error { return nil },
			runRecorder(&rangeOffsets, nil),
			0, pageFanOut*3*ps)

		expectNoError(t, "walk", err)
		expectOffsets(t, "runs", rangeOffsets, []uint64{0, 1, 2, 4, pageFanOut*2 - 1, pageFanOut*2 + 2}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 6)
	})
}

// vmpl_contiguous_run_compare_test
func TestContiguousRunsSplitWhereTheComparisonFails(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const count = 5
		testPages := getPages(count)

		// Five consecutive pages, divided into runs by the comparison.
		for i := range uint64(count) {
			expect(t, "added", addPage(t, list, testPages[i], i*ps), true)
		}

		// What the comparison says of each page.
		compareResults := [count]bool{false, true, true, false, true}
		var pageVisited [count]bool
		var rangeOffsets []uint64

		err := list.ForEveryPageAndContiguousRunInRange(
			func(_ *PageOrMarker[testPage], off uint64) bool { return compareResults[off/ps] },
			func(_ *PageOrMarker[testPage], off uint64) error {
				pageVisited[off/ps] = true
				return nil
			},
			runRecorder(&rangeOffsets, nil),
			0, pageFanOut*ps)

		expectNoError(t, "walk", err)

		expect(t, "pages visited", pageVisited, compareResults)
		expectOffsets(t, "runs", rangeOffsets, []uint64{1, 3, 4, 5}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 5)
	})
}

// vmpl_contiguous_traversal_end_test
func TestAContiguousRunWalkStopsAfterThePageOrRunThatAsks(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const count = 3
		testPages := getPages(count)

		for i := range uint64(count) {
			expect(t, "added", addPage(t, list, testPages[i], i*ps), true)
		}

		var pageVisited [3]bool
		var rangeOffsets []uint64
		// The comparison accepts every page, but the page callback stops the
		// walk.
		err := list.ForEveryPageAndContiguousRunInRange(always,
			func(_ *PageOrMarker[testPage], off uint64) error {
				pageVisited[off/ps] = true
				// Stop at page 1. It is the last page handled, and it is in
				// the run: the walk stops after it.
				if off/ps < 1 {
					return nil
				}
				return ErrStop
			},
			runRecorder(&rangeOffsets, nil),
			0, pageFanOut*ps)

		expectNoError(t, "walk", err)
		// The first two pages were visited.
		expect(t, "pages visited", pageVisited, [3]bool{true, true, false})
		expectOffsets(t, "runs", rangeOffsets, []uint64{0, 2}, ps)

		// Again, now stopped by the run callback.
		pageVisited = [3]bool{}
		rangeOffsets = nil
		err = list.ForEveryPageAndContiguousRunInRange(
			// Even pages are in the run.
			func(_ *PageOrMarker[testPage], off uint64) bool { return (off/ps)%2 == 0 },
			func(_ *PageOrMarker[testPage], off uint64) error {
				pageVisited[off/ps] = true
				return nil
			},
			// Stop after the first run.
			runRecorder(&rangeOffsets, ErrStop),
			0, pageFanOut*ps)

		expectNoError(t, "walk", err)
		// Only the first page was visited.
		expect(t, "pages visited", pageVisited, [3]bool{true, false, false})
		expectOffsets(t, "runs", rangeOffsets, []uint64{0, 1}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 3)
	})
}

// vmpl_contiguous_traversal_error_test
func TestAContiguousRunWalkFailsWithThePageOrRunError(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const count = 3
		testPages := getPages(count)

		for i := range uint64(count) {
			expect(t, "added", addPage(t, list, testPages[i], i*ps), true)
		}

		var pageVisited [3]bool
		var rangeOffsets []uint64
		// The comparison accepts every page, but the page callback fails.
		err := list.ForEveryPageAndContiguousRunInRange(always,
			func(_ *PageOrMarker[testPage], off uint64) error {
				pageVisited[off/ps] = true
				// Only page 0 succeeds.
				if off/ps < 1 {
					return nil
				}
				return errBadState
			},
			runRecorder(&rangeOffsets, nil),
			0, pageFanOut*ps)

		expect(t, "walk", err, errBadState)
		// The first two pages were visited.
		expect(t, "pages visited", pageVisited, [3]bool{true, true, false})
		// The run up to the page that failed was handled.
		expectOffsets(t, "runs", rangeOffsets, []uint64{0, 1}, ps)

		// Again, now failed by the run callback.
		pageVisited = [3]bool{}
		rangeOffsets = nil
		err = list.ForEveryPageAndContiguousRunInRange(
			// Even pages are in the run.
			func(_ *PageOrMarker[testPage], off uint64) bool { return (off/ps)%2 == 0 },
			func(_ *PageOrMarker[testPage], off uint64) error {
				pageVisited[off/ps] = true
				return nil
			},
			// Fail after the first run.
			runRecorder(&rangeOffsets, errBadState),
			0, pageFanOut*ps)

		expect(t, "walk", err, errBadState)
		// Only the first page was visited.
		expect(t, "pages visited", pageVisited, [3]bool{true, false, false})
		expectOffsets(t, "runs", rangeOffsets, []uint64{0, 1}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 3)
	})
}

// vmpl_cursor_test
func TestACursorWalksOnlyConsecutiveSlots(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Entries in nodes some of which follow each other and some not.
		const off1 = pageFanOut*3 + 4
		const off2 = pageFanOut*5 + 4
		const off3 = pageFanOut*6 + 1
		const off4 = pageFanOut*6 + 2
		const off5 = pageFanOut*8 + 1

		for _, off := range []uint64{off1, off2, off3, off4, off5} {
			expect(t, "added", addMarker(t, list, off*ps), true)
		}

		// An offset outside every node gives a cursor that is not valid.
		cursor := list.LookupMutableCursor((off1 - pageFanOut) * ps)
		expect(t, "current", cursor.Current(), nil)
		cursor = list.LookupMutableCursor((off1 + pageFanOut) * ps)
		expect(t, "current", cursor.Current(), nil)

		// One in a node gives a cursor, even with nothing at the offset.
		cursor = list.LookupMutableCursor((off1 - 1) * ps)
		if cursor.Current() == nil {
			t.Fatal("no cursor in a node")
		}
		expect(t, "empty", cursor.Current().IsEmpty(), true)

		// The cursor steps onto the marker.
		cursor.Step()
		if cursor.Current() == nil {
			t.Fatal("no cursor at the marker")
		}
		expect(t, "marker", cursor.Current().IsMarker(), true)

		// It stops at the end of the node, since the next node does not
		// follow.
		cursor.Step()
		for cursor.Current() != nil {
			expect(t, "empty", cursor.Current().IsEmpty(), true)
			cursor.Step()
		}

		// It walks across nodes that follow each other.
		cursor = list.LookupMutableCursor(off2 * ps)
		if cursor.Current() == nil {
			t.Fatal("no cursor at off2")
		}
		expect(t, "marker", cursor.Current().IsMarker(), true)
		cursor.Step()

		// On to the next marker, in another node, counting the slots.
		items := uint64(0)
		countToMarker := func(p *PageOrMarker[testPage]) error {
			items++
			if p.IsMarker() {
				return ErrStop
			}
			return nil
		}
		expectNoError(t, "walk", cursor.ForEveryContiguous(countToMarker))
		expect(t, "items", items, off3-off2)

		// The walk stopped at off3, so the next slot is off4, also a marker.
		cursor.Step()
		if cursor.Current() == nil {
			t.Fatal("no cursor at off4")
		}
		expect(t, "marker", cursor.Current().IsMarker(), true)

		// Again, it stops: the next marker is in a node that does not follow.
		items = 0
		cursor.Step()
		expectNoError(t, "walk", cursor.ForEveryContiguous(countToMarker))
		expect(t, "current", cursor.Current(), nil)
		// It walked the rest of off4's node.
		expect(t, "items", items, pageFanOut-(off4%pageFanOut)-1)

		list.RemoveAllContent(dropContent)
	})
}

// startEndWalk is the walk of Zircon's interval cases: every slot is a Dirty
// interval start or end, whose offsets it notes, and every gap is noted.
func startEndWalk(list *PageList[testPage], from, to uint64) (start, end uint64, gaps []uint64, err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() {
				start = off
			} else if p.IsIntervalEnd() {
				end = off
			}
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return start, end, gaps, err
}

// vmpl_interval_single_node_test
func TestAZeroIntervalInOneNodeIsAStartAndAnEnd(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// [1, 3] in one node.
		const expectedStart, expectedEnd = 1, 3
		const size = pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		var start, end uint64
		err := list.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() {
				start = off
			} else if p.IsIntervalEnd() {
				end = off
			}
			return nil
		})
		expectNoError(t, "walk", err)
		expect(t, "start", start, expectedStart*ps)
		expect(t, "end", end, expectedEnd*ps)

		start, end, gaps, err := startEndWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expect(t, "start", start, expectedStart*ps)
		expect(t, "end", end, expectedEnd*ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_multiple_nodes_test
func TestAZeroIntervalAcrossNodesIsAStartAndAnEnd(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		var start, end uint64
		err := list.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() {
				start = off
			} else if p.IsIntervalEnd() {
				end = off
			}
			return nil
		})
		expectNoError(t, "walk", err)
		expect(t, "start", start, expectedStart*ps)
		expect(t, "end", end, expectedEnd*ps)

		start, end, gaps, err := startEndWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expect(t, "start", start, expectedStart*ps)
		expect(t, "end", end, expectedEnd*ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_traversal_test
func TestAWalkStartingOrEndingInsideAnIntervalSeesNoGapThere(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Ending inside the interval sees only the gap before its start.
		start, end, gaps, err := startEndWalk(list, 0, (expectedEnd-1)*ps)
		expectNoError(t, "walk", err)
		expect(t, "start", start, expectedStart*ps)
		// Not the end.
		expect(t, "end", end, 0)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart}, ps)

		// Starting inside it sees only the gap after its end.
		start, end, gaps, err = startEndWalk(list, (expectedStart+1)*ps, size*ps)
		expectNoError(t, "walk", err)
		// Not the start.
		expect(t, "start", start, 0)
		expect(t, "end", end, expectedEnd*ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)

		// Starting and ending inside it sees neither gaps nor slots.
		start, end, gaps, err = startEndWalk(list, (expectedStart+1)*ps, (expectedEnd-1)*ps)
		expectNoError(t, "walk", err)
		expect(t, "start", start, 0)
		expect(t, "end", end, 0)
		expect(t, "gaps", len(gaps), 0)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_merge_test
func TestAdjacentZeroIntervalsInOneStateMerge(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// [7, 12].
		const expectedStart, expectedEnd = 7, 12
		const size = 2 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Intervals to its left and right merge with it into one.
		const newExpectedStart = 3
		const newExpectedEnd = 20
		// [3, 6].
		mustNotFail(t, "add left", list.AddZeroInterval(newExpectedStart*ps, expectedStart*ps, IntervalDirty))
		// [13, 20].
		mustNotFail(t, "add right", list.AddZeroInterval((expectedEnd+1)*ps, (newExpectedEnd+1)*ps, IntervalDirty))

		var start, end uint64
		err := list.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() {
				start = off
			} else if p.IsIntervalEnd() {
				end = off
			}
			return nil
		})
		expectNoError(t, "walk", err)
		expect(t, "start", start, newExpectedStart*ps)
		expect(t, "end", end, newExpectedEnd*ps)

		start, end, gaps, err := startEndWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expect(t, "start", start, newExpectedStart*ps)
		expect(t, "end", end, newExpectedEnd*ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, newExpectedStart, newExpectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// intervalPageWalk is the walk of Zircon's add page cases: every slot is an
// interval start, an interval end or a page. Starts and ends alternate and
// are noted, as are the page and every gap.
func intervalPageWalk(list *PageList[testPage], from, to uint64, pageOff *uint64) (intervals, gaps []uint64,
	err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd() || p.IsPage()) {
				return errBadState
			}
			if p.IsIntervalStart() {
				if len(intervals)%2 == 1 {
					return errBadState
				}
				intervals = append(intervals, off)
			} else if p.IsIntervalEnd() {
				if len(intervals)%2 == 0 {
					return errBadState
				}
				intervals = append(intervals, off)
			} else if p.IsPage() {
				*pageOff = off
			}
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return intervals, gaps, err
}

// slotPageWalk is the walk of Zircon's add page cases once the interval is
// all Slots: every slot is an interval Slot or a page, and each is noted, as
// is every gap.
func slotPageWalk(list *PageList[testPage], from, to uint64) (slots, pages, gaps []uint64, err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalSlot() || p.IsPage()) {
				return errBadState
			}
			if p.IsIntervalSlot() {
				slots = append(slots, off)
			} else if p.IsPage() {
				pages = append(pages, off)
			}
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return slots, pages, gaps, err
}

// vmpl_interval_add_page_test
func TestAPageAddedInsideAnIntervalSplitsIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// A page in the interval splits it.
		page := getPages(1)[0]
		const pageOffset = pageFanOut
		expect(t, "added", addPage(t, list, page, pageOffset*ps), true)

		var pageOff uint64
		intervals, gaps, err := intervalPageWalk(list, 0, size*ps, &pageOff)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals,
			[]uint64{expectedStart, pageOffset - 1, pageOffset + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)
		expect(t, "page", pageOff, pageOffset*ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 1)
	})
}

// vmpl_interval_add_page_slots_test
func TestAPageAddedInsideAThreePageIntervalLeavesTwoSlots(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Three pages, so a page in the middle leaves two Slots.
		const expectedStart, expectedEnd = 0, 2
		const size = pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// A page in the interval splits it.
		page := getPages(1)[0]
		const pageOffset = 1
		expect(t, "added", addPage(t, list, page, pageOffset*ps), true)

		slots, pages, gaps, err := slotPageWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "slots", slots, []uint64{expectedStart, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)
		expectOffsets(t, "pages", pages, []uint64{pageOffset}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 1)
	})
}

// vmpl_interval_add_page_start_test
func TestPagesAddedAtAnIntervalStartMoveTheStart(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const expectedStart, expectedEnd = 0, 2
		const size = pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		pages := getPages(2)

		// A page at the interval's start.
		expect(t, "added", addPage(t, list, pages[0], expectedStart*ps), true)

		pageOff := uint64(size * ps)
		intervals, gaps, err := intervalPageWalk(list, 0, size*ps, &pageOff)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{expectedStart + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)
		expect(t, "page", pageOff, expectedStart*ps)

		// Another at the new interval's start.
		expect(t, "added", addPage(t, list, pages[1], (expectedStart+1)*ps), true)

		slots, pageOffsets, gaps, err := slotPageWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "slots", slots, []uint64{expectedEnd}, ps)
		expectOffsets(t, "pages", pageOffsets, []uint64{expectedStart, expectedStart + 1}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 2)
	})
}

// vmpl_interval_add_page_end_test
func TestPagesAddedAtAnIntervalEndMoveTheEnd(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const expectedStart = 0
		expectedEnd := uint64(2)
		const size = pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		pages := getPages(2)

		// A page at the interval's end.
		expect(t, "added", addPage(t, list, pages[0], expectedEnd*ps), true)

		var pageOff uint64
		intervals, gaps, err := intervalPageWalk(list, 0, size*ps, &pageOff)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{expectedStart, expectedEnd - 1}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)
		expect(t, "page", pageOff, expectedEnd*ps)

		// Another at the new interval's end.
		expect(t, "added", addPage(t, list, pages[1], (expectedEnd-1)*ps), true)

		slots, pageOffsets, gaps, err := slotPageWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "slots", slots, []uint64{expectedStart}, ps)
		expectOffsets(t, "pages", pageOffsets, []uint64{expectedEnd - 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{expectedEnd + 1, size}, ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 2)
	})
}

// vmpl_interval_replace_slot_test
func TestAPageReplacesASinglePageInterval(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		const expectedInterval = 0
		const size = pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedInterval*ps, (expectedInterval+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		interval := uint64(size * ps)
		var gaps []uint64
		err := list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalSlot() {
					return errBadState
				}
				interval = off
				return nil
			},
			func(begin, end uint64) error {
				gaps = append(gaps, begin, end)
				return nil
			},
			0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "gaps", gaps, []uint64{expectedInterval + 1, size}, ps)
		expect(t, "interval", interval, expectedInterval*ps)

		// A page in the interval's slot.
		page := getPages(1)[0]
		expect(t, "added", addPage(t, list, page, expectedInterval*ps), true)

		pageOff := uint64(size * ps)
		gaps = nil
		err = list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsPage() {
					return errBadState
				}
				pageOff = off
				return nil
			},
			func(begin, end uint64) error {
				gaps = append(gaps, begin, end)
				return nil
			},
			0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "gaps", gaps, []uint64{expectedInterval + 1, size}, ps)
		expect(t, "page", pageOff, expectedInterval*ps)

		var freeList []*testPage
		list.RemoveAllContent(releasePages(&freeList))
		expect(t, "pages freed", len(freeList), 1)
	})
}

// contiguousRuns is the run callback of Zircon's interval run cases: it notes
// each run and refuses any that is not an interval.
func contiguousRuns(runs *[]uint64) func(begin, end uint64, isInterval bool) error {
	return func(begin, end uint64, isInterval bool) error {
		if !isInterval {
			return errBadState
		}
		*runs = append(*runs, begin, end)
		return nil
	}
}

// vmpl_interval_contig_full_test
func TestAnIntervalIsOneContiguousRun(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		var pages, contig []uint64
		err := list.ForEveryPageAndContiguousRunInRange(always,
			func(p *PageOrMarker[testPage], off uint64) error {
				if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
					return errBadState
				}
				if p.IsIntervalStart() {
					if len(pages)%2 == 1 {
						return errBadState
					}
				} else if p.IsIntervalEnd() {
					if len(pages)%2 == 0 {
						return errBadState
					}
				}
				pages = append(pages, off)
				return nil
			},
			contiguousRuns(&contig),
			0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "pages", pages, []uint64{expectedStart, expectedEnd}, ps)
		expectOffsets(t, "runs", contig, []uint64{expectedStart, expectedEnd + 1}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_contig_partial_test
func TestPartOfAnIntervalIsAContiguousRun(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		var page uint64
		var contig []uint64
		// Starting partway into the interval.
		err := list.ForEveryPageAndContiguousRunInRange(always,
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalEnd() {
					return errBadState
				}
				page = off
				return nil
			},
			contiguousRuns(&contig),
			(expectedStart+1)*ps, size*ps)
		expectNoError(t, "walk", err)

		// Only the end was visited.
		expect(t, "page", page, expectedEnd*ps)
		expectOffsets(t, "runs", contig, []uint64{expectedStart + 1, expectedEnd + 1}, ps)

		contig = nil
		// Ending partway into the interval.
		err = list.ForEveryPageAndContiguousRunInRange(always,
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalStart() {
					return errBadState
				}
				page = off
				return nil
			},
			contiguousRuns(&contig),
			0, (expectedEnd-1)*ps)
		expectNoError(t, "walk", err)

		// Only the start was visited.
		expect(t, "page", page, expectedStart*ps)
		expectOffsets(t, "runs", contig, []uint64{expectedStart, expectedEnd - 1}, ps)

		contig = nil
		// Starting and ending partway into the interval.
		err = list.ForEveryPageAndContiguousRunInRange(always,
			// No slot is visited.
			func(*PageOrMarker[testPage], uint64) error { return errBadState },
			contiguousRuns(&contig),
			(expectedStart+1)*ps, (expectedEnd-1)*ps)
		expectNoError(t, "walk", err)

		// The range asked for is a run, though neither end was visited.
		expectOffsets(t, "runs", contig, []uint64{expectedStart + 1, expectedEnd - 1}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_contig_compare_test
func TestAnIntervalIsNoRunUnlessBothEndsCompare(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// The start fails the comparison, so there is no run.
		noRun := func(uint64, uint64, bool) error { return errInvalidArgs }

		var page uint64
		// Starting partway into the interval.
		err := list.ForEveryPageAndContiguousRunInRange(
			// The start fails the comparison.
			func(p *PageOrMarker[testPage], _ uint64) bool { return !p.IsIntervalStart() },
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalEnd() {
					return errBadState
				}
				page = off
				return nil
			},
			noRun,
			(expectedStart+1)*ps, size*ps)
		expectNoError(t, "walk", err)

		// Only the end was visited.
		expect(t, "page", page, expectedEnd*ps)

		// Ending partway into the interval.
		err = list.ForEveryPageAndContiguousRunInRange(
			// The end fails the comparison.
			func(p *PageOrMarker[testPage], _ uint64) bool { return !p.IsIntervalEnd() },
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalStart() {
					return errBadState
				}
				page = off
				return nil
			},
			noRun,
			0, (expectedEnd-1)*ps)
		expectNoError(t, "walk", err)

		// Only the start was visited.
		expect(t, "page", page, expectedStart*ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_populate_full_test
func TestPopulatingAWholeIntervalMakesEverySlot(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across five nodes, the middle ones unpopulated.
		const expectedStart, expectedEnd = 1, 4 * pageFanOut
		const size = 5 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Populate all of it.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(expectedStart*ps, (expectedEnd+1)*ps))

		nextOff := uint64(expectedStart * ps)
		var gaps []uint64
		// Only interval Slots.
		err := list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if !p.IsIntervalSlot() {
					return errBadState
				}
				if !p.IsZeroIntervalDirty() {
					return errBadState
				}
				if off != nextOff {
					return errOutOfRange
				}
				nextOff += ps
				return nil
			},
			func(begin, end uint64) error {
				gaps = append(gaps, begin, end)
				return nil
			},
			0, size*ps)
		expectNoError(t, "walk", err)
		expect(t, "next offset", nextOff, (expectedEnd+1)*ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// populateWalk is the walk of Zircon's populate cases: every slot is a Dirty
// interval sentinel. Starts and ends alternate and are noted, and the Slots
// must run on from slot, which it returns past the last one. Gaps are noted.
func populateWalk(list *PageList[testPage], from, to, slot, ps uint64) (intervals, gaps []uint64, next uint64,
	err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !p.IsInterval() {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() || p.IsIntervalEnd() {
				if p.IsIntervalStart() && len(intervals)%2 == 1 {
					return errBadState
				}
				if p.IsIntervalEnd() && len(intervals)%2 == 0 {
					return errBadState
				}
				intervals = append(intervals, off)
				return nil
			}
			if off != slot {
				return errBadState
			}
			slot += ps
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return intervals, gaps, slot, err
}

// vmpl_interval_populate_partial_test
func TestPopulatingTheMiddleOfAnIntervalSplitsIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Populate some slots in the middle.
		const slotStart = expectedStart + 2
		const slotEnd = expectedEnd - 2
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(slotStart*ps, (slotEnd+1)*ps))

		// Slots where it was populated.
		intervals, gaps, slot, err := populateWalk(list, 0, size*ps, slotStart*ps, ps)
		expectNoError(t, "walk", err)
		expect(t, "slots end", slot, (slotEnd+1)*ps)
		expectOffsets(t, "intervals", intervals, []uint64{expectedStart, slotStart - 1, slotEnd + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_populate_start_test
func TestPopulatingFromAnIntervalStartMovesTheStart(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Populate some slots from the start.
		const slotStart = expectedStart
		const slotEnd = expectedEnd - 2
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(slotStart*ps, (slotEnd+1)*ps))

		// Slots where it was populated.
		intervals, gaps, slot, err := populateWalk(list, 0, size*ps, slotStart*ps, ps)
		expectNoError(t, "walk", err)
		expect(t, "slots end", slot, (slotEnd+1)*ps)
		expectOffsets(t, "intervals", intervals, []uint64{slotEnd + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_populate_end_test
func TestPopulatingToAnIntervalEndMovesTheEnd(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Populate some slots up to the end.
		const slotStart = expectedStart + 2
		const slotEnd = expectedEnd
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(slotStart*ps, (slotEnd+1)*ps))

		// Slots where it was populated.
		intervals, gaps, slot, err := populateWalk(list, 0, size*ps, slotStart*ps, ps)
		expectNoError(t, "walk", err)
		expect(t, "slots end", slot, (slotEnd+1)*ps)
		expectOffsets(t, "intervals", intervals, []uint64{expectedStart, slotStart - 1}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_populate_slot_test
func TestPopulatingOneSlotIsUndoneByReturningIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Populate one slot.
		const singleSlot = expectedEnd - 3
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(singleSlot*ps, (singleSlot+1)*ps))

		// One Slot.
		singleSlotWalk := func() (intervals, gaps []uint64, err error) {
			err = list.ForEveryPageAndGapInRange(
				func(p *PageOrMarker[testPage], off uint64) error {
					if !p.IsInterval() {
						return errBadState
					}
					if !p.IsZeroIntervalDirty() {
						return errBadState
					}
					if p.IsIntervalStart() || p.IsIntervalEnd() {
						if p.IsIntervalStart() && len(intervals)%2 == 1 {
							return errBadState
						}
						if p.IsIntervalEnd() && len(intervals)%2 == 0 {
							return errBadState
						}
						intervals = append(intervals, off)
						return nil
					}
					if off != singleSlot*ps {
						return errBadState
					}
					return nil
				},
				func(begin, end uint64) error {
					gaps = append(gaps, begin, end)
					return nil
				},
				0, size*ps)
			return intervals, gaps, err
		}
		intervals, gaps, err := singleSlotWalk()
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals,
			[]uint64{expectedStart, singleSlot - 1, singleSlot + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		// Populating over a Slot does nothing.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(singleSlot*ps, (singleSlot+1)*ps))
		intervals, gaps, err = singleSlotWalk()
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals,
			[]uint64{expectedStart, singleSlot - 1, singleSlot + 1, expectedEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		// Returning the Slot restores the interval.
		list.ReturnIntervalSlot(singleSlot * ps)
		gaps = nil
		err = list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
					return errBadState
				}
				if !p.IsZeroIntervalDirty() {
					return errBadState
				}
				if p.IsIntervalStart() && off != expectedStart*ps {
					return errBadState
				}
				if p.IsIntervalEnd() && off != expectedEnd*ps {
					return errBadState
				}
				return nil
			},
			func(begin, end uint64) error {
				gaps = append(gaps, begin, end)
				return nil
			},
			0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "gaps", gaps, []uint64{0, expectedStart, expectedEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_overwrite_full_test
func TestOverwritingAWholeIntervalChangesItsState(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const expectedStart, expectedEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(expectedStart*ps, (expectedEnd+1)*ps, IntervalDirty))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// An Untracked interval over the Dirty one.
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(expectedStart*ps, expectedEnd*ps,
			expectedStart*ps, expectedEnd*ps, IntervalUntracked))

		// The start and end stay, in the new state.
		err := list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if p.IsIntervalStart() && off == expectedStart*ps {
					if !p.IsZeroIntervalUntracked() {
						return errBadState
					}
					return nil
				}
				if p.IsIntervalEnd() && off == expectedEnd*ps {
					if !p.IsZeroIntervalUntracked() {
						return errBadState
					}
					return nil
				}
				return errBadState
			},
			func(uint64, uint64) error { return errBadState },
			expectedStart*ps, (expectedEnd+1)*ps)
		expectNoError(t, "walk", err)

		list.RemoveAllContent(dropContent)
	})
}

// stateWalk is the walk of Zircon's overwrite cases: only interval starts and
// ends, alternating, each noted in pages with its dirty state, and no gap.
func stateWalk(list *PageList[testPage], from, to, ps uint64) (intervals []uint64, states []IntervalDirtyState,
	err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if p.IsInterval() {
				if (p.IsIntervalStart() && len(intervals)%2 == 0) || (p.IsIntervalEnd() && len(intervals)%2 == 1) {
					intervals = append(intervals, off/ps)
					states = append(states, p.GetZeroIntervalDirtyState())
					return nil
				}
			}
			return errBadState
		},
		func(uint64, uint64) error { return errBadState },
		from, to)
	return intervals, states, err
}

func expectStates(t *testing.T, intervals []uint64, states []IntervalDirtyState, wantIntervals []uint64,
	wantStates []IntervalDirtyState) {
	t.Helper()
	if !slices.Equal(intervals, wantIntervals) {
		t.Errorf("intervals %v, want %v", intervals, wantIntervals)
	}
	if !slices.Equal(states, wantStates) {
		t.Errorf("states %v, want %v", states, wantStates)
	}
}

// vmpl_interval_overwrite_start_test
func TestOverwritingAnIntervalStartBreaksItOff(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const oldStart, oldEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(oldStart*ps, (oldEnd+1)*ps, IntervalUntracked))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// The start of the Untracked interval breaks off as a Dirty one.
		const newEnd = oldEnd - 5
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(oldStart*ps, noOffset, oldStart*ps, newEnd*ps,
			IntervalDirty))

		intervals, states, err := stateWalk(list, oldStart*ps, (oldEnd+1)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{oldStart, newEnd, newEnd + 1, oldEnd},
			[]IntervalDirtyState{IntervalDirty, IntervalDirty, IntervalUntracked, IntervalUntracked})

		list.RemoveAllContent(dropContent)
	})
}

// noOffset is UINT64_MAX, which OverwriteZeroInterval takes for an old start
// or end not given.
const noOffset = ^uint64(0)

// vmpl_interval_overwrite_end_test
func TestOverwritingAnIntervalEndBreaksItOff(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const oldStart, oldEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(oldStart*ps, (oldEnd+1)*ps, IntervalUntracked))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// The end of the Untracked interval breaks off as a Dirty one.
		const newStart = oldStart + 5
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(noOffset, oldEnd*ps, newStart*ps, oldEnd*ps,
			IntervalDirty))

		intervals, states, err := stateWalk(list, oldStart*ps, (oldEnd+1)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{oldStart, newStart - 1, newStart, oldEnd},
			[]IntervalDirtyState{IntervalUntracked, IntervalUntracked, IntervalDirty, IntervalDirty})

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_overwrite_slot_test
func TestOverwritingASlotChangesItsState(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// An interval of one slot.
		const expectedSlot = 1
		mustNotFail(t, "add", list.AddZeroInterval(expectedSlot*ps, (expectedSlot+1)*ps, IntervalDirty))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(expectedSlot*ps, (expectedSlot+1)*ps), true)

		// An Untracked interval over the Dirty one.
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(expectedSlot*ps, expectedSlot*ps,
			expectedSlot*ps, expectedSlot*ps, IntervalUntracked))

		// The slot stays, in the new state.
		err := list.ForEveryPageAndGapInRange(
			func(p *PageOrMarker[testPage], off uint64) error {
				if p.IsIntervalSlot() && off == expectedSlot*ps {
					if !p.IsZeroIntervalUntracked() {
						return errBadState
					}
					return nil
				}
				return errBadState
			},
			func(uint64, uint64) error { return errBadState },
			expectedSlot*ps, (expectedSlot+1)*ps)
		expectNoError(t, "walk", err)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_overwrite_merge_left_test
func TestAnOverwrittenStartMergesWithTheIntervalOnItsLeft(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Two intervals side by side in different states.
		const leftStart, leftEnd = 1, 4
		const rightStart, rightEnd = leftEnd + 1, 10
		mustNotFail(t, "add left", list.AddZeroInterval(leftStart*ps, (leftEnd+1)*ps, IntervalDirty))
		mustNotFail(t, "add right", list.AddZeroInterval(rightStart*ps, (rightEnd+1)*ps, IntervalUntracked))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(leftStart*ps, (rightEnd+1)*ps), true)

		// The start of the right interval breaks off and joins the left.
		const newEnd = rightStart + 2
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(rightStart*ps, noOffset, rightStart*ps, newEnd*ps,
			IntervalDirty))

		intervals, states, err := stateWalk(list, leftStart*ps, (rightEnd+1)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{leftStart, newEnd, newEnd + 1, rightEnd},
			[]IntervalDirtyState{IntervalDirty, IntervalDirty, IntervalUntracked, IntervalUntracked})

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_overwrite_merge_right_test
func TestAnOverwrittenEndMergesWithTheIntervalOnItsRight(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Two intervals side by side in different states.
		const leftStart, leftEnd = 1, 6
		const rightStart, rightEnd = leftEnd + 1, 10
		mustNotFail(t, "add left", list.AddZeroInterval(leftStart*ps, (leftEnd+1)*ps, IntervalDirty))
		mustNotFail(t, "add right", list.AddZeroInterval(rightStart*ps, (rightEnd+1)*ps, IntervalUntracked))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(leftStart*ps, (rightEnd+1)*ps), true)

		// The end of the left interval breaks off and joins the right.
		const newStart = leftEnd - 2
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(noOffset, leftEnd*ps, newStart*ps, leftEnd*ps,
			IntervalUntracked))

		intervals, states, err := stateWalk(list, leftStart*ps, (rightEnd+1)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{leftStart, newStart - 1, newStart, rightEnd},
			[]IntervalDirtyState{IntervalDirty, IntervalDirty, IntervalUntracked, IntervalUntracked})

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_overwrite_merge_slots_test
func TestAnOverwrittenSlotMergesWithSlotsOnBothSides(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Three Slots in alternating states.
		const left, mid, right = 3, 4, 5
		mustNotFail(t, "add left", list.AddZeroInterval(left*ps, (left+1)*ps, IntervalUntracked))
		mustNotFail(t, "add mid", list.AddZeroInterval(mid*ps, (mid+1)*ps, IntervalDirty))
		mustNotFail(t, "add right", list.AddZeroInterval(right*ps, (right+1)*ps, IntervalUntracked))
		expect(t, "any", list.AnyPagesOrIntervalsInRange(left*ps, (right+1)*ps), true)

		// Overwrite the middle so it joins both sides.
		mustNotFail(t, "overwrite", list.OverwriteZeroInterval(mid*ps, mid*ps, mid*ps, mid*ps, IntervalUntracked))

		intervals, states, err := stateWalk(list, left*ps, (right+1)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{left, right},
			[]IntervalDirtyState{IntervalUntracked, IntervalUntracked})

		list.RemoveAllContent(dropContent)
	})
}

// clipWalk is the walk of Zircon's clip cases: only Dirty interval starts and
// ends, alternating and noted, and gaps noted.
func clipWalk(list *PageList[testPage], from, to uint64) (intervals, gaps []uint64, err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !(p.IsIntervalStart() || p.IsIntervalEnd()) {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if p.IsIntervalStart() && len(intervals)%2 == 1 {
				return errBadState
			}
			if p.IsIntervalEnd() && len(intervals)%2 == 0 {
				return errBadState
			}
			intervals = append(intervals, off)
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return intervals, gaps, err
}

// slotWalk is the walk of Zircon's clip cases once one Slot is left: that
// Dirty Slot at slot, and gaps noted.
func slotWalk(list *PageList[testPage], from, to, slot uint64) (gaps []uint64, err error) {
	err = list.ForEveryPageAndGapInRange(
		func(p *PageOrMarker[testPage], off uint64) error {
			if !p.IsIntervalSlot() {
				return errBadState
			}
			if !p.IsZeroIntervalDirty() {
				return errBadState
			}
			if off != slot {
				return errBadState
			}
			return nil
		},
		func(begin, end uint64) error {
			gaps = append(gaps, begin, end)
			return nil
		},
		from, to)
	return gaps, err
}

// vmpl_interval_clip_start_test
func TestClippingAnIntervalStartDownToASlot(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const oldStart, oldEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(oldStart*ps, (oldEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Clip the start, leaving several pages.
		const newStart = oldEnd - 3
		mustNotFail(t, "clip", list.ClipIntervalStart(oldStart*ps, (newStart-oldStart)*ps))

		intervals, gaps, err := clipWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{newStart, oldEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, newStart, oldEnd + 1, size}, ps)

		// Clip it again, leaving one Slot.
		mustNotFail(t, "clip", list.ClipIntervalStart(newStart*ps, (oldEnd-newStart)*ps))
		gaps, err = slotWalk(list, 0, size*ps, oldEnd*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "gaps", gaps, []uint64{0, oldEnd, oldEnd + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_interval_clip_end_test
func TestClippingAnIntervalEndDownToASlot(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		list := NewPageList[testPage](ps)

		// Across three nodes, the middle one unpopulated.
		const oldStart, oldEnd = 1, 2 * pageFanOut
		const size = 3 * pageFanOut
		mustNotFail(t, "add", list.AddZeroInterval(oldStart*ps, (oldEnd+1)*ps, IntervalDirty))

		expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size*ps), true)

		// Clip the end, leaving several pages.
		const newEnd = oldStart + 3
		mustNotFail(t, "clip", list.ClipIntervalEnd(oldEnd*ps, (oldEnd-newEnd)*ps))

		intervals, gaps, err := clipWalk(list, 0, size*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{oldStart, newEnd}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, oldStart, newEnd + 1, size}, ps)

		// Clip it again, leaving one Slot.
		mustNotFail(t, "clip", list.ClipIntervalEnd(newEnd*ps, (newEnd-oldStart)*ps))
		gaps, err = slotWalk(list, 0, size*ps, oldStart*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "gaps", gaps, []uint64{0, oldStart, oldStart + 1, size}, ps)

		list.RemoveAllContent(dropContent)
	})
}

// awaitingCleanInterval is the start of Zircon's AwaitingClean cases: a Dirty
// interval across three nodes, the middle one unpopulated, from start to end
// inclusive, with an AwaitingClean length of length.
func awaitingCleanInterval(t *testing.T, ps, length uint64) (list *PageList[testPage], start, end uint64) {
	t.Helper()
	list = NewPageList[testPage](ps)
	start, end = ps, 2*pageFanOut*ps
	size := 3 * pageFanOut * ps
	mustNotFail(t, "add", list.AddZeroInterval(start, end+ps, IntervalDirty))

	expect(t, "any", list.AnyPagesOrIntervalsInRange(0, size), true)

	list.LookupMutable(start).SetZeroIntervalAwaitingCleanLength(length)
	expect(t, "length at start", list.Lookup(start).GetZeroIntervalAwaitingCleanLength(), length)
	return list, start, end
}

// awaitingCleanLength is the AwaitingClean length of the sentinel at offset.
func awaitingCleanLength(list *PageList[testPage], offset uint64) uint64 {
	return list.Lookup(offset).GetZeroIntervalAwaitingCleanLength()
}

// onlyTheInterval checks the list holds the interval [start, end] and nothing
// else.
func onlyTheInterval(list *PageList[testPage], start, end uint64) error {
	return list.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
		if p.IsIntervalStart() {
			if off != start {
				return errBadState
			}
			return nil
		}
		if p.IsIntervalEnd() {
			if off != end {
				return errBadState
			}
			return nil
		}
		return errBadState
	})
}

// vmpl_awaiting_clean_split_test
func TestSplittingAnIntervalKeepsItsAwaitingCleanLengthAtTheStart(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		expectedLen := 2 * pageFanOut * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Split it in the middle.
		mid := end - 2*ps
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(mid, mid+ps))

		// The length is unchanged.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expect(t, "at mid", awaitingCleanLength(list, mid), 0)
		expect(t, "after mid", awaitingCleanLength(list, mid+ps), 0)

		// Split it at the end.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(end, end+ps))

		// The length is unchanged.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expect(t, "at mid", awaitingCleanLength(list, mid), 0)
		expect(t, "after mid", awaitingCleanLength(list, mid+ps), 0)
		expect(t, "at end", awaitingCleanLength(list, end), 0)

		// Split it at the start.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start, start+ps))

		// The length moves to the new start.
		expect(t, "at start", awaitingCleanLength(list, start), ps)
		expect(t, "at new start", awaitingCleanLength(list, start+ps), expectedLen-ps)
		expect(t, "at mid", awaitingCleanLength(list, mid), 0)
		expect(t, "after mid", awaitingCleanLength(list, mid+ps), 0)
		expect(t, "at end", awaitingCleanLength(list, end), 0)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_clip_test
func TestClippingAnIntervalStartClipsItsAwaitingCleanLength(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		expectedLen := 2 * pageFanOut * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Clip the end.
		mustNotFail(t, "clip end", list.ClipIntervalEnd(end, 2*ps))

		// The length is unchanged.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)

		// Clip the start.
		mustNotFail(t, "clip start", list.ClipIntervalStart(start, 2*ps))

		// The length is clipped too.
		expect(t, "at new start", awaitingCleanLength(list, start+2*ps), expectedLen-2*ps)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_return_slot_test
func TestReturningASlotRestoresTheAwaitingCleanLength(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		expectedLen := 2 * pageFanOut * ps
		list, start, _ := awaitingCleanInterval(t, ps, expectedLen)

		// Split it at the start.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start, start+ps))

		// The length moves to the new start.
		expect(t, "at start", awaitingCleanLength(list, start), ps)
		expect(t, "at new start", awaitingCleanLength(list, start+ps), expectedLen-ps)

		// Give the Slot back.
		list.ReturnIntervalSlot(start)

		// The length is restored.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_return_slots_test
func TestReturningSlotsSplitOneByOneRestoresTheAwaitingCleanLength(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		expectedLen := 2 * pageFanOut * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Split the start three times, so every Slot made has a length.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start, start+ps))
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start+ps, start+2*ps))
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start+2*ps, start+3*ps))

		expect(t, "slot 0", awaitingCleanLength(list, start), ps)
		expect(t, "slot 1", awaitingCleanLength(list, start+ps), ps)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), ps)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), expectedLen-3*ps)

		// Returning the first Slot joins the first two into an interval.
		list.ReturnIntervalSlot(start)
		expect(t, "start", list.Lookup(start).IsIntervalStart(), true)
		expect(t, "end", list.Lookup(start+ps).IsIntervalEnd(), true)

		expect(t, "slots 0 and 1", awaitingCleanLength(list, start), 2*ps)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), ps)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), expectedLen-3*ps)

		// Returning the third joins everything back as it was.
		list.ReturnIntervalSlot(start + 2*ps)
		// The length is restored.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_populate_slots_test
func TestReturningPopulatedSlotsRestoresTheAwaitingCleanLength(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		expectedLen := 2 * pageFanOut * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Populate some slots at the start.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start, start+3*ps))

		expect(t, "slot 0", awaitingCleanLength(list, start), ps)
		expect(t, "slot 1", awaitingCleanLength(list, start+ps), ps)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), ps)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), expectedLen-3*ps)

		// Returning the first Slot joins the first two into an interval.
		list.ReturnIntervalSlot(start)
		expect(t, "start", list.Lookup(start).IsIntervalStart(), true)
		expect(t, "end", list.Lookup(start+ps).IsIntervalEnd(), true)

		expect(t, "slots 0 and 1", awaitingCleanLength(list, start), 2*ps)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), ps)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), expectedLen-3*ps)

		// Returning the third joins everything back as it was.
		list.ReturnIntervalSlot(start + 2*ps)
		// The length is restored.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_intersecting_test
func TestPopulatingAcrossTheAwaitingCleanLengthSplitsIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		// A length over part of the interval.
		expectedLen := 2 * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Populate slots at the start, some inside the length and some
		// outside.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start, start+3*ps))

		expect(t, "slot 0", awaitingCleanLength(list, start), ps)
		expect(t, "slot 1", awaitingCleanLength(list, start+ps), ps)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), 0)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), 0)

		// Returning the first Slot joins the first two into an interval.
		list.ReturnIntervalSlot(start)
		expect(t, "start", list.Lookup(start).IsIntervalStart(), true)
		expect(t, "end", list.Lookup(start+ps).IsIntervalEnd(), true)

		expect(t, "slots 0 and 1", awaitingCleanLength(list, start), expectedLen)
		expect(t, "slot 2", awaitingCleanLength(list, start+2*ps), 0)
		expect(t, "the rest", awaitingCleanLength(list, start+3*ps), 0)

		// Returning the third joins everything back as it was.
		list.ReturnIntervalSlot(start + 2*ps)
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		// Populate a slot again, partway into the interval.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start+ps, start+2*ps))

		// The start's length is unchanged.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		// The Slot and the rest of the interval have none.
		expect(t, "slot", awaitingCleanLength(list, start+ps), 0)
		expect(t, "the rest", awaitingCleanLength(list, start+2*ps), 0)

		// Returning the Slot restores the interval.
		list.ReturnIntervalSlot(start + ps)
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		list.RemoveAllContent(dropContent)
	})
}

// vmpl_awaiting_clean_non_intersecting_test
func TestPopulatingPastTheAwaitingCleanLengthLeavesIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		// A length over part of the interval.
		expectedLen := 2 * ps
		list, start, end := awaitingCleanInterval(t, ps, expectedLen)

		// Populate slots past the length.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(start+expectedLen, start+expectedLen+3*ps))

		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expect(t, "slot 0", awaitingCleanLength(list, start+expectedLen), 0)
		expect(t, "slot 1", awaitingCleanLength(list, start+expectedLen+ps), 0)
		expect(t, "slot 2", awaitingCleanLength(list, start+expectedLen+2*ps), 0)
		expect(t, "the rest", awaitingCleanLength(list, start+expectedLen+3*ps), 0)

		// Returning the first Slot joins it back into the interval.
		list.ReturnIntervalSlot(start + expectedLen)
		expect(t, "end", list.Lookup(start+expectedLen+ps).IsIntervalEnd(), true)

		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expect(t, "slot 2", awaitingCleanLength(list, start+expectedLen+2*ps), 0)
		expect(t, "the rest", awaitingCleanLength(list, start+expectedLen+3*ps), 0)

		// Returning the third joins everything back as it was.
		list.ReturnIntervalSlot(start + expectedLen + 2*ps)
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		// Populate a slot again, at the end.
		mustNotFail(t, "populate", list.PopulateSlotsInInterval(end, end+ps))

		// The start's length is unchanged.
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		// The Slot has none.
		expect(t, "at end", awaitingCleanLength(list, end), 0)

		// Returning the Slot restores the interval.
		list.ReturnIntervalSlot(end)
		expect(t, "at start", awaitingCleanLength(list, start), expectedLen)
		expectNoError(t, "the interval", onlyTheInterval(list, start, end))

		list.RemoveAllContent(dropContent)
	})
}

// The cases below are not Zircon's. Zircon reaches these parts of the page
// list through its VmCowPages tests, which port in a later step, or not at
// all; mutation testing found them untested here. They test the page list
// directly.

// A marker's share count goes up and down through the slot and through a
// reference to it, and setting it replaces it.
func TestAMarkerCountsItsSharers(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		slot, _ := pl.LookupOrAllocate(ps, NoIntervals)
		slot.Set(MarkerWithShareCount[testPage](3))
		expect(t, "a marker", slot.IsMarker(), true)
		expect(t, "count", slot.GetMarkerShareCount(), 3)

		slot.IncrementMarkerShareCount()
		expect(t, "count", slot.GetMarkerShareCount(), 4)
		slot.DecrementMarkerShareCount()
		slot.DecrementMarkerShareCount()
		expect(t, "count", slot.GetMarkerShareCount(), 2)
		slot.SetMarkerShareCount(7)
		expect(t, "count", slot.GetMarkerShareCount(), 7)

		ref := pl.LookupMutable(ps)
		expect(t, "valid", ref.Valid(), true)
		ref.IncrementMarkerShareCount()
		expect(t, "count", ref.GetMarkerShareCount(), 8)
		ref.DecrementMarkerShareCount()
		expect(t, "count", ref.GetMarkerShareCount(), 7)
		expect(t, "still a marker", pl.Lookup(ps).IsMarker(), true)

		// A marker shared by nobody cannot be shared less.
		slot.SetMarkerShareCount(0)
		expectPanic(t, "decrement from zero", slot.DecrementMarkerShareCount)

		expect(t, "no slot", pl.LookupMutable(pageFanOut*ps).Valid(), false)
		pl.RemoveAllContent(dropContent)
	})
}

// A slot's content changes kind in place: a reference for a page, a page for
// a reference, and one reference for another, each handing back what it held.
func TestASlotSwapsOneKindOfContentForAnother(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(2)
		ref1 := MakeReferenceValue(testReference(1))
		ref2 := MakeReferenceValue(testReference(2))

		slot, _ := pl.LookupOrAllocate(0, NoIntervals)
		slot.Set(Reference[testPage](ref1))
		expect(t, "reference", slot.Reference(), ref1)

		expect(t, "reference given back", slot.SwapReferenceForPage(pages[0]), ref1)
		expect(t, "page", slot.Page(), pages[0])
		expect(t, "page given back", slot.SwapPageForReference(ref2), pages[0])
		expect(t, "reference", slot.Reference(), ref2)
		expect(t, "reference given back", slot.SwapReferenceForReference(ref1), ref2)
		expect(t, "reference", slot.Reference(), ref1)

		ref := pl.LookupMutable(0)
		expect(t, "reference given back", ref.SwapReferenceForPage(pages[1]), ref1)
		expect(t, "page", ref.Get().Page(), pages[1])
		expect(t, "page given back", ref.SwapPageForReference(ref2), pages[1])
		expect(t, "reference given back", ref.SwapReferenceForReference(ref1), ref2)
		expect(t, "content given back", ref.SwapContent(Page(pages[0])), Reference[testPage](ref1))
		expect(t, "page", pl.Lookup(0).Page(), pages[0])
		expectPanic(t, "swapping in nothing", func() { ref.SwapContent(Empty[testPage]()) })

		expect(t, "content given back", slot.Swap(Marker[testPage]()), Page(pages[0]))
		expect(t, "marker", pl.Lookup(0).IsMarker(), true)

		// A slot holding a page cannot be overwritten, which would lose it.
		slot.Set(Page(pages[1]))
		expectPanic(t, "overwriting a page", func() { slot.Set(Marker[testPage]()) })
		expect(t, "page", pl.RemoveContent(0).Page(), pages[1])
	})
}

// ParentContent is content in the list but not owned by it.
func TestParentContentIsInTheListButNotOwnedByIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		slot, _ := pl.LookupOrAllocate(2*ps, NoIntervals)
		slot.Set(ParentContent[testPage]())
		expect(t, "parent content", pl.Lookup(2*ps).IsParentContent(), true)
		expect(t, "a marker", pl.Lookup(2*ps).IsMarker(), false)

		expect(t, "any", pl.AnyPagesOrIntervalsInRange(0, 4*ps), true)
		expect(t, "any owned", pl.AnyOwnedPagesOrIntervalsInRange(0, 4*ps), false)

		// A marker beside it is owned.
		slot, _ = pl.LookupOrAllocate(3*ps, NoIntervals)
		slot.Set(Marker[testPage]())
		expect(t, "any owned", pl.AnyOwnedPagesOrIntervalsInRange(0, 4*ps), true)
		expect(t, "any owned before it", pl.AnyOwnedPagesOrIntervalsInRange(0, 3*ps), false)

		// So is an interval, even where no slot of it is in the range.
		mustNotFail(t, "add", pl.AddZeroInterval(pageFanOut*ps, 3*pageFanOut*ps, IntervalUntracked))
		expect(t, "any owned in the interval",
			pl.AnyOwnedPagesOrIntervalsInRange((pageFanOut+2)*ps, (pageFanOut+4)*ps), true)
		expect(t, "any owned past it", pl.AnyOwnedPagesOrIntervalsInRange(3*pageFanOut*ps, 4*pageFanOut*ps), false)
		pl.RemoveAllContent(dropContent)
	})
}

// The mutable walks hand out references through which content changes kind
// where it is.
func TestMutableWalksChangeContentWhereItIs(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(3)
		for i, off := range []uint64{0, 2 * ps, pageFanOut * ps} {
			expect(t, "added", addReference(t, pl, MakeReferenceValue(testReference(uint32(i))), off), true)
		}

		var seen []uint64
		err := pl.ForEveryPageMutable(func(p PageOrMarkerRef[testPage], off uint64) error {
			seen = append(seen, off)
			p.SwapReferenceForPage(pages[len(seen)-1])
			return nil
		})
		expectNoError(t, "every page", err)
		expectOffsets(t, "every page", seen, []uint64{0, 2, pageFanOut}, ps)
		expect(t, "page", pl.Lookup(2*ps).Page(), pages[1])

		seen = nil
		err = pl.ForEveryPageInRangeMutable(func(p PageOrMarkerRef[testPage], off uint64) error {
			seen = append(seen, off)
			p.SwapPageForReference(MakeReferenceValue(testReference(9)))
			return ErrStop
		}, ps, pageFanOut*ps+ps)
		expectNoError(t, "a range", err)
		expectOffsets(t, "a range", seen, []uint64{2}, ps)
		expect(t, "reference", pl.Lookup(2*ps).Reference().Value(), testReference(9))

		var gaps []uint64
		seen = nil
		err = pl.ForEveryPageAndGapInRangeMutable(func(p PageOrMarkerRef[testPage], off uint64) error {
			seen = append(seen, off)
			if p.Get().IsReference() {
				p.SwapReferenceForPage(pages[1])
			}
			return nil
		}, func(start, end uint64) error {
			gaps = append(gaps, start, end)
			return nil
		}, 0, 2*pageFanOut*ps)
		expectNoError(t, "pages and gaps", err)
		expectOffsets(t, "pages", seen, []uint64{0, 2, pageFanOut}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{1, 2, 3, pageFanOut, pageFanOut + 1, 2 * pageFanOut}, ps)
		expect(t, "page", pl.Lookup(2*ps).Page(), pages[1])

		var freed []*testPage
		pl.RemoveAllContent(releasePages(&freed))
		expect(t, "pages freed", len(freed), 3)
	})
}

// An empty range holds nothing to walk, even inside a node.
func TestAnEmptyRangeHoldsNothingToWalk(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		expect(t, "added", addMarker(t, pl, ps), true)
		called := false
		err := pl.ForEveryPageInRange(func(*PageOrMarker[testPage], uint64) error {
			called = true
			return nil
		}, ps, ps)
		expectNoError(t, "walk", err)
		expect(t, "called", called, false)
		pl.RemoveAllContent(dropContent)
	})
}

// A batch inserter finds the slot LookupOrAllocate would, whether the offsets
// rise within a node, step to the next node, jump past the last, or go back.
func TestABatchInserterFindsTheSlotsLookupOrAllocateWould(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		// Nodes at 0 and 2, so the inserter meets one it holds, one it must
		// make between them, and one past the last.
		expect(t, "added", addMarker(t, pl, 0), true)
		expect(t, "added", addMarker(t, pl, 2*pageFanOut*ps), true)

		inserter := pl.NewBatchInserter()
		offsets := []uint64{
			ps, 2 * ps, // the node at 0
			pageFanOut * ps,                // a node to make before the node at 2
			2*pageFanOut*ps + ps,           // the node at 2, the next one
			4 * pageFanOut * ps,            // past the last
			4*pageFanOut*ps + ps,           // the node just made
			pageFanOut*ps + ps,             // back to a node before
			6 * pageFanOut * ps,            // past the last again
			3 * ps, 3*pageFanOut*ps + 2*ps, // back to the first, then a node to make after it
		}
		for _, off := range offsets {
			slot := inserter.LookupOrAllocate(off)
			if slot == nil {
				t.Fatalf("no slot at %#x", off)
			}
			slot.Set(Marker[testPage]())
			if pl.Lookup(off) != slot {
				t.Errorf("at %#x the inserter's slot is not the list's", off)
			}
		}
		var markers []uint64
		expectNoError(t, "walk", pl.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if p.IsMarker() {
				markers = append(markers, off)
			}
			return nil
		}))
		want := append([]uint64{0, 2 * pageFanOut * ps}, offsets...)
		slices.Sort(want)
		if !slices.Equal(markers, want) {
			t.Errorf("markers at %#x, want %#x", markers, want)
		}
		expect(t, "nodes", pl.HeapAllocationBytes(), 6*nodeBytes)

		// After the list changes behind it, Reset makes it safe again.
		pl.RemoveAllContent(dropContent)
		inserter.Reset()
		slot := inserter.LookupOrAllocate(5 * ps)
		slot.Set(Marker[testPage]())
		expect(t, "marker", pl.Lookup(5*ps).IsMarker(), true)
		pl.RemoveAllContent(dropContent)
	})
}

// nodeBytes is what one node takes: 16 slots of 16 bytes, and its entry in
// the tree.
const nodeBytes = pageFanOut*16 + 16

// A list's heap is its nodes.
func TestAListsHeapIsItsNodes(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		expect(t, "empty", pl.HeapAllocationBytes(), 0)
		for _, off := range []uint64{0, ps, pageFanOut * ps, 9 * pageFanOut * ps} {
			expect(t, "added", addMarker(t, pl, off), true)
		}
		expect(t, "three nodes", pl.HeapAllocationBytes(), 3*nodeBytes)
		pl.RemoveAllContent(dropContent)
	})
}

// A page replaced with a zero interval joins the intervals beside it in the
// same state, and is handed back.
func TestAPageReplacedWithAZeroIntervalJoinsItsNeighbours(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		page := getPages(1)[0]
		mustNotFail(t, "add", pl.AddZeroInterval(ps, pageFanOut*ps, IntervalDirty))
		mustNotFail(t, "add", pl.AddZeroInterval((pageFanOut+1)*ps, 2*pageFanOut*ps, IntervalDirty))
		expect(t, "added", addPage(t, pl, page, pageFanOut*ps), true)
		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(pageFanOut*ps), false)

		expect(t, "page given back", pl.ReplacePageWithZeroInterval(pageFanOut*ps, IntervalDirty), page)

		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(pageFanOut*ps), true)
		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(5*ps), true)
		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(0), false)
		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(2*pageFanOut*ps), false)
		expect(t, "in a zero interval", pl.IsOffsetInZeroInterval(9*pageFanOut*ps), false)
		expectNoError(t, "one interval", onlyTheInterval(pl, ps, (2*pageFanOut-1)*ps))
		expect(t, "dirty", pl.Lookup(ps).IsZeroIntervalDirty(), true)
		expect(t, "clean", pl.Lookup(ps).IsZeroIntervalClean(), false)

		// A start at the first slot of a node is in its interval.
		mustNotFail(t, "add", pl.AddZeroInterval(3*pageFanOut*ps, (3*pageFanOut+4)*ps, IntervalUntracked))
		expect(t, "at a start", pl.IsOffsetInZeroInterval(3*pageFanOut*ps), true)
		pl.RemoveAllContent(dropContent)
	})
}

// A cursor can be had at the first slot at or after an offset, and a walk
// from it ends where asked or where its callback stops it, reporting the stop.
func TestACursorRangeWalkStartsWhereTheCursorIs(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		for _, off := range []uint64{pageFanOut + 3, pageFanOut + 5, 3*pageFanOut + 1} {
			expect(t, "added", addMarker(t, pl, off*ps), true)
		}

		// Inside a node, the cursor is at the offset.
		cursor := pl.LookupNearestMutableCursor((pageFanOut + 1) * ps)
		expect(t, "offset", cursor.Offset(), (pageFanOut+1)*ps)
		expect(t, "empty", cursor.CurrentRef().Get().IsEmpty(), true)
		// Before a node, at its first slot.
		cursor = pl.LookupNearestMutableCursor(2 * ps)
		expect(t, "offset", cursor.Offset(), pageFanOut*ps)
		cursor = pl.LookupNearestMutableCursor((2*pageFanOut + 7) * ps)
		expect(t, "offset", cursor.Offset(), 3*pageFanOut*ps)
		// Past the last node, nowhere.
		cursor = pl.LookupNearestMutableCursor(4 * pageFanOut * ps)
		expect(t, "current", cursor.Current(), nil)
		expect(t, "ref", cursor.CurrentRef().Valid(), false)

		cursor = pl.LookupNearestMutableCursor(pageFanOut * ps)
		var seen []uint64
		collect := func(_ *PageOrMarker[testPage], off uint64) error {
			seen = append(seen, off)
			return nil
		}
		expectNoError(t, "walk", pl.ForEveryPageInCursorRange(collect, cursor, 4*pageFanOut*ps))
		expectOffsets(t, "walk", seen, []uint64{pageFanOut + 3, pageFanOut + 5, 3*pageFanOut + 1}, ps)

		seen = nil
		expectNoError(t, "walk", pl.ForEveryPageInCursorRange(collect, cursor, (pageFanOut+4)*ps))
		expectOffsets(t, "walk to an end", seen, []uint64{pageFanOut + 3}, ps)

		seen = nil
		expectNoError(t, "walk", pl.ForEveryPageInCursorRange(collect, cursor, pageFanOut*ps))
		expect(t, "an empty walk", len(seen), 0)

		// Unlike the other walks, it reports a stop.
		err := pl.ForEveryPageInCursorRange(func(*PageOrMarker[testPage], uint64) error { return ErrStop },
			cursor, 4*pageFanOut*ps)
		expect(t, "stopped", err, ErrStop)
		pl.RemoveAllContent(dropContent)
	})
}

// Removing pages and walking gaps frees the nodes the walk empties.
func TestRemovingPagesAndWalkingGapsFreesEmptiedNodes(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(3)
		for i, off := range []uint64{1, pageFanOut + 2, 2*pageFanOut + 3} {
			expect(t, "added", addPage(t, pl, pages[i], off*ps), true)
		}

		var removed []*testPage
		var gaps []uint64
		err := pl.RemovePagesAndIterateGaps(func(p *PageOrMarker[testPage], _ uint64) error {
			removed = append(removed, p.ReleasePage())
			return nil
		}, func(start, end uint64) error {
			gaps = append(gaps, start, end)
			return nil
		}, 0, 2*pageFanOut*ps)
		expectNoError(t, "remove", err)
		if !slices.Equal(removed, pages[:2]) {
			t.Errorf("removed %v, want the first two pages", removed)
		}
		expectOffsets(t, "gaps", gaps, []uint64{0, 1, 2, pageFanOut + 2, pageFanOut + 3, 2 * pageFanOut}, ps)
		expect(t, "nodes", pl.HeapAllocationBytes(), nodeBytes)
		expect(t, "page left", pl.Lookup((2*pageFanOut+3)*ps).Page(), pages[2])
		expect(t, "no node left", pl.Lookup(ps), nil)
		pl.RemoveAllContent(dropContent)
	})
}

// A splice list walked with gaps is processed up to where a callback stops: a
// page it stops on is processed, and a gap it stops on is not.
func TestASpliceListWalkResumesWhereItStopped(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(2)
		expect(t, "added", addPage(t, pl, pages[0], 2*ps), true)
		expect(t, "added", addPage(t, pl, pages[1], 5*ps), true)

		splice := NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(8 * ps)
		splice.AddPagesFrom(moveSlot, pl, 0)
		expect(t, "position", splice.Position(), 0)

		// MutatePages sees the pages and takes none.
		var mutated []uint64
		expectNoError(t, "mutate", splice.MutatePages(func(_ PageOrMarkerRef[testPage], off uint64) error {
			mutated = append(mutated, off)
			return nil
		}, 3*ps))
		expectOffsets(t, "mutated", mutated, []uint64{5}, ps)

		var taken []*testPage
		var gaps []uint64
		takePage := func(content PageOrMarker[testPage], _ uint64) error {
			taken = append(taken, content.Page())
			return ErrStop
		}
		stopAtGap := func(start, end uint64) error {
			gaps = append(gaps, start, end)
			return ErrStop
		}
		nextGap := func(start, end uint64) error {
			gaps = append(gaps, start, end)
			return nil
		}

		// The first gap stops the walk and is not processed.
		expectNoError(t, "walk", splice.RemovePagesAndIterateGaps(takePage, stopAtGap))
		expect(t, "position", splice.Position(), 0)
		// Now the gap passes and the page stops the walk, processed.
		expectNoError(t, "walk", splice.RemovePagesAndIterateGaps(takePage, nextGap))
		expect(t, "position", splice.Position(), 3*ps)
		expectNoError(t, "walk", splice.RemovePagesAndIterateGaps(takePage, nextGap))
		expect(t, "position", splice.Position(), 6*ps)
		expect(t, "processed", splice.IsProcessed(), false)
		expectNoError(t, "walk", splice.RemovePagesAndIterateGaps(takePage, nextGap))
		expect(t, "position", splice.Position(), 8*ps)
		expect(t, "processed", splice.IsProcessed(), true)

		if !slices.Equal(taken, pages) {
			t.Errorf("took %v, want both pages", taken)
		}
		expectOffsets(t, "gaps", gaps, []uint64{0, 2, 0, 2, 3, 5, 6, 8}, ps)

		// A failing callback fails the walk.
		splice = NewPageSpliceList(ps, &freedContent{})
		splice.Initialize(2 * ps)
		splice.Finalize()
		err := splice.RemovePagesAndIterateGaps(takePage, func(uint64, uint64) error { return errBadState })
		expect(t, "walk", err, errBadState)
		expect(t, "position", splice.Position(), 0)
	})
}

// Freeing a splice list gives back the pages and references it still holds,
// in any state, and nothing twice.
func TestFreeingASpliceListGivesBackWhatItStillHolds(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pages := getPages(1)
		ref := MakeReferenceValue(testReference(4))

		// Made but never given a range: nothing to give back.
		freed := &freedContent{}
		NewPageSpliceList(ps, freed).Free()
		expect(t, "pages freed", len(freed.pages), 0)

		// Initialized and filled but not finalized.
		splice := NewPageSpliceList(ps, freed)
		splice.Initialize(4 * ps)
		expect(t, "initialized", splice.IsInitialized(), true)
		expectNoError(t, "insert", splice.Insert(0, Page(pages[0])))
		expectNoError(t, "insert", splice.Insert(ps, Reference[testPage](ref)))
		expectNoError(t, "insert", splice.Insert(2*ps, Marker[testPage]()))
		expect(t, "empty", splice.IsEmpty(), false)
		splice.Free()
		expect(t, "processed", splice.IsProcessed(), true)
		if !slices.Equal(freed.pages, pages) || !slices.Equal(freed.refs, []ReferenceValue{ref}) {
			t.Errorf("freed %v and %v, want the page and the reference", freed.pages, freed.refs)
		}
		// Processed already, so freeing again gives back nothing.
		splice.Free()
		expect(t, "pages freed", len(freed.pages), 1)

		// Content inserted where no slot can be had is freed at once.
		freed = &freedContent{}
		splice = NewPageSpliceList(ps, freed)
		splice.Initialize(^uint64(0))
		maxSize := NewPageList[testPage](ps).MaxSize()
		expect(t, "insert", splice.Insert(maxSize, Page(pages[0])), ErrNoMemory)
		expect(t, "insert", splice.Insert(maxSize, Reference[testPage](ref)), ErrNoMemory)
		expect(t, "insert", splice.Insert(maxSize, Marker[testPage]()), ErrNoMemory)
		if !slices.Equal(freed.pages, pages) || !slices.Equal(freed.refs, []ReferenceValue{ref}) {
			t.Errorf("freed %v and %v, want the page and the reference", freed.pages, freed.refs)
		}
	})
}

// LookupOrAllocate checks for intervals only when asked, and gives no slot in
// an interval it may not split.
func TestLookupOrAllocateSplitsAnIntervalOnlyWhenAllowed(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		mustNotFail(t, "add", pl.AddZeroInterval(ps, 4*ps, IntervalDirty))

		slot, inInterval := pl.LookupOrAllocate(2*ps, CheckForInterval)
		expect(t, "slot", slot, nil)
		expect(t, "in an interval", inInterval, true)
		slot, inInterval = pl.LookupOrAllocate(5*ps, CheckForInterval)
		expect(t, "in an interval", inInterval, false)
		expect(t, "empty", slot.IsEmpty(), true)
		pl.ReturnEmptySlot(5 * ps)
		// An end is in its interval too.
		slot, inInterval = pl.LookupOrAllocate(3*ps, CheckForInterval)
		expect(t, "slot", slot, nil)
		expect(t, "in an interval", inInterval, true)

		slot, inInterval = pl.LookupOrAllocate(5*ps, IntervalHandling(9))
		expect(t, "slot", slot, nil)
		expect(t, "in an interval", inInterval, false)

		slot, inInterval = pl.LookupOrAllocate(pl.MaxSize(), SplitInterval)
		expect(t, "slot", slot, nil)
		expect(t, "in an interval", inInterval, false)
		expectNoError(t, "the interval", onlyTheInterval(pl, ps, 3*ps))
		pl.RemoveAllContent(dropContent)
	})
}

// Merging a range onto another list at an offset that is not node aligned
// splits each node's slots across two, and keeps what migrate leaves.
func TestMergingARangeSplitsNodesAndKeepsWhatIsLeft(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(pageFanOut)
		for i := range uint64(pageFanOut) {
			expect(t, "added", addPage(t, pl, pages[i], (pageFanOut+i)*ps), true)
		}
		other := NewPageList[testPage](ps)

		// The range starts three pages before the node, so the node's pages
		// land at 3 to 18 in other, across two of its nodes. Pages that would
		// land at odd offsets are left where they are.
		pl.MergeRangeOnto(func(src, dst *PageOrMarker[testPage], otherOffset uint64) {
			if (otherOffset/ps)%2 == 1 {
				return
			}
			dst.Set(src.Take())
		}, other, (pageFanOut-3)*ps, 2*pageFanOut*ps)

		var moved []uint64
		expectNoError(t, "walk", other.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if p.Page() != pages[off/ps-3] {
				return errBadState
			}
			moved = append(moved, off/ps)
			return nil
		}))
		var wantMoved []uint64
		for off := uint64(4); off <= pageFanOut+2; off += 2 {
			wantMoved = append(wantMoved, off)
		}
		if !slices.Equal(moved, wantMoved) {
			t.Errorf("moved to %v, want %v", moved, wantMoved)
		}

		var kept []uint64
		expectNoError(t, "walk", pl.ForEveryPage(func(_ *PageOrMarker[testPage], off uint64) error {
			kept = append(kept, off/ps)
			return nil
		}))
		var wantKept []uint64
		for i := uint64(0); i < pageFanOut; i += 2 {
			wantKept = append(wantKept, pageFanOut+i)
		}
		if !slices.Equal(kept, wantKept) {
			t.Errorf("kept %v, want %v", kept, wantKept)
		}
		expect(t, "other's nodes", other.HeapAllocationBytes(), 2*nodeBytes)

		// A range migrate leaves wholly in place makes no node in other.
		third := NewPageList[testPage](ps)
		pl.MergeRangeOnto(func(*PageOrMarker[testPage], *PageOrMarker[testPage], uint64) {}, third,
			pageFanOut*ps, 2*pageFanOut*ps)
		expect(t, "third's nodes", third.HeapAllocationBytes(), 0)

		// Lists of two page sizes cannot be merged.
		otherSize := NewPageList[testPage](2 * ps)
		expectPanic(t, "merging across page sizes", func() {
			pl.MergeRangeOnto(moveSlot, otherSize, 0, pageFanOut*ps)
		})
		pl.RemoveAllContent(dropContent)
		other.RemoveAllContent(dropContent)
	})
}

// Clipping nothing off an interval changes nothing.
func TestClippingNothingOffAnIntervalChangesNothing(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		mustNotFail(t, "add", pl.AddZeroInterval(ps, 4*ps, IntervalDirty))
		mustNotFail(t, "clip start", pl.ClipIntervalStart(ps, 0))
		mustNotFail(t, "clip end", pl.ClipIntervalEnd(3*ps, 0))
		expectNoError(t, "the interval", onlyTheInterval(pl, ps, 3*ps))
		pl.RemoveAllContent(dropContent)
	})
}

// A list's page size is a power of two, at least 512 bytes, so an
// AwaitingClean length keeps the bits above a zero interval's dirty state.
func TestAPageListTakesPageSizesThatArePowersOfTwo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		expect(t, "4 KiB", NewPageList[testPage](4<<10).PageSize(), 4<<10)
		expect(t, "2 MiB", NewPageList[testPage](2<<20).PageSize(), 2<<20)
		expect(t, "512 B", NewPageList[testPage](512).PageSize(), 512)
		expectPanic(t, "3 KiB", func() { NewPageList[testPage](3 << 10) })
		expectPanic(t, "256 B", func() { NewPageList[testPage](256) })
		expectPanic(t, "a reference that is not aligned", func() { MakeReferenceValue(1) })
		expect(t, "states", fmt.Sprint(IntervalUntracked, IntervalClean, IntervalDirty, IntervalDirtyState(7)),
			"Untracked Clean Dirty IntervalDirtyState(7)")
	})
}

// An interval added between two in its state joins them, though the slots
// before and after it are in different nodes, and both are given back.
func TestAnIntervalBridgingTwoAcrossNodesJoinsThem(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		mustNotFail(t, "add left", pl.AddZeroInterval(10*ps, pageFanOut*ps, IntervalDirty))
		mustNotFail(t, "add right", pl.AddZeroInterval((pageFanOut+1)*ps, (pageFanOut+5)*ps, IntervalDirty))
		mustNotFail(t, "add bridge", pl.AddZeroInterval(pageFanOut*ps, (pageFanOut+1)*ps, IntervalDirty))
		expectNoError(t, "one interval", onlyTheInterval(pl, 10*ps, (pageFanOut+4)*ps))

		// An Untracked slot joins an Untracked interval added before it.
		mustNotFail(t, "add slot", pl.AddZeroInterval(5*pageFanOut*ps, (5*pageFanOut+1)*ps, IntervalUntracked))
		mustNotFail(t, "add before", pl.AddZeroInterval((5*pageFanOut-2)*ps, 5*pageFanOut*ps, IntervalUntracked))
		expect(t, "start", pl.Lookup((5*pageFanOut-2)*ps).IsIntervalStart(), true)
		expect(t, "end", pl.Lookup(5*pageFanOut*ps).IsIntervalEnd(), true)
		expect(t, "untracked", pl.Lookup(5*pageFanOut*ps).IsZeroIntervalUntracked(), true)
		pl.RemoveAllContent(dropContent)
	})
}

// A contiguous run walk finds an interval's start or end in the node before
// or after, wherever in that node it is, and passes compare its offset.
func TestARunWalkFindsAnIntervalsFarEndInAnotherNode(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		// From the first slot of node 1 to slot 5 of node 2.
		start, end := uint64(pageFanOut), uint64(2*pageFanOut+5)
		mustNotFail(t, "add", pl.AddZeroInterval(start*ps, (end+1)*ps, IntervalDirty))
		// And [3, 9] in node 0.
		mustNotFail(t, "add", pl.AddZeroInterval(3*ps, 10*ps, IntervalDirty))

		var compared []uint64
		compare := func(_ *PageOrMarker[testPage], off uint64) bool {
			compared = append(compared, off/ps)
			return true
		}
		noPage := func(*PageOrMarker[testPage], uint64) error { return nil }
		var runs []uint64

		// From inside the interval: its start is found at the first slot of
		// the node before.
		err := pl.ForEveryPageAndContiguousRunInRange(compare, noPage, contiguousRuns(&runs),
			(start+4)*ps, (end+3)*ps)
		expectNoError(t, "walk", err)
		if !slices.Equal(compared, []uint64{end, start}) {
			t.Errorf("compared %v, want the end then the start", compared)
		}
		expectOffsets(t, "runs", runs, []uint64{start + 4, end + 1}, ps)

		// To inside the interval: its end is found at slot 5 of the node
		// after.
		compared, runs = nil, nil
		err = pl.ForEveryPageAndContiguousRunInRange(compare, noPage, contiguousRuns(&runs),
			start*ps, (end-2)*ps)
		expectNoError(t, "walk", err)
		if !slices.Equal(compared, []uint64{start, end}) {
			t.Errorf("compared %v, want the start then the end", compared)
		}
		expectOffsets(t, "runs", runs, []uint64{start, end - 2}, ps)

		// To inside [3, 9]: its end is in the same node.
		compared, runs = nil, nil
		err = pl.ForEveryPageAndContiguousRunInRange(compare, noPage, contiguousRuns(&runs), 3*ps, 6*ps)
		expectNoError(t, "walk", err)
		if !slices.Equal(compared, []uint64{3, 9}) {
			t.Errorf("compared %v, want 3 then 9", compared)
		}
		expectOffsets(t, "runs", runs, []uint64{3, 6}, ps)

		// From inside an interval whose start is partway into the node
		// before.
		farStart, farEnd := uint64(4*pageFanOut+5), uint64(5*pageFanOut+3)
		mustNotFail(t, "add", pl.AddZeroInterval(farStart*ps, (farEnd+1)*ps, IntervalDirty))
		compared, runs = nil, nil
		err = pl.ForEveryPageAndContiguousRunInRange(compare, noPage, contiguousRuns(&runs),
			(farStart+2)*ps, (farEnd+3)*ps)
		expectNoError(t, "walk", err)
		if !slices.Equal(compared, []uint64{farEnd, farStart}) {
			t.Errorf("compared %v, want the end then the start", compared)
		}
		expectOffsets(t, "runs", runs, []uint64{farStart + 2, farEnd + 1}, ps)
		pl.RemoveAllContent(dropContent)
	})
}

// Pages run up to an interval, which starts a run of its own, as a single
// slot interval does.
func TestAnIntervalEndsTheRunOfPagesBeforeIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		pages := getPages(2)
		expect(t, "added", addPage(t, pl, pages[0], 0), true)
		expect(t, "added", addPage(t, pl, pages[1], ps), true)
		mustNotFail(t, "add slot", pl.AddZeroInterval(2*ps, 3*ps, IntervalDirty))
		mustNotFail(t, "add", pl.AddZeroInterval(4*ps, 7*ps, IntervalUntracked))

		noPage := func(*PageOrMarker[testPage], uint64) error { return nil }
		var runs []uint64
		var kinds []bool
		err := pl.ForEveryPageAndContiguousRunInRange(always, noPage, func(s, e uint64, isInterval bool) error {
			runs = append(runs, s, e)
			kinds = append(kinds, isInterval)
			return nil
		}, 0, pageFanOut*ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "runs", runs, []uint64{0, 2, 2, 3, 4, 7}, ps)
		if !slices.Equal(kinds, []bool{false, true, true}) {
			t.Errorf("runs are intervals %v, want a run of pages then two intervals", kinds)
		}

		// The run callback failing on the pages before the interval fails
		// the walk there.
		runs = nil
		err = pl.ForEveryPageAndContiguousRunInRange(always, noPage, func(s, e uint64, _ bool) error {
			runs = append(runs, s, e)
			return errInvalidArgs
		}, 0, pageFanOut*ps)
		expect(t, "walk", err, errInvalidArgs)
		expectOffsets(t, "runs", runs, []uint64{0, 2}, ps)

		var freed []*testPage
		pl.RemoveAllContent(releasePages(&freed))
		expect(t, "pages freed", len(freed), 2)
	})
}

// A callback's error ends a walk wherever it comes, and a run callback is
// called only on runs that hold a page.
func TestACallbackErrorEndsAWalkWhereverItComes(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const start, end = 1, 2 * pageFanOut
		mustNotFail(t, "add", pl.AddZeroInterval(start*ps, (end+1)*ps, IntervalDirty))
		pages := getPages(2)
		expect(t, "added", addPage(t, pl, pages[0], 3*pageFanOut*ps), true)
		expect(t, "added", addPage(t, pl, pages[1], (3*pageFanOut+1)*ps), true)

		noPage := func(*PageOrMarker[testPage], uint64) error { return nil }
		var runs []uint64
		failingRun := func(s, e uint64, isInterval bool) error {
			runs = append(runs, s, e)
			return errInvalidArgs
		}

		// Inside the interval up to its end, which the walk does not see.
		err := pl.ForEveryPageAndContiguousRunInRange(always, noPage, failingRun, (start+1)*ps, end*ps)
		expect(t, "walk inside", err, errInvalidArgs)
		expectOffsets(t, "runs", runs, []uint64{start + 1, end}, ps)

		// From before the interval into it.
		runs = nil
		err = pl.ForEveryPageAndContiguousRunInRange(always, noPage, failingRun, 0, (end-1)*ps)
		expect(t, "walk into", err, errInvalidArgs)
		expectOffsets(t, "runs", runs, []uint64{start, end - 1}, ps)

		// A run of pages that ends with the range.
		runs = nil
		err = pl.ForEveryPageAndContiguousRunInRange(always, noPage, failingRun,
			3*pageFanOut*ps, (3*pageFanOut+2)*ps)
		expect(t, "walk of pages", err, errInvalidArgs)
		expectOffsets(t, "runs", runs, []uint64{3 * pageFanOut, 3*pageFanOut + 2}, ps)

		// A page callback failing on the first page of a run: no run before
		// it to call the run callback on.
		runs = nil
		err = pl.ForEveryPageAndContiguousRunInRange(always,
			func(*PageOrMarker[testPage], uint64) error { return errBadState }, failingRun,
			3*pageFanOut*ps, 4*pageFanOut*ps)
		expect(t, "walk failing at once", err, errBadState)
		expect(t, "runs", len(runs), 0)

		// A gap callback failing on the last gap.
		err = pl.ForEveryPageAndGapInRange(noPage, func(s, e uint64) error {
			if e == 4*pageFanOut*ps {
				return errOutOfRange
			}
			return nil
		}, 3*pageFanOut*ps, 4*pageFanOut*ps)
		expect(t, "last gap", err, errOutOfRange)

		var freed []*testPage
		pl.RemoveAllContent(releasePages(&freed))
	})
}

// Splitting an interval at the last slot of a node makes the node after for
// the new start, and populating slots from there runs across the boundary.
func TestSplittingAnIntervalAtANodesLastSlotMakesTheNextNode(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		const start, end = 1, 3*pageFanOut + 1
		mustNotFail(t, "add", pl.AddZeroInterval(start*ps, (end+1)*ps, IntervalDirty))

		page := getPages(1)[0]
		expect(t, "added", addPage(t, pl, page, (pageFanOut-1)*ps), true)
		var pageOff uint64
		intervals, gaps, err := intervalPageWalk(pl, 0, 4*pageFanOut*ps, &pageOff)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{start, pageFanOut - 2, pageFanOut, end}, ps)
		expectOffsets(t, "gaps", gaps, []uint64{0, start, end + 1, 4 * pageFanOut}, ps)
		expect(t, "page", pageOff, (pageFanOut-1)*ps)

		// Three slots from the last slot of node 1 into node 2.
		from := uint64(2*pageFanOut - 1)
		mustNotFail(t, "populate", pl.PopulateSlotsInInterval(from*ps, (from+3)*ps))
		intervals, _, next, err := populateWalk(pl, pageFanOut*ps, (end+1)*ps, from*ps, ps)
		expectNoError(t, "walk", err)
		expectOffsets(t, "intervals", intervals, []uint64{pageFanOut, from - 1, from + 3, end}, ps)
		expect(t, "slots end", next, (from+3)*ps)

		// Two slots across the next boundary, with none between them.
		from = 3*pageFanOut - 1
		mustNotFail(t, "populate", pl.PopulateSlotsInInterval(from*ps, (from+2)*ps))
		expect(t, "slot", pl.Lookup(from*ps).IsIntervalSlot(), true)
		expect(t, "slot", pl.Lookup((from+1)*ps).IsIntervalSlot(), true)

		// No slot populated without an AwaitingClean length has one.
		expectNoError(t, "lengths", pl.ForEveryPage(func(p *PageOrMarker[testPage], off uint64) error {
			if (p.IsIntervalStart() || p.IsIntervalSlot()) && p.GetZeroIntervalAwaitingCleanLength() != 0 {
				return fmt.Errorf("length %#x at %#x", p.GetZeroIntervalAwaitingCleanLength(), off)
			}
			return nil
		}))
		var freed []*testPage
		pl.RemoveAllContent(releasePages(&freed))
		expect(t, "pages freed", len(freed), 1)
	})
}

// An AwaitingClean length joins across returned slots far from offset zero,
// where the length to the left is the longer.
func TestReturnedSlotsJoinTheirAwaitingCleanLengthFarFromZero(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		start := (3*pageFanOut + 2) * ps
		end := start + 20*ps
		mustNotFail(t, "add", pl.AddZeroInterval(start, end+ps, IntervalDirty))
		pl.LookupMutable(start).SetZeroIntervalAwaitingCleanLength(3 * ps)

		mustNotFail(t, "populate", pl.PopulateSlotsInInterval(start, start+3*ps))
		expect(t, "slot 2", awaitingCleanLength(pl, start+2*ps), ps)
		expect(t, "the rest", awaitingCleanLength(pl, start+3*ps), 0)

		pl.ReturnIntervalSlot(start)
		expect(t, "slots 0 and 1", awaitingCleanLength(pl, start), 2*ps)
		pl.ReturnIntervalSlot(start + 2*ps)
		expect(t, "at start", awaitingCleanLength(pl, start), 3*ps)
		expectNoError(t, "the interval", onlyTheInterval(pl, start, end))
		pl.RemoveAllContent(dropContent)
	})
}

// Overwriting one page at either end of an interval leaves a Slot there, and
// overwriting the start carries what is left of the AwaitingClean length to
// the start left behind.
func TestOverwritingOnePageAtAnIntervalsEndLeavesASlot(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pl := NewPageList[testPage](ps)
		mustNotFail(t, "add", pl.AddZeroInterval(ps, 11*ps, IntervalUntracked))

		mustNotFail(t, "overwrite start", pl.OverwriteZeroInterval(ps, noOffset, ps, ps, IntervalDirty))
		expect(t, "slot", pl.Lookup(ps).IsIntervalSlot(), true)
		expect(t, "dirty", pl.Lookup(ps).IsZeroIntervalDirty(), true)
		expect(t, "start", pl.Lookup(2*ps).IsIntervalStart(), true)
		expect(t, "untracked", pl.Lookup(2*ps).IsZeroIntervalUntracked(), true)

		mustNotFail(t, "overwrite end", pl.OverwriteZeroInterval(noOffset, 10*ps, 10*ps, 10*ps, IntervalDirty))
		expect(t, "slot", pl.Lookup(10*ps).IsIntervalSlot(), true)
		expect(t, "dirty", pl.Lookup(10*ps).IsZeroIntervalDirty(), true)
		expect(t, "end", pl.Lookup(9*ps).IsIntervalEnd(), true)
		expect(t, "untracked", pl.Lookup(9*ps).IsZeroIntervalUntracked(), true)
		pl.RemoveAllContent(dropContent)

		pl = NewPageList[testPage](ps)
		mustNotFail(t, "add", pl.AddZeroInterval(ps, 11*ps, IntervalDirty))
		pl.LookupMutable(ps).SetZeroIntervalAwaitingCleanLength(6 * ps)
		mustNotFail(t, "overwrite", pl.OverwriteZeroInterval(ps, noOffset, ps, 3*ps, IntervalUntracked))
		expect(t, "carried length", awaitingCleanLength(pl, 4*ps), 3*ps)
		pl.RemoveAllContent(dropContent)
	})
}

// An overwrite that joins its neighbour gives back the neighbour's sentinel,
// and with it the node it was alone in.
func TestAnOverwriteThatJoinsANeighbourFreesItsNode(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		// The left interval's end is alone in node 1.
		pl := NewPageList[testPage](ps)
		mustNotFail(t, "add left", pl.AddZeroInterval(10*ps, 2*pageFanOut*ps, IntervalDirty))
		mustNotFail(t, "add right", pl.AddZeroInterval(2*pageFanOut*ps, (2*pageFanOut+9)*ps, IntervalUntracked))
		mustNotFail(t, "overwrite", pl.OverwriteZeroInterval(2*pageFanOut*ps, noOffset, 2*pageFanOut*ps,
			(2*pageFanOut+1)*ps, IntervalDirty))
		expect(t, "node 1", pl.Lookup((2*pageFanOut-1)*ps), nil)
		intervals, states, err := stateWalk(pl, 10*ps, (2*pageFanOut+9)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{10, 2*pageFanOut + 1, 2*pageFanOut + 2, 2*pageFanOut + 8},
			[]IntervalDirtyState{IntervalDirty, IntervalDirty, IntervalUntracked, IntervalUntracked})
		pl.RemoveAllContent(dropContent)

		// The right interval's start is alone in node 2.
		pl = NewPageList[testPage](ps)
		mustNotFail(t, "add left", pl.AddZeroInterval(5*ps, 2*pageFanOut*ps, IntervalUntracked))
		mustNotFail(t, "add right", pl.AddZeroInterval(2*pageFanOut*ps, (3*pageFanOut+12)*ps, IntervalDirty))
		mustNotFail(t, "overwrite", pl.OverwriteZeroInterval(noOffset, (2*pageFanOut-1)*ps, (2*pageFanOut-2)*ps,
			(2*pageFanOut-1)*ps, IntervalDirty))
		expect(t, "node 2", pl.Lookup(2*pageFanOut*ps), nil)
		intervals, states, err = stateWalk(pl, 5*ps, (3*pageFanOut+12)*ps, ps)
		expectNoError(t, "walk", err)
		expectStates(t, intervals, states, []uint64{5, 2*pageFanOut - 3, 2*pageFanOut - 2, 3*pageFanOut + 11},
			[]IntervalDirtyState{IntervalUntracked, IntervalUntracked, IntervalDirty, IntervalDirty})
		pl.RemoveAllContent(dropContent)
	})
}

func expectPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		if recover() == nil {
			t.Errorf("%s: did not panic", what)
		}
	}()
	f()
}
