// Copyright 2016 The Fuchsia Authors
// Ported from zircon/kernel/vm/include/vm/vm_page_list.h and vm/vm_page_list.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

// Freer gives back what a slot owned: a page to where pages come from, and a
// reference to the storage it refers to. Zircon frees into the kernel's one
// pmm and one page compression. A process here runs a pager per page size,
// each with its own arena and spill file, so a splice list is given its
// Freer.
type Freer[P any] interface {
	FreePage(page *P)
	FreeReference(ref ReferenceValue)
}

// spliceState is where a splice list is in its life.
type spliceState uint8

const (
	spliceConstructed spliceState = iota
	spliceInitialized
	spliceFinalized
	spliceProcessed
)

// PageSpliceList holds the content taken from a range of a page list, gaps
// and markers included: Zircon's VmPageSpliceList. Every splice list goes
// through these states:
//  1. It is made.
//  2. Initialize gives it its range.
//  3. Pages are added to it.
//  4. Finalize ends the adding.
//  5. Pages are popped from it. Once all are, it is processed.
//  6. Processed, it can be dropped.
//
// Zircon's CreateFromPageList, which only its PhysicalPageProvider uses, is
// not ported: physical memory is not.
type PageSpliceList[P any] struct {
	length   uint64
	pos      uint64
	state    spliceState
	pageList *PageList[P]
	freer    Freer[P]
}

// NewPageSpliceList is a splice list over pages of pageSize bytes, which gives
// back what it still owns through freer when it is freed.
func NewPageSpliceList[P any](pageSize uint64, freer Freer[P]) *PageSpliceList[P] {
	return &PageSpliceList[P]{pageList: NewPageList[P](pageSize), freer: freer}
}

// Initialize gives the list its range, once, before any page is added.
func (s *PageSpliceList[P]) Initialize(length uint64) {
	assert(s.IsEmpty() && s.state == spliceConstructed, "a splice list is initialized once, empty")
	s.length = length
	s.state = spliceInitialized
}

// Free is the splice list's destructor: it gives back every page and
// reference the list still holds. Go has no destructors, so the owner calls
// it.
func (s *PageSpliceList[P]) Free() {
	switch s.state {
	case spliceConstructed:
	case spliceInitialized:
		s.Finalize()
		fallthrough
	case spliceFinalized:
		s.freeAllPages()
	case spliceProcessed:
	}
}

func (s *PageSpliceList[P]) freeAllPages() {
	// Give back every page and reference the list owns.
	s.pageList.RemoveAllContent(func(page PageOrMarker[P]) {
		if page.IsPage() {
			s.freer.FreePage(page.ReleasePage())
		} else if page.IsReference() {
			s.freer.FreeReference(page.ReleaseReference())
		}
	})
	s.state = spliceProcessed
}

// Pop takes the next slot's content off the list, which must be finalized.
func (s *PageSpliceList[P]) Pop() PageOrMarker[P] {
	// Zircon fails this with DEBUG_ASSERT_MSG and returns Empty in a build
	// without debug asserts; asserts always panic here.
	assert(s.IsFinalized(), "a splice list is finalized before it is popped")
	res := s.pageList.RemoveContent(s.pos)
	s.pos += s.pageList.g.pageSize()
	if s.pos >= s.length {
		s.state = spliceProcessed
	}
	return res
}

// AddPagesFrom takes the content of source from offset, for the list's length,
// into the list, which must be initialized and empty, and finalizes it.
// merge moves content from src to dst. Zircon returns ZX_ERR_NO_MEMORY when it
// cannot allocate a node, having moved some pages; a Go allocation does not
// fail, so this returns nothing.
func (s *PageSpliceList[P]) AddPagesFrom(merge func(src, dst *PageOrMarker[P], offset uint64), source *PageList[P],
	offset uint64) {
	end := offset + s.length
	source.MergeRangeOnto(merge, s.pageList, offset, end)
	s.Finalize()
}

