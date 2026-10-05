package vmmemory

import "github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"

// pageRuns is a set of pages held as runs of consecutive pages: the Dirty
// intervals of a page list of its own. The page list joins an interval to the
// ones beside it as pages enter and splits it as they leave, and a walk of it
// visits a run's two ends and nothing between them. So reading the set costs
// its runs and not its pages, which is what a seal's pause needs: see
// sealableRuns. Zircon reports a Dirty zero interval to EnumerateDirtyRanges
// as one range in the same way. Nothing here is a zero page; the interval is
// the page list's only structure that holds a run in one step. Its zero value
// is not usable; newPageRuns makes one. The owner synchronizes it.
type pageRuns struct {
	list *zirconvm.PageList[struct{}]
}

func newPageRuns(pageSize uint64) pageRuns {
	return pageRuns{list: zirconvm.NewPageList[struct{}](pageSize)}
}

func (s pageRuns) offset(page uint64) uint64 { return page * s.list.PageSize() }

// has reports whether page is in the set.
func (s pageRuns) has(page uint64) bool { return s.list.IsOffsetInZeroInterval(s.offset(page)) }

// add puts every page of [first, last) in the set.
func (s pageRuns) add(first, last uint64) {
	var gaps [][2]uint64
	if err := s.list.ForEveryPageAndGapInRange(func(*zirconvm.PageOrMarker[struct{}], uint64) error { return nil },
		func(start, end uint64) error {
			gaps = append(gaps, [2]uint64{start, end})
			return nil
		}, s.offset(first), s.offset(last)); err != nil {
		panic("vmmemory: walking a set of runs: " + err.Error())
	}
	for _, gap := range gaps {
		if err := s.list.AddZeroInterval(gap[0], gap[1], zirconvm.IntervalDirty); err != nil {
			panic("vmmemory: adding a run: " + err.Error())
		}
	}
}

// remove takes page out of the set, splitting the run that holds it.
func (s pageRuns) remove(page uint64) {
	offset := s.offset(page)
	if !s.list.IsOffsetInZeroInterval(offset) {
		return
	}
	// The split leaves the page an interval of its own, which goes.
	s.list.LookupOrAllocate(offset, zirconvm.SplitInterval)
	s.list.RemoveContent(offset)
}

// runs is the set's runs within [0, pages), in order.
func (s pageRuns) runs(pages uint64) []PageRun {
	var runs []PageRun
	size := s.list.PageSize()
	if err := s.list.ForEveryPageAndContiguousRunInRange(
		func(*zirconvm.PageOrMarker[struct{}], uint64) bool { return true },
		func(*zirconvm.PageOrMarker[struct{}], uint64) error { return nil },
		func(start, end uint64, _ bool) error {
			runs = append(runs, PageRun{Page: start / size, Count: int((end - start) / size)})
			return nil
		}, 0, s.offset(pages)); err != nil {
		panic("vmmemory: walking a set of runs: " + err.Error())
	}
	return runs
}