// MutatePages calls fn on every slot from start to the end of the list
// without taking its content. The slots are not processed by it and cannot be
// removed. start is page rounded and below the list's length.
func (s *PageSpliceList[P]) MutatePages(fn func(slot PageOrMarkerRef[P], offset uint64) error, start uint64) error {
	assert(s.IsFinalized(), "the splice list is finalized")
	assert(start < s.length, "the start is in the list")
	assert(s.pageList.g.isPageRounded(start), "the start is page rounded")
	return s.pageList.ForEveryPageInRangeMutable(fn, start, s.length)
}

// RemovePagesAndIterateGaps walks the list's pages and gaps from where the
// last walk stopped, and hands each page's content to pageFn, which owns it
// from then on. The list must be finalized. pageFn owns the content whatever
// it returns, so a slot it stops on, with ErrStop or an error, is processed and
// is not walked again. A gap gapFn stops on is not processed and comes back on
// the next walk.
func (s *PageSpliceList[P]) RemovePagesAndIterateGaps(pageFn func(content PageOrMarker[P], offset uint64) error,
	gapFn func(start, end uint64) error) error {
	assert(s.IsFinalized(), "the splice list is finalized")
	// Assume the whole range is processed, and trim it if a callback stops.
	processed := s.length
	err := s.pageList.RemovePagesAndIterateGaps(
		func(slot *PageOrMarker[P], srcOffset uint64) error {
			// The content leaves the slot before pageFn sees it, so pageFn
			// must deal with it and cannot leave it in the slot.
			content := slot.Take()
			err := pageFn(content, srcOffset)
			if err != nil {
				processed = srcOffset + s.pageList.g.pageSize()
			}
			return err
		},
		func(gapStart, gapEnd uint64) error {
			err := gapFn(gapStart, gapEnd)
			if err != nil {
				processed = gapStart
			}
			return err
		},
		s.pos, s.length)
	s.pos = processed
	if s.pos == s.length {
		assert(s.pageList.IsEmpty(), "a processed splice list is empty")
		s.state = spliceProcessed
	}
	return err
}

// Insert puts content in the list at offset. The list owns it from then on,
// and must not be finalized.
func (s *PageSpliceList[P]) Insert(offset uint64, content PageOrMarker[P]) error {
	assert(offset < s.length, "the offset is in the list")
	assert(s.IsInitialized(), "the splice list is initialized and not finalized")
	assert(!content.IsInterval(), "a splice list holds no interval")

	slot, _ := s.pageList.LookupOrAllocate(offset, NoIntervals)
	if slot == nil {
		// The content was the list's, so it is freed. Zircon also asserts a
		// page is in no page queue; pages have none until the page queues
		// are ported.
		if content.IsPage() {
			s.freer.FreePage(content.ReleasePage())
		} else if content.IsReference() {
			s.freer.FreeReference(content.ReleaseReference())
		}
		return ErrNoMemory
	}
	slot.Set(content)
	return nil
}

// IsProcessed reports whether every slot has been popped or walked.
func (s *PageSpliceList[P]) IsProcessed() bool { return s.state == spliceProcessed }

// IsInitialized reports whether the list has its range and is not finalized.
func (s *PageSpliceList[P]) IsInitialized() bool { return s.state == spliceInitialized }

// IsEmpty reports whether the list holds nothing.
func (s *PageSpliceList[P]) IsEmpty() bool { return s.pageList.IsEmpty() }

// Finalize ends the adding of pages. It is called once, on an initialized
// list.
func (s *PageSpliceList[P]) Finalize() {
	assert(s.IsInitialized(), "the splice list is initialized")
	s.state = spliceFinalized
}

// IsFinalized reports whether the list is finalized and not yet processed.
func (s *PageSpliceList[P]) IsFinalized() bool { return s.state == spliceFinalized }

// Position is where the list is in its range.
func (s *PageSpliceList[P]) Position() uint64 { return s.pos }
