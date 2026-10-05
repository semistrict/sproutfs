// Copyright 2016 The Fuchsia Authors
// Ported from zircon/kernel/vm/include/vm/vm_page_list.h and vm/vm_page_list.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"errors"
	"math"
	"math/bits"
	"unsafe"

	"github.com/google/btree"
)

// pageGeometry is the page size a page list is built over. Zircon's is the
// constant kPageSize; a pager's is fixed for its life, 4 KiB or 2 MiB, but is
// not a constant, so each list carries it.
type pageGeometry struct{ shift uint8 }

// pageSize is kPageSize.
func (g pageGeometry) pageSize() uint64 { return 1 << g.shift }

// nodeSize is the span of one node: kPageFanOut pages.
func (g pageGeometry) nodeSize() uint64 { return pageFanOut << g.shift }

// nodeOffset is VmPageListNode::NodeOffset: the base offset of the node that
// holds offset.
func (g pageGeometry) nodeOffset(offset uint64) uint64 { return offset &^ (g.nodeSize() - 1) }

// nodeIndex is VmPageListNode::NodeIndex: the slot of offset in its node.
func (g pageGeometry) nodeIndex(offset uint64) uint64 { return (offset >> g.shift) % pageFanOut }

// endOffset is VmPageListNode::end_offset: one past the last offset of the
// node at base.
func (g pageGeometry) endOffset(base uint64) uint64 {
	assert(g.nodeOffset(base) == base, "a node's base is node aligned")
	return base + g.nodeSize()
}

// isPageRounded is IsPageRounded.
func (g pageGeometry) isPageRounded(offset uint64) bool { return offset&(g.pageSize()-1) == 0 }

// maxSize is VmPageList::MAX_SIZE, which leaves room for the end of the last
// node.
func (g pageGeometry) maxSize() uint64 { return g.nodeOffset(math.MaxUint64) }

// pageListNode is VmPageListNode: kPageFanOut consecutive slots.
type pageListNode[P any] struct {
	pages [pageFanOut]PageOrMarker[P]
}

// forEveryPage calls fn on every slot of the node that is not Empty.
func (n *pageListNode[P]) forEveryPage(g pageGeometry, base uint64, fn func(*PageOrMarker[P], uint64) error) error {
	return n.forEveryPageInRange(g, base, fn, base, g.endOffset(base))
}

// forEveryPageInRange calls fn on every slot in [start, end) that is not
// Empty. The range is within the node.
func (n *pageListNode[P]) forEveryPageInRange(g pageGeometry, base uint64, fn func(*PageOrMarker[P], uint64) error,
	start, end uint64) error {
	assert(end >= start, "the range is not backwards")
	assert(start >= base, "the range starts in the node")
	assert(end <= g.endOffset(base), "the range ends in the node")
	first := (start - base) >> g.shift
	last := (end - base) >> g.shift
	for i := first; i < last; i++ {
		if !n.pages[i].IsEmpty() {
			if err := fn(&n.pages[i], base+i*g.pageSize()); err != nil {
				return err
			}
		}
	}
	return nil
}

// isOffsetInInterval reports whether off is in an interval, as far as this
// node can tell: it cannot find the whole interval, which may need another
// node, but it can tell whether off is in one. It returns the sentinel it
// found, or nil.
func (n *pageListNode[P]) isOffsetInInterval(g pageGeometry, objOffset, off uint64) *PageOrMarker[P] {
	assert(off >= objOffset, "the offset is in the node")
	assert(off < g.endOffset(objOffset), "the offset is in the node")
	index := (off - objOffset) >> g.shift
	// If the slot is any kind of sentinel, the offset is in an interval.
	if !n.pages[index].IsEmpty() {
		if n.pages[index].IsInterval() {
			return &n.pages[index]
		}
		return nil
	}
	// An interval end to the right puts the offset in an interval. Anything
	// else there means it cannot be in one.
	for i := index + 1; i < pageFanOut; i++ {
		if !n.pages[i].IsEmpty() {
			if n.pages[i].IsIntervalEnd() {
				return &n.pages[i]
			}
			return nil
		}
	}
	// Nothing to the right, so look for an interval start to the left.
	for i := index; i > 0; i-- {
		if !n.pages[i-1].IsEmpty() {
			if n.pages[i-1].IsIntervalStart() {
				return &n.pages[i-1]
			}
			return nil
		}
	}
	panic("zirconvm: unexpected empty node")
}

// nodeStartsInInterval returns the node's first slot that is not Empty if it
// is an interval end, which means an interval began in an earlier node and
// covers the start of this one, and nil otherwise.
func (n *pageListNode[P]) nodeStartsInInterval() *PageOrMarker[P] {
	for i := range n.pages {
		if !n.pages[i].IsEmpty() {
			if n.pages[i].IsIntervalEnd() {
				return &n.pages[i]
			}
			return nil
		}
	}
	panic("zirconvm: unexpected empty node")
}

func (n *pageListNode[P]) lookup(index uint64) *PageOrMarker[P] {
	assert(index < pageFanOut, "the index is in the node")
	return &n.pages[index]
}

// isEmpty reports whether the node holds no page, sentinel, reference or
// marker.
func (n *pageListNode[P]) isEmpty() bool {
	for i := range n.pages {
		if !n.pages[i].IsEmpty() {
			return false
		}
	}
	return true
}

// hasNoPageOrRef reports whether the node owns nothing that must be given
// back.
func (n *pageListNode[P]) hasNoPageOrRef() bool {
	for i := range n.pages {
		if n.pages[i].IsPageOrRef() {
			return false
		}
	}
	return true
}

func (n *pageListNode[P]) hasNoIntervalSentinel() bool {
	for i := range n.pages {
		if n.pages[i].IsInterval() {
			return false
		}
	}
	return true
}

func (n *pageListNode[P]) mergeRangeOnto(g pageGeometry, base, otherBase uint64,
	migrate func(src, dst *PageOrMarker[P], otherOffset uint64), other *pageListNode[P],
	start, end, otherStart uint64) {
	assert(otherStart >= otherBase, "the other range starts in the other node")
	assert(otherStart+(end-start) <= g.endOffset(otherBase), "the other range ends in the other node")
	_ = n.forEveryPageInRange(g, base, func(slot *PageOrMarker[P], offset uint64) error {
		otherOffset := offset - start + otherStart
		assert(g.nodeOffset(otherOffset) == otherBase, "the other offset is in the other node")
		migrate(slot, &other.pages[g.nodeIndex(otherOffset)], otherOffset)
		return nil
	}, start, end)
}

// nodeEntry is a node in the tree with its base offset. A valid entry stands
// for a valid btree iterator, and the zero entry for an invalid one.
type nodeEntry[P any] struct {
	offset uint64
	node   *pageListNode[P]
}

func (e nodeEntry[P]) valid() bool { return e.node != nil }

func lessNode[P any](a, b nodeEntry[P]) bool { return a.offset < b.offset }

// btreeDegree is the degree of the tree of nodes. Zircon's lib/btree sizes
// its nodes by bytes; google/btree takes a degree.
const btreeDegree = 16

// PageList is a VMO's pages by offset, Zircon's VmPageList. It is a tree of
// nodes of kPageFanOut slots each, keyed by the node's base offset, and holds
// only nodes with at least one slot that is not Empty.
//
// Zircon's tree is its own lib/btree, whose iterators the list and its cursors
// step from node to node. This one is github.com/google/btree, which has no
// iterators: a step to the next or previous node is a search from the
// current node's offset. That finds the same node.
type PageList[P any] struct {
	g    pageGeometry
	list *btree.BTreeG[nodeEntry[P]]
}

// NewPageList is an empty page list over pages of pageSize bytes, which is a
// power of two.
func NewPageList[P any](pageSize uint64) *PageList[P] {
	shift := bits.TrailingZeros64(pageSize)
	assert(pageSize == 1<<shift, "the page size is a power of two")
	// Zircon's static_assert that an AwaitingClean length, which drops the
	// bits below the page shift, keeps its bits above the dirty state.
	assert(shift >= awaitingCleanLengthShift, "the page shift leaves room below it")
	return &PageList[P]{
		g:    pageGeometry{shift: uint8(shift)},
		list: btree.NewG(btreeDegree, lessNode[P]),
	}
}

// PageSize is the size of the list's pages.
func (pl *PageList[P]) PageSize() uint64 { return pl.g.pageSize() }

// MaxSize is VmPageList::MAX_SIZE: no slot is at or past it, so the end of
// every node fits in an offset.
func (pl *PageList[P]) MaxSize() uint64 { return pl.g.maxSize() }

// NodePages is kPageFanOut, the number of slots in one node.
const NodePages = pageFanOut

// find is the node at nodeOffset, or an invalid entry.
func (pl *PageList[P]) find(nodeOffset uint64) nodeEntry[P] {
	e, _ := pl.list.Get(nodeEntry[P]{offset: nodeOffset})
	return e
}

// lowerBound is the first node at or after offset, or an invalid entry.
func (pl *PageList[P]) lowerBound(offset uint64) nodeEntry[P] {
	var found nodeEntry[P]
	pl.list.AscendGreaterOrEqual(nodeEntry[P]{offset: offset}, func(e nodeEntry[P]) bool {
		found = e
		return false
	})
	return found
}

// next is the node after e, or an invalid entry. Zircon increments an
// iterator.
func (pl *PageList[P]) next(e nodeEntry[P]) nodeEntry[P] { return pl.lowerBound(e.offset + 1) }

// prev is the node before e, or an invalid entry. Zircon decrements an
// iterator.
func (pl *PageList[P]) prev(e nodeEntry[P]) nodeEntry[P] {
	var found nodeEntry[P]
	if e.offset == 0 {
		return found
	}
	pl.list.DescendLessOrEqual(nodeEntry[P]{offset: e.offset - 1}, func(x nodeEntry[P]) bool {
		found = x
		return false
	})
	return found
}

// insert adds a node at nodeOffset, where there is none.
func (pl *PageList[P]) insert(nodeOffset uint64, node *pageListNode[P]) nodeEntry[P] {
	e := nodeEntry[P]{offset: nodeOffset, node: node}
	_, replaced := pl.list.ReplaceOrInsert(e)
	assert(!replaced, "a node is inserted where there is none")
	return e
}

// erase removes a node, which must own no page or reference: Zircon's node
// destructor asserts that, since freeing the node would lose them.
func (pl *PageList[P]) erase(e nodeEntry[P]) {
	assert(e.node.hasNoPageOrRef(), "an erased node owns no page or reference")
	pl.list.Delete(e)
}

// eraseNext erases e and returns the node after it, as Zircon's erase returns
// the next iterator.
func (pl *PageList[P]) eraseNext(e nodeEntry[P]) nodeEntry[P] {
	pl.erase(e)
	return pl.lowerBound(e.offset + 1)
}

// ForEveryPage calls fn on every slot of the list that is not Empty, in order
// of offset. fn must only read the slot.
func (pl *PageList[P]) ForEveryPage(fn func(p *PageOrMarker[P], offset uint64) error) error {
	return pl.forEveryPage(fn)
}

// ForEveryPageMutable is ForEveryPage with a PageOrMarkerRef, which allows the
// limited changes it allows.
func (pl *PageList[P]) ForEveryPageMutable(fn func(p PageOrMarkerRef[P], offset uint64) error) error {
	return pl.forEveryPage(func(p *PageOrMarker[P], offset uint64) error {
		return fn(PageOrMarkerRef[P]{slot: p}, offset)
	})
}

// ForEveryPageInRange calls fn on every slot in [start, end) that is not
// Empty. fn must only read the slot.
func (pl *PageList[P]) ForEveryPageInRange(fn func(p *PageOrMarker[P], offset uint64) error, start, end uint64) error {
	return pl.forEveryPageInRange(fn, start, end, false)
}

// ForEveryPageInCursorRange is ForEveryPageInRange from a valid cursor's
// offset. Unlike it, it returns ErrStop when fn stopped the walk.
func (pl *PageList[P]) ForEveryPageInCursorRange(fn func(p *PageOrMarker[P], offset uint64) error, cursor Cursor[P],
	end uint64) error {
	start := cursor.Offset()
	if start >= end {
		return nil
	}
	return pl.forEveryPageInRangeInternal(fn, cursor.node, start, end, false)
}

// ForEveryPageInRangeMutable is ForEveryPageInRange with a PageOrMarkerRef.
func (pl *PageList[P]) ForEveryPageInRangeMutable(fn func(p PageOrMarkerRef[P], offset uint64) error,
	start, end uint64) error {
	return pl.forEveryPageInRange(func(p *PageOrMarker[P], offset uint64) error {
		return fn(PageOrMarkerRef[P]{slot: p}, offset)
	}, start, end, false)
}

// ForEveryPageAndGapInRange calls pageFn on every slot in [start, end) that is
// not Empty, and gapFn on every run of Empty slots between them that is not
// inside an interval. pageFn must only read the slot.
func (pl *PageList[P]) ForEveryPageAndGapInRange(pageFn func(p *PageOrMarker[P], offset uint64) error,
	gapFn func(start, end uint64) error, start, end uint64) error {
	return pl.forEveryPageAndGapInRange(pageFn, gapFn, start, end, false)
}

// ForEveryPageAndGapInRangeMutable is ForEveryPageAndGapInRange with a
// PageOrMarkerRef.
func (pl *PageList[P]) ForEveryPageAndGapInRangeMutable(pageFn func(p PageOrMarkerRef[P], offset uint64) error,
	gapFn func(start, end uint64) error, start, end uint64) error {
	return pl.forEveryPageAndGapInRange(func(p *PageOrMarker[P], offset uint64) error {
		return pageFn(PageOrMarkerRef[P]{slot: p}, offset)
	}, gapFn, start, end, false)
}

// ForEveryPageAndContiguousRunInRange calls pageFn on every slot in [start,
// end) that compare accepts, and runFn on every run of consecutive such slots
// with whether the run is an interval. An interval is a run of its own and is
// never joined to pages or markers beside it, and its part in the range is a
// run only if compare accepts both its start and its end, wherever they lie.
// pageFn and compare must only read the slot.
func (pl *PageList[P]) ForEveryPageAndContiguousRunInRange(compare func(p *PageOrMarker[P], offset uint64) bool,
	pageFn func(p *PageOrMarker[P], offset uint64) error, runFn func(start, end uint64, isInterval bool) error,
	start, end uint64) error {
	return pl.forEveryPageAndContiguousRunInRange(compare, pageFn, runFn, start, end)
}

// AnyPagesOrIntervalsInRange reports whether any slot in [start, end) holds a
// page, reference or marker, or whether the range is part of an interval.
func (pl *PageList[P]) AnyPagesOrIntervalsInRange(start, end uint64) bool {
	found := false
	_ = pl.ForEveryPageInRange(func(*PageOrMarker[P], uint64) error {
		found = true
		return ErrStop
	}, start, end)
	// The range can be part of an interval with no slot in it populated. The
	// whole range is then in the same interval, so its start tells.
	if found {
		return true
	}
	return pl.isOffsetInInterval(start)
}

// AnyOwnedPagesOrIntervalsInRange is AnyPagesOrIntervalsInRange skipping
// ParentContent markers, which stand for content the parent owns.
func (pl *PageList[P]) AnyOwnedPagesOrIntervalsInRange(start, end uint64) bool {
	found := false
	_ = pl.ForEveryPageInRange(func(p *PageOrMarker[P], _ uint64) error {
		if p.IsParentContent() {
			return nil
		}
		found = true
		return ErrStop
	}, start, end)
	if found {
		return true
	}
	return pl.isOffsetInInterval(start)
}

// Lookup is the slot at offset, or nil if no node holds it. A slot it returns
// may be Empty. The caller must only read it, as Zircon's const pointer
// allows; it is valid until a Remove, Take or Merge changes the list.
func (pl *PageList[P]) Lookup(offset uint64) *PageOrMarker[P] {
	if pln := pl.find(pl.g.nodeOffset(offset)); pln.valid() {
		return pln.node.lookup(pl.g.nodeIndex(offset))
	}
	return nil
}

// LookupMutable is Lookup with a PageOrMarkerRef, which is not Valid when no
// node holds the offset. Other changes need LookupOrAllocate.
func (pl *PageList[P]) LookupMutable(offset uint64) PageOrMarkerRef[P] {
	if pln := pl.find(pl.g.nodeOffset(offset)); pln.valid() {
		return PageOrMarkerRef[P]{slot: pln.node.lookup(pl.g.nodeIndex(offset))}
	}
	return PageOrMarkerRef[P]{}
}

// LookupMutableCursor is a cursor at offset, which is not valid when no node
// holds it.
func (pl *PageList[P]) LookupMutableCursor(offset uint64) Cursor[P] {
	pln := pl.find(pl.g.nodeOffset(offset))
	if !pln.valid() {
		return invalidCursor(pl)
	}
	return Cursor[P]{list: pl, node: pln, index: pl.g.nodeIndex(offset)}
}

// LookupNearestMutableCursor is a cursor at the first slot at or after offset
// that a node holds, if there is one.
func (pl *PageList[P]) LookupNearestMutableCursor(offset uint64) Cursor[P] {
	nodeOffset := pl.g.nodeOffset(offset)
	if pln := pl.lowerBound(nodeOffset); pln.valid() {
		var index uint64
		if pln.offset == nodeOffset {
			index = pl.g.nodeIndex(offset)
		}
		return Cursor[P]{list: pl, node: pln, index: index}
	}
	return invalidCursor(pl)
}

// IntervalHandling is how LookupOrAllocate treats an offset in an interval.
type IntervalHandling uint8

const (
	// NoIntervals says the list holds no intervals, so every slot may be
	// changed on its own and no check is made.
	NoIntervals IntervalHandling = iota
	// CheckForInterval reports whether the offset is in an interval, and
	// returns a slot only if it can be changed on its own.
	CheckForInterval
	// SplitInterval splits an interval around the offset, so the slot it
	// returns may be changed freely. See lookupOrAllocateCheckForInterval.
	SplitInterval
)

// LookupOrAllocate is the slot at offset, allocating its node if need be, and
// whether the offset is in an interval. It returns no slot only for an offset
// at or past MaxSize, or for one in an interval that handling does not allow
// splitting.
//
// The slot may be changed freely, except that a slot that was not Empty must
// not be made Empty: RemoveContent does that. A slot that was Empty and is
// left Empty must be given back with ReturnEmptySlot, so no empty node stays.
func (pl *PageList[P]) LookupOrAllocate(offset uint64, handling IntervalHandling) (*PageOrMarker[P], bool) {
	switch handling {
	case NoIntervals:
		// The list holds no intervals, so there is nothing to check.
		return pl.lookupOrAllocateInternal(offset), false
	case CheckForInterval:
		return pl.lookupOrAllocateCheckForInterval(offset, false)
	case SplitInterval:
		return pl.lookupOrAllocateCheckForInterval(offset, true)
	}
	return nil, false
}

// BatchInserter is LookupOrAllocate with NoIntervals for offsets near each
// other: in the same node or the next ones. While it is in use nothing else
// may change the list, or Reset must be called before it is used again.
type BatchInserter[P any] struct {
	list *PageList[P]
	node nodeEntry[P]
}

// NewBatchInserter is a BatchInserter over the list.
func (pl *PageList[P]) NewBatchInserter() *BatchInserter[P] {
	return &BatchInserter[P]{list: pl}
}

// Reset makes the inserter safe to use again after the list was changed.
func (b *BatchInserter[P]) Reset() { b.node = nodeEntry[P]{} }

// LookupOrAllocate is PageList.LookupOrAllocate with NoIntervals. It is
// correct for any offset, and faster when each is near the one before.
func (b *BatchInserter[P]) LookupOrAllocate(offset uint64) *PageOrMarker[P] {
	g := b.list.g
	target := g.nodeOffset(offset)
	// Assume a search for the node is needed.
	search := true
	// First see whether the node held is the one wanted.
	if b.node.valid() {
		if b.node.offset == target {
			return b.node.node.lookup(g.nodeIndex(offset))
		}
		// Offsets are assumed to rise, so the next node may be the one.
		if b.node.offset < target {
			b.node = b.list.next(b.node)
			if !b.node.valid() {
				// Past the last node, so the node wanted is not in the tree.
				search = false
			} else if b.node.offset == target {
				return b.node.node.lookup(g.nodeIndex(offset))
			}
		}
	}
	// Unless the node is known not to be in the tree, search for it so it is
	// not inserted twice.
	if search {
		b.node = b.list.lowerBound(target)
		if b.node.valid() && b.node.offset == target {
			return b.node.node.lookup(g.nodeIndex(offset))
		}
	}
	// Zircon returns nullptr here when the node cannot be allocated or the
	// tree cannot insert it. A Go allocation does not fail.
	node := new(pageListNode[P])
	slot := node.lookup(g.nodeIndex(offset))
	b.node = b.list.insert(target, node)
	return slot
}

// RemoveAllContent moves every slot's content to free, which owns it from
// then on, and leaves the list empty.
func (pl *PageList[P]) RemoveAllContent(free func(content PageOrMarker[P])) {
	_ = pl.forEveryPage(func(p *PageOrMarker[P], _ uint64) error {
		free(p.Take())
		return nil
	})
	pl.Clear()
}

// Clear removes every node. It panics if a slot still holds a page or a
// reference, which would be lost.
func (pl *PageList[P]) Clear() {
	pl.list.Ascend(func(e nodeEntry[P]) bool {
		assert(e.node.hasNoPageOrRef(), "a cleared node owns no page or reference")
		return true
	})
	pl.list.Clear(false)
}

// RemovePages calls fn on every slot in [start, end) that is not Empty. fn may
// change the slot and take its page, and nodes it leaves empty are freed.
func (pl *PageList[P]) RemovePages(fn func(p *PageOrMarker[P], offset uint64) error, start, end uint64) error {
	return pl.forEveryPageInRange(fn, start, end, true)
}

// RemovePagesAndIterateGaps is RemovePages that also calls gapFn on the gaps,
// as ForEveryPageAndGapInRange does.
func (pl *PageList[P]) RemovePagesAndIterateGaps(pageFn func(p *PageOrMarker[P], offset uint64) error,
	gapFn func(start, end uint64) error, start, end uint64) error {
	return pl.forEveryPageAndGapInRange(pageFn, gapFn, start, end, true)
}

// IsEmpty reports whether the list holds no page, reference, marker or
// interval.
func (pl *PageList[P]) IsEmpty() bool { return pl.list.Len() == 0 }

// MergeRangeOnto moves [offset, endOffset) of this list onto other, with
// offset here at 0 there. For every slot in the range that is not Empty,
// migrate is called with the slot and its slot in other. Zircon returns false
// when it cannot allocate a node in other; a Go allocation does not fail, so
// this returns nothing.
func (pl *PageList[P]) MergeRangeOnto(migrate func(src, dst *PageOrMarker[P], otherOffset uint64), other *PageList[P],
	offset, endOffset uint64) {
	// Zircon's lists share one page size. These may not, and must.
	assert(other.g == pl.g, "the lists have one page size")
	g := pl.g
	for iter := pl.lowerBound(g.nodeOffset(offset)); iter.valid() && iter.offset < endOffset; {
		nodeObjOffset, node := iter.offset, iter.node
		assert(node.hasNoIntervalSentinel(), "a merged node holds no interval")
		// The part of the range in node.
		nodeStart := max(nodeObjOffset, offset)
		nodeEnd := min(g.endOffset(nodeObjOffset), endOffset)
		// If offset is not node aligned, node's slots go to two nodes in other.
		for nodeStart < nodeEnd {
			otherStart := nodeStart - offset
			otherObjOffset := g.nodeOffset(otherStart)
			curOther := other.lowerBound(otherObjOffset)
			if !curOther.valid() || curOther.offset != otherObjOffset {
				// Zircon returns false here when the node cannot be allocated
				// or inserted. A Go allocation does not fail.
				curOther = other.insert(otherObjOffset, new(pageListNode[P]))
			}
			// The end of curOther may come before the end of the range in node.
			otherEnd := min(g.endOffset(otherObjOffset), nodeEnd-offset)
			length := otherEnd - otherStart
			node.mergeRangeOnto(g, nodeObjOffset, otherObjOffset, migrate, curOther.node,
				nodeStart, nodeStart+length, otherStart)
			// Nothing moved, or migrate left nothing: remove curOther.
			if curOther.node.isEmpty() {
				other.erase(curOther)
			}
			nodeStart += length
		}
		// node keeps content migrate left in it, or outside the range.
		if node.isEmpty() {
			iter = pl.eraseNext(iter)
		} else {
			iter = pl.next(iter)
		}
	}
}

// HeapAllocationBytes is the memory the list's nodes take. Zircon counts its
// tree's own nodes from the tree; google/btree does not report them, so this
// counts each page list node and its entry in the tree.
func (pl *PageList[P]) HeapAllocationBytes() uint64 {
	perNode := unsafe.Sizeof(pageListNode[P]{}) + unsafe.Sizeof(nodeEntry[P]{})
	return uint64(pl.list.Len()) * uint64(perNode)
}

// AddZeroInterval adds a zero interval over [start, end), which must hold
// nothing, in the given dirty state. It joins intervals beside it in the same
// state.
func (pl *PageList[P]) AddZeroInterval(start, end uint64, state IntervalDirtyState) error {
	return pl.addZeroIntervalInternal(start, end, state, 0, false)
}

func (pl *PageList[P]) lookupOrAllocateInternal(offset uint64) *PageOrMarker[P] {
	nodeOffset := pl.g.nodeOffset(offset)
	index := pl.g.nodeIndex(offset)
	if nodeOffset >= pl.g.maxSize() {
		return nil
	}
	// Zircon looks up with lower_bound rather than find so a failed lookup
	// leaves a hint for the insert below.
	if pln := pl.lowerBound(nodeOffset); pln.valid() && pln.offset == nodeOffset {
		return pln.node.lookup(index)
	}
	// Zircon returns nullptr here when the node cannot be allocated or the
	// tree cannot insert it. A Go allocation does not fail.
	node := new(pageListNode[P])
	pl.insert(nodeOffset, node)
	return node.lookup(index)
}

// ReturnEmptySlot gives back a slot LookupOrAllocate just returned Empty and
// that is still Empty, so a node it allocated is freed.
func (pl *PageList[P]) ReturnEmptySlot(offset uint64) {
	pln := pl.find(pl.g.nodeOffset(offset))
	assert(pln.valid(), "the slot's node is in the list")
	page := pln.node.lookup(pl.g.nodeIndex(offset)).Take()
	assert(page.IsEmpty(), "a returned slot is Empty")
	if pln.node.isEmpty() {
		pl.erase(pln)
	}
}

// RemoveContent moves out whatever is at offset, which is Empty if nothing is.
func (pl *PageList[P]) RemoveContent(offset uint64) PageOrMarker[P] {
	pln := pl.find(pl.g.nodeOffset(offset))
	if !pln.valid() {
		return Empty[P]()
	}
	page := pln.node.lookup(pl.g.nodeIndex(offset)).Take()
	// The last slot of the node goes, and the node with it.
	if !page.IsEmpty() && pln.node.isEmpty() {
		pl.erase(pln)
	}
	return page
}

// HasNoPageOrRef reports whether the list owns no page or reference, nothing
// that must be given back.
func (pl *PageList[P]) HasNoPageOrRef() bool {
	noPages := true
	_ = pl.ForEveryPage(func(p *PageOrMarker[P], _ uint64) error {
		if p.IsPageOrRef() {
			noPages = false
			return ErrStop
		}
		return nil
	})
	return noPages
}

// HasNoPageRefOrMarker is HasNoPageOrRef that counts a marker too.
func (pl *PageList[P]) HasNoPageRefOrMarker() bool {
	noPages := true
	_ = pl.ForEveryPage(func(p *PageOrMarker[P], _ uint64) error {
		if p.IsPageOrRef() || p.IsMarker() {
			noPages = false
			return ErrStop
		}
		return nil
	})
	return noPages
}

// findIntervalStartForEnd is the start of the interval that ends at end, and
// its offset.
func (pl *PageList[P]) findIntervalStartForEnd(end uint64) (*PageOrMarker[P], uint64) {
	// The node that would hold the end.
	pln := pl.find(pl.g.nodeOffset(end))
	assert(pln.valid(), "the end's node is in the list")
	nodeIndex := pl.g.nodeIndex(end)
	assert(pln.node.lookup(nodeIndex).IsIntervalEnd(), "the slot is an interval end")
	// An interval populates only its start and its end. So the start is in
	// the end's node or in the populated node before it.
	for index := nodeIndex; index > 0; {
		index--
		slot := pln.node.lookup(index)
		if !slot.IsEmpty() {
			assert(slot.IsIntervalStart(), "the slot before an end is its start")
			return slot, pln.offset + index*pl.g.pageSize()
		}
	}
	// Not in the end's node, so in the one before.
	pln = pl.prev(pln)
	assert(pln.valid(), "an interval end has a start before it")
	for index := uint64(pageFanOut); index >= 1; index-- {
		slot := pln.node.lookup(index - 1)
		if !slot.IsEmpty() {
			assert(slot.IsIntervalStart(), "the slot before an end is its start")
			return slot, pln.offset + (index-1)*pl.g.pageSize()
		}
	}
	panic("zirconvm: an interval end has no start")
}

// findIntervalEndForStart is the end of the interval that starts at start, and
// its offset.
func (pl *PageList[P]) findIntervalEndForStart(start uint64) (*PageOrMarker[P], uint64) {
	// The node that would hold the start.
	pln := pl.find(pl.g.nodeOffset(start))
	assert(pln.valid(), "the start's node is in the list")
	nodeIndex := pl.g.nodeIndex(start)
	assert(pln.node.lookup(nodeIndex).IsIntervalStart(), "the slot is an interval start")
	// An interval populates only its start and its end. So the end is in the
	// start's node or in the populated node after it.
	for index := nodeIndex; index < pageFanOut-1; {
		index++
		slot := pln.node.lookup(index)
		if !slot.IsEmpty() {
			assert(slot.IsIntervalEnd(), "the slot after a start is its end")
			return slot, pln.offset + index*pl.g.pageSize()
		}
	}
	// Not in the start's node, so in the one after.
	pln = pl.next(pln)
	assert(pln.valid(), "an interval start has an end after it")
	for index := uint64(0); index < pageFanOut; index++ {
		slot := pln.node.lookup(index)
		if !slot.IsEmpty() {
			assert(slot.IsIntervalEnd(), "the slot after a start is its end")
			return slot, pln.offset + index*pl.g.pageSize()
		}
	}
	panic("zirconvm: an interval start has no end")
}

// lookupOrAllocateCheckForInterval is lookupOrAllocateInternal that also
// reports whether offset is in an interval, and splits the interval around
// offset if split is set, so the slot can be changed freely. It returns:
//  1. (slot, false): offset is in no interval.
//  2. (slot, true): offset is in an interval and split was set. The interval
//     is split around the slot, which can be treated as if it were in none.
//  3. (nil, true): offset is in an interval and split was not set. No slot is
//     returned, since one in an interval cannot safely be changed without
//     splitting the interval around it.
//
// Splitting [start, end), where start < offset < end, leaves three intervals:
// [start, offset), [offset, offset+pageSize) and [offset+pageSize, end). The
// middle one is a single Slot sentinel, which can be changed on its own.
func (pl *PageList[P]) lookupOrAllocateCheckForInterval(offset uint64, split bool) (*PageOrMarker[P], bool) {
	g := pl.g
	// The node that would hold offset.
	nodeOffset := g.nodeOffset(offset)
	nodeIndex := g.nodeIndex(offset)
	if nodeOffset >= g.maxSize() {
		return nil, false
	}

	// The lower bound is the node holding offset if there is one, and the
	// next node otherwise. The aim is as few tree lookups as LookupOrAllocate:
	// hold on to this node and walk left or right only if need be. Within a
	// node too, the order of the checks lets the walk end as soon as it can.
	pln := pl.lowerBound(nodeOffset)

	// The slot that will hold offset.
	var slot *PageOrMarker[P]

	// A sentinel of the interval offset is in, if it is in one. It is the
	// model for new sentinels if the interval is split.
	var foundInterval *PageOrMarker[P]
	inInterval := false
	// An interval has an end, so if there is no node here or after, offset
	// cannot be in one and there is nothing to check.
	if pln.valid() {
		if pln.offset == nodeOffset {
			// The node holding offset.
			slot = pln.node.lookup(nodeIndex)
			// A sentinel at offset settles it. This only saves the call
			// below, which would find the same.
			if slot.IsInterval() {
				inInterval = true
				foundInterval = slot
			}
		}

		if !inInterval {
			foundInterval = pl.isOffsetInIntervalHelper(offset, pln)
			inInterval = foundInterval != nil
			// An interval found has a sentinel found.
			assert(!inInterval || foundInterval.IsInterval(), "the interval found has a sentinel")
		}

		// In an interval that may not be split, no slot can be changed on
		// its own.
		if inInterval && !split {
			return nil, true
		}
	}

	// No slot if the node found does not hold offset.
	if slot == nil {
		// Zircon returns (nullptr, is_in_interval) here when the node
		// cannot be allocated or inserted. A Go allocation does not fail.
		node := new(pageListNode[P])
		slot = node.lookup(nodeIndex)
		pln = pl.insert(nodeOffset, node)
	}

	// Outside an interval, or at a single page interval already, the slot is
	// ready.
	if !inInterval || slot.IsIntervalSlot() {
		// Only zero intervals are supported.
		assert(!inInterval || slot.IsIntervalZero(), "the interval is a zero interval")
		return slot, inInterval
	}

	// In an interval, which must be split to return the slot.
	assert(inInterval && split, "the interval is to be split")
	assert(pln.valid(), "the slot's node is in the list")

	// An Empty slot needs an end to its left, a start to its right, and a
	// Slot at offset: a new start and a new end. A populated slot in an
	// interval is its start or its end, and needs one or the other.
	needNewEnd, needNewStart := true, true
	if slot.IsIntervalStart() {
		// Move the start right and make the old start a Slot. No new end.
		needNewEnd = false
	} else if slot.IsIntervalEnd() {
		// Move the end left and make the old end a Slot. No new start.
		needNewStart = false
	}

	// Find the slots the new end and the new start go in. At most one of the
	// two nodes below is allocated, as which depends on nodeIndex. Zircon
	// gives back the slot at offset when that allocation fails; a Go
	// allocation does not fail.
	var newEnd *PageOrMarker[P]
	if needNewEnd {
		if nodeIndex > 0 {
			// The slot before is in the same node.
			newEnd = pln.node.lookup(nodeIndex - 1)
		} else {
			// The slot before is in the node to the left, which may need
			// allocating. The slot is Empty in an interval or is its end, and
			// is the first of its node, so a node to the left holds the
			// interval's start.
			iter := pl.prev(pln)
			assert(iter.valid(), "a node to the left holds the interval's start")
			prevNodeOffset := nodeOffset - g.nodeSize()
			if iter.offset == prevNodeOffset {
				newEnd = iter.node.lookup(pageFanOut - 1)
			} else {
				assert(iter.offset < prevNodeOffset, "the node found is to the left")
				node := new(pageListNode[P])
				newEnd = node.lookup(pageFanOut - 1)
				plnKey := pln.offset
				pl.insert(prevNodeOffset, node)
				// Zircon's insert moves the tree's iterators, so it finds the
				// slot's node again.
				pln = pl.find(plnKey)
				assert(pln.valid(), "the slot's node is in the list")
			}
		}
		assert(newEnd != nil, "there is a slot for the new end")
	}

	var newStart *PageOrMarker[P]
	if needNewStart {
		if nodeIndex < pageFanOut-1 {
			// The slot after is in the same node.
			newStart = pln.node.lookup(nodeIndex + 1)
		} else {
			// The slot after is in the node to the right, which may need
			// allocating. The slot is Empty or the interval's start, and is
			// the last of its node, so a node to the right holds the
			// interval's end.
			iter := pl.next(pln)
			assert(iter.valid(), "a node to the right holds the interval's end")
			nextNodeOffset := nodeOffset + g.nodeSize()
			if iter.offset == nextNodeOffset {
				newStart = iter.node.lookup(0)
			} else {
				assert(iter.offset > nextNodeOffset, "the node found is to the right")
				node := new(pageListNode[P])
				newStart = node.lookup(0)
				pl.insert(nextNodeOffset, node)
			}
		}
		assert(newStart != nil, "there is a slot for the new start")
	}

	// New sentinels for the split. Only zero intervals are made; another
	// kind of interval would need this changed.
	mint := func(sentinel SentinelType) PageOrMarker[P] {
		assert(foundInterval.IsIntervalZero(), "the interval is a zero interval")
		// The dirty state carries across the split.
		return zeroInterval[P](sentinel, foundInterval.GetZeroIntervalDirtyState(), g.shift)
	}

	// With every slot found and allocated, make the change: an end to the
	// left of offset and a start to the right.
	if newStart != nil {
		if newStart.IsIntervalEnd() {
			// An interval ended at the next slot: it becomes a Slot.
			newStart.changeIntervalSentinel(SentinelSlot)
		} else {
			assert(newStart.IsEmpty(), "the new start's slot is Empty")
			newStart.Set(mint(SentinelStart))
		}
	}
	if newEnd != nil {
		if newEnd.IsIntervalStart() {
			// An interval started at the previous slot: it becomes a Slot.
			newEnd.changeIntervalSentinel(SentinelSlot)
		} else {
			assert(newEnd.IsEmpty(), "the new end's slot is Empty")
			newEnd.Set(mint(SentinelEnd))
		}
	}

	// Last, a Slot at offset.
	if slot.IsEmpty() {
		slot.Set(mint(SentinelSlot))
	} else {
		assert(slot.IsIntervalStart() || slot.IsIntervalEnd(), "the slot is the interval's start or end")
		// Over a start or an end, carry what the rest of the interval needs.
		// For a zero interval that is a nonzero AwaitingClean length in its
		// start, which matters only when the split is at the start. That saves
		// walking to another node for the start to update, so the length can
		// be longer than the interval left: the caller carries such lengths
		// across intervals (see VmCowPages::WritebackEndLocked).
		if slot.IsIntervalStart() {
			awaitingCleanLen := slot.GetZeroIntervalAwaitingCleanLength()
			if awaitingCleanLen > g.pageSize() {
				newStart.SetZeroIntervalAwaitingCleanLength(awaitingCleanLen - g.pageSize())
				slot.SetZeroIntervalAwaitingCleanLength(g.pageSize())
			}
		}
		slot.changeIntervalSentinel(SentinelSlot)
	}

	return slot, true
}

// ReturnIntervalSlot gives back an interval Slot that PopulateSlotsInInterval
// or a split made and that was not used, so it joins the interval it came
// from.
func (pl *PageList[P]) ReturnIntervalSlot(offset uint64) {
	// The slot is there already.
	slot := pl.lookupOrAllocateInternal(offset)
	assert(slot != nil, "the interval slot is in the list")
	assert(slot.IsIntervalSlot(), "the slot is an interval slot")

	// Only zero intervals are supported. Another kind would be handled here.
	assert(slot.IsIntervalZero(), "the slot is a zero interval")
	state := slot.GetZeroIntervalDirtyState()
	awaitingCleanLen := slot.GetZeroIntervalAwaitingCleanLength()
	// Empty the slot and add a zero interval at it, which joins it to the
	// intervals beside it. The slot is reused, so it is not given back here.
	slot.Take()
	err := pl.addZeroIntervalInternal(offset, offset+pl.g.pageSize(), state, awaitingCleanLen, true)
	// A slot that is reused needs no node, so this cannot fail.
	assert(err == nil, "returning an interval slot needs no node")
}

// PopulateSlotsInInterval makes every offset in [start, end), which is in an
// interval, a Slot of its own, so that none of them fails a later lookup and
// each can be replaced with a page. It is the split of
// lookupOrAllocateCheckForInterval at every offset of the range, without a
// tree search for each.
func (pl *PageList[P]) PopulateSlotsInInterval(start, end uint64) error {
	g := pl.g
	ps := g.pageSize()
	assert(g.isPageRounded(start), "the start is page rounded")
	assert(g.isPageRounded(end), "the end is page rounded")
	assert(end > start, "the range is not empty")
	// The end is inclusive from here on.
	end -= ps

	// Both ends are in an interval, and every offset between them is Empty
	// and in the same interval: no page or gap between them.
	assert(pl.isOffsetInInterval(start), "the start is in an interval")
	assert(pl.isOffsetInInterval(end), "the end is in an interval")
	if start+ps < end {
		err := pl.ForEveryPageAndGapInRange(
			func(*PageOrMarker[P], uint64) error { return errBadState },
			func(uint64, uint64) error { return errBadState },
			start+ps, end)
		assert(err == nil, "the range is one interval")
	}

	// Split the interval around start and end first. If a later step fails,
	// these slots are given back: the call populates every slot asked for,
	// or leaves the interval as it was.
	startSlot, startInInterval := pl.lookupOrAllocateCheckForInterval(start, true)
	if startSlot == nil {
		return ErrNoMemory
	}
	assert(startInInterval, "the start is in an interval")
	assert(startSlot.IsIntervalSlot(), "the start is a Slot")
	// A single slot was asked for.
	if start == end {
		return nil
	}

	endSlot, endInInterval := pl.lookupOrAllocateCheckForInterval(end, true)
	if endSlot == nil {
		// Give back the start before failing.
		pl.ReturnIntervalSlot(start)
		return ErrNoMemory
	}
	assert(endInInterval, "the end is in an interval")
	assert(endSlot.IsIntervalSlot(), "the end is a Slot")
	// Only zero intervals are supported, and both ends are in one state.
	assert(startSlot.GetZeroIntervalDirtyState() == endSlot.GetZeroIntervalDirtyState(),
		"both ends are in one state")

	// No slot left between the two.
	if end == start+ps {
		return nil
	}

	// Every offset between start and end becomes a Slot. First the nodes in
	// between are allocated. After the splits, the node holding start+ps holds
	// the interval's new start and the node holding end-ps its new end, so
	// any node missing lies between those two.
	firstNodeOffset := g.nodeOffset(start + ps)
	firstNodeIndex := g.nodeIndex(start + ps)
	lastNodeOffset := g.nodeOffset(end - ps)
	lastNodeIndex := g.nodeIndex(end - ps)
	assert(lastNodeOffset >= firstNodeOffset, "the nodes are in order")
	if lastNodeOffset > firstNodeOffset+g.nodeSize() {
		firstUnpopulated := firstNodeOffset + g.nodeSize()
		lastUnpopulated := lastNodeOffset - g.nodeSize()
		for nodeOffset := firstUnpopulated; nodeOffset <= lastUnpopulated; nodeOffset += g.nodeSize() {
			// Zircon frees the nodes it added and gives back both ends when
			// a node cannot be allocated or inserted. A Go allocation does
			// not fail.
			pl.insert(nodeOffset, new(pageListNode[P]))
		}
	}

	// With every node allocated, nothing below can fail. Every offset after
	// start and before end becomes a Slot.
	nodeOffset := firstNodeOffset
	pln := pl.find(nodeOffset)
	// This has to do what lookupOrAllocateCheckForInterval would at each
	// offset, AwaitingClean length included. After the split at start, the
	// slot after it may carry a nonzero AwaitingClean length for the
	// interval after it. That length moves to the slot after the last one
	// populated, less every slot populated on the way.
	//
	// For example, populating three slots from an interval start with an
	// AwaitingClean length of five pages leaves lengths of [1, 1, 1, 2] pages
	// on the three slots and the rest of the interval. From a length of two
	// it leaves [1, 1, 0, 0].
	awaitingCleanLen := pln.node.lookup(firstNodeIndex).GetZeroIntervalAwaitingCleanLength()
	for nodeOffset <= lastNodeOffset {
		assert(pln.valid(), "the node is in the list")
		assert(pln.offset == nodeOffset, "the nodes are consecutive")
		first := uint64(0)
		if nodeOffset == firstNodeOffset {
			first = firstNodeIndex
		}
		last := uint64(pageFanOut - 1)
		if nodeOffset == lastNodeOffset {
			last = lastNodeIndex
		}
		for index := first; index <= last; index++ {
			cur := pln.node.lookup(index)
			cur.Set(zeroInterval[P](SentinelSlot, startSlot.GetZeroIntervalDirtyState(), g.shift))
			if awaitingCleanLen > 0 {
				cur.SetZeroIntervalAwaitingCleanLength(ps)
				awaitingCleanLen -= ps
			}
		}
		pln = pl.next(pln)
		nodeOffset += g.nodeSize()
	}

	if awaitingCleanLen > 0 {
		// The last slot populated has its length too.
		pl.LookupMutable(end).SetZeroIntervalAwaitingCleanLength(ps)
		awaitingCleanLen -= ps
		// What is left goes to the interval after the last slot, if there
		// is one.
		if awaitingCleanLen > 0 {
			next := pl.LookupMutable(end + ps)
			if next.Valid() && (next.Get().IsIntervalStart() || next.Get().IsIntervalSlot()) {
				oldLen := next.Get().GetZeroIntervalAwaitingCleanLength()
				next.SetZeroIntervalAwaitingCleanLength(max(oldLen, awaitingCleanLen))
			}
		}
	}

	// Every offset in [start, end] is a Slot.
	nextOff := start
	err := pl.ForEveryPageInRange(func(p *PageOrMarker[P], off uint64) error {
		if off != nextOff || !p.IsIntervalSlot() {
			return errBadState
		}
		nextOff += ps
		return nil
	}, start, end+ps)
	assert(err == nil, "every offset in the range is a Slot")

	return nil
}

// errBadState is Zircon's ZX_ERR_BAD_STATE, which its own checks of the list
// return from a walk to fail it.
var errBadState = errors.New("zirconvm: bad state")

// IsOffsetInZeroInterval reports whether offset is in a zero interval.
func (pl *PageList[P]) IsOffsetInZeroInterval(offset uint64) bool {
	// The node holding offset if there is one, and the next node otherwise.
	pln := pl.lowerBound(pl.g.nodeOffset(offset))
	// No node here or after, so no interval end: offset is in no interval.
	if !pln.valid() {
		return false
	}
	// The list holds no empty node.
	assert(!pln.node.isEmpty(), "the list holds no empty node")

	// Whether offset is in an interval, and that interval's sentinel.
	interval := pl.isOffsetInIntervalHelper(offset, pln)
	assert(interval == nil || interval.IsInterval(), "the sentinel is an interval")
	if interval == nil {
		return false
	}
	return interval.IsIntervalZero()
}

// isOffsetInInterval reports whether offset is in an interval.
func (pl *PageList[P]) isOffsetInInterval(offset uint64) bool {
	// The node holding offset if there is one, and the next node otherwise.
	pln := pl.lowerBound(pl.g.nodeOffset(offset))
	// No node here or after, so no interval end: offset is in no interval.
	if !pln.valid() {
		return false
	}
	// The list holds no empty node.
	assert(!pln.node.isEmpty(), "the list holds no empty node")
	interval := pl.isOffsetInIntervalHelper(offset, pln)
	assert(interval == nil || interval.IsInterval(), "the sentinel is an interval")
	return interval != nil
}

// isOffsetInIntervalHelper returns the sentinel of the interval offset is in,
// or nil. lowerBound is the node the caller found with a lower bound lookup
// of offset's node, which saves another lookup.
func (pl *PageList[P]) isOffsetInIntervalHelper(offset uint64, lowerBound nodeEntry[P]) *PageOrMarker[P] {
	if offset < lowerBound.offset {
		return lowerBound.node.nodeStartsInInterval()
	}
	return lowerBound.node.isOffsetInInterval(pl.g, lowerBound.offset, offset)
}

// addZeroIntervalInternal is AddZeroInterval. replaceExistingSlot may be set
// when the interval is a single page whose slot is already allocated and
// Empty, so it is reused.
func (pl *PageList[P]) addZeroIntervalInternal(start, end uint64, state IntervalDirtyState, awaitingCleanLen uint64,
	replaceExistingSlot bool) error {
	g := pl.g
	ps := g.pageSize()
	assert(g.isPageRounded(start), "the start is page rounded")
	assert(g.isPageRounded(end), "the end is page rounded")
	assert(start < end, "the interval is not empty")
	assert(!replaceExistingSlot || end == start+ps, "a reused slot is a single page")
	// A reused slot may be in an empty node, which no walk of the list
	// expects, so the range cannot be walked then. The slot is checked to be
	// Empty below instead, and its node given back if it is not used.
	assert(replaceExistingSlot || !pl.AnyPagesOrIntervalsInRange(start, end), "the range holds nothing")
	assert(awaitingCleanLen == 0 || state == IntervalDirty, "only a Dirty interval is AwaitingClean")

	intervalStart := start
	intervalEnd := end - ps
	prevOffset := intervalStart - ps
	nextOffset := intervalEnd + ps

	// The slot at an offset, without allocating.
	lookupSlot := func(offset uint64) *PageOrMarker[P] {
		pln := pl.find(g.nodeOffset(offset))
		if !pln.valid() {
			return nil
		}
		return pln.node.lookup(g.nodeIndex(offset))
	}

	// Whether the interval joins one before it.
	mergeWithPrev := false
	var prevSlot *PageOrMarker[P]
	// The start, and its offset, after the merge, for the AwaitingClean
	// length.
	var finalStart PageOrMarkerRef[P]
	var finalStartOffset uint64
	if intervalStart > 0 {
		prevSlot = lookupSlot(prevOffset)
		// A zero interval end or slot to the left in the same state joins.
		if prevSlot != nil && prevSlot.IsIntervalZero() &&
			(prevSlot.IsIntervalEnd() || prevSlot.IsIntervalSlot()) &&
			prevSlot.GetZeroIntervalDirtyState() == state {
			mergeWithPrev = true

			// The new length is joined to the left interval's below, so
			// note that interval's start before the list changes.
			if awaitingCleanLen > 0 {
				if prevSlot.IsIntervalSlot() {
					finalStart = PageOrMarkerRef[P]{slot: prevSlot}
					finalStartOffset = prevOffset
				} else {
					_, off := pl.findIntervalStartForEnd(prevOffset)
					finalStartOffset = off
					// A second lookup, for a mutable slot. This case is rare,
					// so the cost does not matter.
					finalStart = pl.LookupMutable(finalStartOffset)
				}
			}
		}
	}

	// Whether the interval joins one after it: a zero interval start or slot
	// to the right in the same state.
	mergeWithNext := false
	nextSlot := lookupSlot(nextOffset)
	if nextSlot != nil && nextSlot.IsIntervalZero() &&
		(nextSlot.IsIntervalStart() || nextSlot.IsIntervalSlot()) &&
		nextSlot.GetZeroIntervalDirtyState() == state {
		mergeWithNext = true
	}

	// Allocate the slots the interval needs first.
	var newStart, newEnd *PageOrMarker[P]
	// No interval to the left to join, so a new start.
	if !mergeWithPrev {
		newStart = pl.lookupOrAllocateInternal(intervalStart)
		if newStart == nil {
			assert(!replaceExistingSlot, "a reused slot is there")
			return ErrNoMemory
		}
		assert(newStart.IsEmpty(), "the new start's slot is Empty")
	}
	// No interval to the right to join, so a new end.
	if !mergeWithNext {
		newEnd = pl.lookupOrAllocateInternal(intervalEnd)
		if newEnd == nil {
			assert(!replaceExistingSlot, "a reused slot is there")
			// Give back the start's slot before failing.
			if newStart != nil {
				assert(newStart.IsEmpty(), "the new start's slot is Empty")
				pl.ReturnEmptySlot(intervalStart)
			}
			return ErrNoMemory
		}
		assert(newEnd.IsEmpty(), "the new end's slot is Empty")
	}
	// A reused slot that joins both sides is not needed. Giving it back is not
	// strictly needed, but says so. It shares a node with the slot before or
	// the slot after, so its node is freed with theirs or keeps them.
	if replaceExistingSlot && mergeWithPrev && mergeWithNext {
		pl.ReturnEmptySlot(intervalStart)
	}

	// Every error has been checked for. Now the change.
	if mergeWithPrev {
		// Join the new length to the left interval's.
		if awaitingCleanLen > 0 {
			oldLen := finalStart.Get().GetZeroIntervalAwaitingCleanLength()
			// Only if no gap lies between the left interval's AwaitingClean
			// range and the new interval.
			if finalStartOffset+oldLen >= intervalStart {
				finalStart.SetZeroIntervalAwaitingCleanLength(
					max(finalStartOffset+oldLen, intervalStart+awaitingCleanLen) - finalStartOffset)
			}
		}
		if prevSlot.IsIntervalEnd() {
			// The left interval extends over the new one: its old end goes.
			prevSlot.Set(Empty[P]())
		} else {
			// A Slot to the left extends too, and becomes a start.
			assert(prevSlot.IsIntervalSlot(), "the slot to the left is a Slot")
			assert(prevSlot.GetZeroIntervalDirtyState() == state, "the slot to the left is in the state")
			prevSlot.changeIntervalSentinel(SentinelStart)
		}
	} else {
		// Nothing to join on the left: a new interval starts.
		assert(newStart.IsEmpty(), "the new start's slot is Empty")
		newStart.Set(zeroInterval[P](SentinelStart, state, g.shift))
		if awaitingCleanLen > 0 {
			finalStart = PageOrMarkerRef[P]{slot: newStart}
			finalStartOffset = intervalStart
			newStart.SetZeroIntervalAwaitingCleanLength(awaitingCleanLen)
		}
	}

	if mergeWithNext {
		// Join the right interval's AwaitingClean length to the one built so
		// far on the left. The two could be joined even with no new length,
		// when the new interval bridges them, but that case is skipped so the
		// start is not looked up needlessly. The right interval's length is
		// lost instead, as it would be if an interval were only extended on
		// its left.
		if awaitingCleanLen > 0 {
			length := nextSlot.GetZeroIntervalAwaitingCleanLength()
			oldLen := finalStart.Get().GetZeroIntervalAwaitingCleanLength()
			// Only if no gap lies between the AwaitingClean range so far and
			// the right interval.
			if length > 0 && finalStartOffset+oldLen >= nextOffset {
				finalStart.SetZeroIntervalAwaitingCleanLength(
					max(finalStartOffset+oldLen, nextOffset+length) - finalStartOffset)
			}
		}

		if nextSlot.IsIntervalStart() {
			// The right interval's start moves back over the new one: the
			// old start goes.
			nextSlot.Set(Empty[P]())
		} else {
			// A Slot to the right moves back too, and becomes an end.
			assert(nextSlot.IsIntervalSlot(), "the slot to the right is a Slot")
			assert(nextSlot.GetZeroIntervalDirtyState() == state, "the slot to the right is in the state")
			nextSlot.SetZeroIntervalAwaitingCleanLength(0)
			nextSlot.changeIntervalSentinel(SentinelEnd)
		}
	} else {
		// Nothing to join on the right: an end. A single page interval has a
		// start already, which becomes a Slot.
		if newEnd.IsIntervalStart() {
			assert(newEnd.GetZeroIntervalDirtyState() == state, "the start is in the state")
			newEnd.changeIntervalSentinel(SentinelSlot)
		} else {
			assert(newEnd.IsEmpty(), "the new end's slot is Empty")
			newEnd.Set(zeroInterval[P](SentinelEnd, state, g.shift))
		}
	}

	// Give back the slot before or after if the join emptied it.
	returnPrevSlot := mergeWithPrev && prevSlot.IsEmpty()
	returnNextSlot := mergeWithNext && nextSlot.IsEmpty()
	if returnPrevSlot {
		pl.ReturnEmptySlot(prevOffset)
		// The two can share a node, freed already with the slot before.
		if returnNextSlot && g.nodeOffset(prevOffset) == g.nodeOffset(nextOffset) {
			returnNextSlot = false
		}
	}
	if returnNextSlot {
		assert(!returnPrevSlot || g.nodeOffset(prevOffset) != g.nodeOffset(nextOffset),
			"the slot after is in another node")
		pl.ReturnEmptySlot(nextOffset)
	}

	return nil
}

// ReplacePageWithZeroInterval puts a zero interval in place of the page at
// offset, and returns the page, which the caller now owns.
func (pl *PageList[P]) ReplacePageWithZeroInterval(offset uint64, state IntervalDirtyState) *P {
	// The page is there, so its slot is found.
	slot := pl.lookupOrAllocateInternal(offset)
	assert(slot != nil, "the page's slot is in the list")
	// Take the page and keep the Empty slot, for the interval to reuse.
	page := slot.ReleasePage()
	err := pl.addZeroIntervalInternal(offset, offset+pl.g.pageSize(), state, 0, true)
	// It can only fail for want of a node, and the slot is reused.
	assert(err == nil, "replacing a page with an interval needs no node")
	return page
}

// OverwriteZeroInterval overwrites all or part of a zero interval with a new
// zero interval [newStart, newEnd] in newState, splitting the old one in two
// if need be. oldStart and oldEnd are the old interval's start and end
// sentinels; either one may be math.MaxUint64 when not given.
//   - To overwrite all of it, both are given, and equal newStart and newEnd.
//     The new interval replaces the old.
//   - To overwrite its start, oldStart is given and equals newStart, and
//     oldEnd is not. The old interval then starts at newEnd + pageSize.
//   - To overwrite its end, oldEnd is given and equals newEnd, and oldStart
//     is not. The old interval then ends at newStart - pageSize.
//   - Overwriting the middle is not allowed: oldStart is newStart, or oldEnd
//     is newEnd, or both.
func (pl *PageList[P]) OverwriteZeroInterval(oldStartOffset, oldEndOffset, newStartOffset, newEndOffset uint64,
	newState IntervalDirtyState) error {
	g := pl.g
	ps := g.pageSize()
	assert(oldStartOffset == math.MaxUint64 || g.isPageRounded(oldStartOffset), "the old start is page rounded")
	assert(oldEndOffset == math.MaxUint64 || g.isPageRounded(oldEndOffset), "the old end is page rounded")
	assert(g.isPageRounded(newStartOffset), "the new start is page rounded")
	assert(g.isPageRounded(newEndOffset), "the new end is page rounded")
	// Only Dirty and Untracked zero intervals are supported.
	assert(newState == IntervalDirty || newState == IntervalUntracked, "the new interval is Dirty or Untracked")

	// The slot at an offset, without allocating.
	lookupSlot := func(offset uint64) *PageOrMarker[P] {
		pln := pl.find(g.nodeOffset(offset))
		if !pln.valid() {
			return nil
		}
		return pln.node.lookup(g.nodeIndex(offset))
	}

	var oldStart, oldEnd *PageOrMarker[P]
	if oldStartOffset != math.MaxUint64 {
		oldStart = lookupSlot(oldStartOffset)
	}
	if oldEndOffset != math.MaxUint64 {
		oldEnd = lookupSlot(oldEndOffset)
	}
	// The old start or end, or both, is found.
	assert(oldStart != nil || oldEnd != nil, "the old start or end is found")
	// A sentinel found is what it should be.
	assert(oldStart == nil || (oldStart.IsIntervalZero() && (oldStart.IsIntervalStart() || oldStart.IsIntervalSlot())),
		"the old start is a zero interval start or slot")
	// Zircon reads oldStart's sentinel in the last check, as here. With no
	// old start and an old end that is not an End, that is a nil pointer, and
	// the call panics as the assert would.
	assert(oldEnd == nil || (oldEnd.IsIntervalZero() && (oldEnd.IsIntervalEnd() || oldStart.IsIntervalSlot())),
		"the old end is a zero interval end or slot")

	var newStart, newEnd *PageOrMarker[P]
	tryMergeLeft, tryMergeRight := false, false

	// The rest allocates any slots it needs before it changes the list, so a
	// failure leaves the list consistent. Slots not used are given back.
	switch {
	case oldStart != nil && oldEnd != nil:
		// Overwriting the whole interval, in its own slots.
		assert(oldStartOffset == newStartOffset, "the start is kept")
		assert(oldEndOffset == newEndOffset, "the end is kept")
		// The new interval has another state.
		assert(newState != oldStart.GetZeroIntervalDirtyState(), "the state changes")
		assert(oldStart.GetZeroIntervalDirtyState() == oldEnd.GetZeroIntervalDirtyState(),
			"the old start and end are in one state")
		newStart = oldStart
		newEnd = oldEnd
		// In a new state, it may join intervals on both sides.
		tryMergeLeft = true
		tryMergeRight = true
	case oldStart != nil:
		// Clipping at the start.
		assert(oldStartOffset == newStartOffset, "the start is kept")
		// The new interval has another state.
		assert(newState != oldStart.GetZeroIntervalDirtyState(), "the state changes")
		newEnd = pl.lookupOrAllocateInternal(newEndOffset)
		if newEnd == nil {
			return ErrNoMemory
		}
		assert(newStartOffset == newEndOffset || newEnd.IsEmpty(), "the new end's slot is Empty")

		clippedStart := pl.lookupOrAllocateInternal(newEndOffset + ps)
		if clippedStart == nil {
			if newStartOffset != newEndOffset {
				pl.ReturnEmptySlot(newEndOffset)
			}
			return ErrNoMemory
		}
		if clippedStart.IsIntervalEnd() {
			clippedStart.changeIntervalSentinel(SentinelSlot)
		} else {
			assert(clippedStart.IsEmpty(), "the clipped start's slot is Empty")
			clippedStart.Set(zeroInterval[P](SentinelStart, oldStart.GetZeroIntervalDirtyState(), g.shift))
		}

		// With the clipped start made, carry over what is left of the old
		// start's AwaitingClean length.
		oldLen := oldStart.GetZeroIntervalAwaitingCleanLength()
		length := newEndOffset + ps - oldStartOffset
		if oldLen > length {
			clippedStart.SetZeroIntervalAwaitingCleanLength(oldLen - length)
		}

		newStart = oldStart
		// In another state from the old interval, it may join one on the left.
		tryMergeLeft = true
	default:
		// Clipping at the end.
		assert(oldEndOffset == newEndOffset, "the end is kept")
		// The new interval has another state.
		assert(newState != oldEnd.GetZeroIntervalDirtyState(), "the state changes")
		newStart = pl.lookupOrAllocateInternal(newStartOffset)
		if newStart == nil {
			return ErrNoMemory
		}
		assert(newStartOffset == newEndOffset || newStart.IsEmpty(), "the new start's slot is Empty")

		clippedEnd := pl.lookupOrAllocateInternal(newStartOffset - ps)
		if clippedEnd == nil {
			if newStartOffset != newEndOffset {
				pl.ReturnEmptySlot(newStartOffset)
			}
			return ErrNoMemory
		}
		if clippedEnd.IsIntervalStart() {
			clippedEnd.changeIntervalSentinel(SentinelSlot)
		} else {
			assert(clippedEnd.IsEmpty(), "the clipped end's slot is Empty")
			clippedEnd.Set(zeroInterval[P](SentinelEnd, oldEnd.GetZeroIntervalDirtyState(), g.shift))
		}

		newEnd = oldEnd
		// In another state from the old interval, it may join one on the right.
		tryMergeRight = true
	}

	if newStart == newEnd {
		newStart.Set(zeroInterval[P](SentinelSlot, newState, g.shift))
	} else {
		newStart.Set(zeroInterval[P](SentinelStart, newState, g.shift))
		newEnd.Set(zeroInterval[P](SentinelEnd, newState, g.shift))
	}

	if tryMergeLeft {
		left := lookupSlot(newStartOffset - ps)
		if left != nil && left.IsIntervalZero() && left.GetZeroIntervalDirtyState() == newState {
			if left.IsIntervalSlot() {
				left.changeIntervalSentinel(SentinelStart)
			} else {
				assert(left.IsIntervalEnd(), "the slot to the left is an end")
				left.Set(Empty[P]())
				pl.ReturnEmptySlot(newStartOffset - ps)
			}
			if newStart.IsIntervalSlot() {
				newStart.changeIntervalSentinel(SentinelEnd)
			} else {
				assert(newStart.IsIntervalStart(), "the new start is a start")
				newStart.Set(Empty[P]())
				pl.ReturnEmptySlot(newStartOffset)
			}
		}
	}

	if tryMergeRight {
		right := lookupSlot(newEndOffset + ps)
		if right != nil && right.IsIntervalZero() && right.GetZeroIntervalDirtyState() == newState {
			if right.IsIntervalSlot() {
				right.changeIntervalSentinel(SentinelEnd)
			} else {
				assert(right.IsIntervalStart(), "the slot to the right is a start")
				right.Set(Empty[P]())
				pl.ReturnEmptySlot(newEndOffset + ps)
			}
			if newEnd.IsIntervalSlot() {
				newEnd.changeIntervalSentinel(SentinelStart)
			} else {
				assert(newEnd.IsIntervalEnd(), "the new end is an end")
				newEnd.Set(Empty[P]())
				pl.ReturnEmptySlot(newEndOffset)
			}
		}
	}

	return nil
}

// ClipIntervalStart moves an interval's start from intervalStart to
// intervalStart+length. The interval is longer than length.
func (pl *PageList[P]) ClipIntervalStart(intervalStart, length uint64) error {
	g := pl.g
	assert(g.isPageRounded(intervalStart), "the start is page rounded")
	assert(g.isPageRounded(length), "the length is page rounded")
	if length == 0 {
		return nil
	}
	newIntervalStart, carry := bits.Add64(intervalStart, length, 0)
	assert(carry == 0, "the new start does not overflow")

	oldStart := pl.Lookup(intervalStart)
	assert(oldStart.IsIntervalStart(), "the old start is an interval start")

	// Only Empty slots lie between the old start and the new.
	err := pl.ForEveryPageAndGapInRange(
		func(*PageOrMarker[P], uint64) error { return errBadState },
		func(uint64, uint64) error { return errBadState },
		intervalStart+g.pageSize(), newIntervalStart)
	assert(err == nil, "only the interval lies between the old start and the new")

	newStart := pl.lookupOrAllocateInternal(newIntervalStart)
	if newStart == nil {
		return ErrNoMemory
	}

	// The start may move all the way to the end, which leaves a Slot.
	if newStart.IsIntervalEnd() {
		newStart.changeIntervalSentinel(SentinelSlot)
	} else {
		assert(newStart.IsEmpty(), "the new start's slot is Empty")
		// Only zero intervals are supported.
		assert(oldStart.IsIntervalZero(), "the interval is a zero interval")
		newStart.Set(zeroInterval[P](SentinelStart, oldStart.GetZeroIntervalDirtyState(), g.shift))
	}

	// With the new start made, carry over what is left of the old start's
	// AwaitingClean length.
	oldLen := oldStart.GetZeroIntervalAwaitingCleanLength()
	if oldLen > length {
		newStart.SetZeroIntervalAwaitingCleanLength(oldLen - length)
	}

	// The old start goes.
	pl.RemoveContent(intervalStart)
	return nil
}

// ClipIntervalEnd moves an interval's end from intervalEnd to
// intervalEnd-length. The interval is longer than length.
func (pl *PageList[P]) ClipIntervalEnd(intervalEnd, length uint64) error {
	g := pl.g
	assert(g.isPageRounded(intervalEnd), "the end is page rounded")
	assert(g.isPageRounded(length), "the length is page rounded")
	if length == 0 {
		return nil
	}
	newIntervalEnd, borrow := bits.Sub64(intervalEnd, length, 0)
	assert(borrow == 0, "the new end does not underflow")

	oldEnd := pl.Lookup(intervalEnd)
	assert(oldEnd.IsIntervalEnd(), "the old end is an interval end")

	// Only Empty slots lie between the new end and the old.
	err := pl.ForEveryPageAndGapInRange(
		func(*PageOrMarker[P], uint64) error { return errBadState },
		func(uint64, uint64) error { return errBadState },
		newIntervalEnd+g.pageSize(), intervalEnd)
	assert(err == nil, "only the interval lies between the new end and the old")

	newEnd := pl.lookupOrAllocateInternal(newIntervalEnd)
	if newEnd == nil {
		return ErrNoMemory
	}

	// The end may move all the way to the start, which leaves a Slot.
	if newEnd.IsIntervalStart() {
		newEnd.changeIntervalSentinel(SentinelSlot)
	} else {
		assert(newEnd.IsEmpty(), "the new end's slot is Empty")
		// Only zero intervals are supported.
		assert(oldEnd.IsIntervalZero(), "the interval is a zero interval")
		newEnd.Set(zeroInterval[P](SentinelEnd, oldEnd.GetZeroIntervalDirtyState(), g.shift))
	}
	// The old end goes.
	pl.RemoveContent(intervalEnd)
	return nil
}

// forEveryPage walks every slot that is not Empty.
func (pl *PageList[P]) forEveryPage(fn func(*PageOrMarker[P], uint64) error) error {
	var err error
	pl.list.Ascend(func(e nodeEntry[P]) bool {
		err = e.node.forEveryPage(pl.g, e.offset, fn)
		return err == nil
	})
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

// forEveryPageInRangeInternal walks the slots in [start, end) that are not
// Empty, from the node cur. With cleanup set, fn may empty slots, and a node
// left empty is freed. It returns nil when the walk ran to the end, and what
// fn returned when fn ended it, ErrStop included.
func (pl *PageList[P]) forEveryPageInRangeInternal(fn func(*PageOrMarker[P], uint64) error, cur nodeEntry[P],
	start, end uint64, cleanup bool) error {
	assert(pl.g.isPageRounded(start), "the start is page rounded")
	assert(pl.g.isPageRounded(end), "the end is page rounded")

	for cur.valid() {
		if cur.offset >= end {
			break
		}
		s := max(start, cur.offset)
		e := min(pl.g.endOffset(cur.offset), end)
		err := cur.node.forEveryPageInRange(pl.g, cur.offset, fn, s, e)
		if cleanup && cur.node.isEmpty() {
			cur = pl.eraseNext(cur)
		} else {
			cur = pl.next(cur)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// forEveryPageInRange walks the slots in [start, end) that are not Empty.
func (pl *PageList[P]) forEveryPageInRange(fn func(*PageOrMarker[P], uint64) error, start, end uint64,
	cleanup bool) error {
	// The first node that can hold start.
	cur := pl.lowerBound(pl.g.nodeOffset(start))
	err := pl.forEveryPageInRangeInternal(fn, cur, start, end, cleanup)
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

// forEveryPageAndGapInRange walks the slots in [start, end) that are not Empty,
// and the gaps between them.
func (pl *PageList[P]) forEveryPageAndGapInRange(pageFn func(*PageOrMarker[P], uint64) error,
	gapFn func(start, end uint64) error, start, end uint64, cleanup bool) error {
	cur := pl.lowerBound(pl.g.nodeOffset(start))

	expectedNextOff := start
	// Set at an interval start until its end is seen.
	inInterval := false
	wrapper := func(p *PageOrMarker[P], off uint64) error {
		// Track intervals first. If a callback then ends the walk this is
		// wasted, but done first and always it lets the compiler share work
		// with the gap check after it.
		if p.IsIntervalStart() {
			// No interval was being tracked already.
			assert(!inInterval, "intervals do not nest")
			// A start and its end are of one type, and only zero intervals
			// are supported.
			assert(p.IsIntervalZero(), "the interval is a zero interval")
			inInterval = true
		} else if p.IsIntervalEnd() {
			// Unless this is the first slot seen, an interval was being
			// tracked.
			assert(inInterval || expectedNextOff == start, "an end follows its start")
			assert(p.IsIntervalZero(), "the interval is a zero interval")
			inInterval = false
		}
		var err error
		// An interval also moves past expectedNextOff, for a run of pages,
		// so an end is not a gap.
		if expectedNextOff != off && !p.IsIntervalEnd() {
			err = gapFn(expectedNextOff, off)
		}
		expectedNextOff = off + pl.g.pageSize()
		if err == nil {
			err = pageFn(p, off)
		}
		return err
	}

	err := pl.forEveryPageInRangeInternal(wrapper, cur, start, end, cleanup)
	if err != nil {
		if errors.Is(err, ErrStop) {
			return nil
		}
		return err
	}

	// The last gap, unless the walk ended in an interval. inInterval alone
	// does not tell, since the walk can start partway into an interval and
	// never see its start. So inInterval is checked first, and then the
	// costlier isOffsetInIntervalHelper, but only if the walk saw no slot at
	// all: having seen one, it would have seen the start of an interval it
	// ended in.
	if expectedNextOff != end {
		// It ended in an interval if inInterval is set, or if it saw no slot
		// and start is in one. The whole range is then in that interval, so
		// one offset tells.
		endedInInterval := inInterval ||
			(expectedNextOff == start && cur.valid() && pl.isOffsetInIntervalHelper(start, cur) != nil)
		if !endedInInterval {
			err = gapFn(expectedNextOff, end)
			if err != nil && !errors.Is(err, ErrStop) {
				return err
			}
		}
	}

	return nil
}

// forEveryPageAndContiguousRunInRange is ForEveryPageAndContiguousRunInRange.
func (pl *PageList[P]) forEveryPageAndContiguousRunInRange(compare func(*PageOrMarker[P], uint64) bool,
	pageFn func(*PageOrMarker[P], uint64) error, runFn func(start, end uint64, isInterval bool) error,
	start, end uint64) error {
	if start == end {
		return nil
	}
	ps := pl.g.pageSize()

	// The run of slots compare accepts.
	runStart := start
	runLen := uint64(0)

	// Whether the walk saw any slot or gap at all.
	foundPageOrGap := false
	// What an interval start told, for its end.
	var tracker struct {
		intervalStartOffset uint64
		startCompareStatus  bool
		startedInterval     bool
	}

	err := pl.forEveryPageAndGapInRange(
		func(p *PageOrMarker[P], off uint64) error {
			foundPageOrGap = true
			var st error
			compareResult := compare(p, off)

			// Intervals first.
			if p.IsInterval() {
				// An interval is a run apart from the pages and markers
				// before it, so a run being tracked ends here and runFn is
				// called on it. That comes before pageFn, so no page is
				// handled if runFn on the run before would fail, and whether
				// or not compare accepts the interval, since the run is of
				// the pages before it.
				if p.IsIntervalStart() || p.IsIntervalSlot() {
					if runLen > 0 {
						st = runFn(runStart, runStart+runLen, false)
						// Start a new run.
						runLen = 0
						if st != nil {
							return st
						}
					}
				}
				assert(runLen == 0, "no run is open at an interval")

				// pageFn on the sentinel first, then the run.
				if compareResult {
					st = pageFn(p, off)
					if st != nil && !errors.Is(st, ErrStop) {
						return st
					}
				}

				// A Slot is a run of one page.
				if p.IsIntervalSlot() {
					// No interval was being tracked.
					assert(!tracker.startedInterval, "intervals do not nest")
					if compareResult {
						return runFn(off, off+ps, true)
					}
					return nil
				}

				if p.IsIntervalStart() {
					// A new interval. None was being tracked.
					assert(!tracker.startedInterval, "intervals do not nest")
					tracker.startedInterval = true
					tracker.intervalStartOffset = off
					// What compare said of the start.
					tracker.startCompareStatus = compareResult
					return nil
				}

				assert(p.IsIntervalEnd(), "the sentinel is an end")
				// An end compare does not accept ends the matter.
				if !compareResult {
					tracker.startedInterval = false
					return nil
				}

				// At an end, the interval is a run if compare accepts both
				// its start and its end. The walk may have started inside the
				// interval and missed the start, which is then found and
				// checked here.
				if !tracker.startedInterval {
					startSlot, intervalStartOffset := pl.findIntervalStartForEnd(off)
					assert(startSlot != nil, "the interval has a start")
					assert(startSlot.IsIntervalStart(), "the start is a start")
					assert(intervalStartOffset < start, "the start is before the range")
					tracker.startedInterval = true
					tracker.startCompareStatus = compare(startSlot, intervalStartOffset)
					// The range before start is not considered, so the
					// interval begins at start here.
					tracker.intervalStartOffset = start
				}
				assert(tracker.startedInterval, "an interval is being tracked")
				tracker.startedInterval = false
				if tracker.startCompareStatus {
					return runFn(tracker.intervalStartOffset, off+ps, true)
				}
				return nil
			}

			// Anything but an interval.
			assert(!p.IsInterval(), "the slot is not an interval")
			assert(!tracker.startedInterval, "no interval is open at a page")

			if compareResult {
				st = pageFn(p, off)
				// An error ends the walk before the page joins a run.
				if st != nil && !errors.Is(st, ErrStop) {
					// A run being tracked ended before this page, so runFn is
					// called on it.
					if runLen > 0 {
						prevRangeStatus := runFn(runStart, runStart+runLen, false)
						runLen = 0
						// An error on the run before comes first, as it is at a
						// lower offset.
						if prevRangeStatus != nil && !errors.Is(prevRangeStatus, ErrStop) {
							return prevRangeStatus
						}
					}
					return st
				}

				// Start a run if none is open.
				if runLen == 0 {
					runStart = off
				}
				// The page joins the run.
				runLen += ps
				// On ErrStop the page is in the run and the walk ends after it.
				return st
			}
			// A page compare does not accept ends the run being tracked,
			// which runFn is called on. A new run starts after the page.
			if runLen > 0 {
				st = runFn(runStart, runStart+runLen, false)
				// Reset whatever runFn returned, so the run is not handled
				// again after the walk.
				runLen = 0
			}
			return st
		},
		func(uint64, uint64) error {
			foundPageOrGap = true
			// No gap lies inside an interval being tracked.
			assert(!tracker.startedInterval, "no gap inside an interval")
			var st error
			// A gap ends the run being tracked, which runFn is called on. A
			// new run starts after the gap.
			if runLen > 0 {
				st = runFn(runStart, runStart+runLen, false)
				// Reset whatever runFn returned, so the run is not handled
				// again after the walk.
				runLen = 0
			}
			return st
		},
		start, end, false)

	if err != nil {
		return err
	}

	// Seeing neither a slot nor a gap means the range is inside one
	// interval. Its start and end are found and compare is asked of both.
	if !foundPageOrGap {
		assert(pl.isOffsetInInterval(start), "the range is in an interval")
		assert(pl.isOffsetInInterval(end-ps), "the range is in an interval")

		intervalEndOffset := uint64(math.MaxUint64)
		endCompareStatus := false
		err = pl.forEveryPageInRange(func(p *PageOrMarker[P], off uint64) error {
			// The first populated slot is the interval's end.
			assert(p.IsIntervalEnd(), "the first slot after the range is the end")
			intervalEndOffset = off
			endCompareStatus = compare(p, off)
			return ErrStop
		}, end, pl.g.maxSize(), false)
		assert(err == nil, "finding the end does not fail")

		if endCompareStatus {
			startSlot, intervalStartOffset := pl.findIntervalStartForEnd(intervalEndOffset)
			assert(startSlot != nil, "the interval has a start")
			assert(startSlot.IsIntervalStart(), "the start is a start")
			assert(intervalStartOffset < start, "the start is before the range")
			if compare(startSlot, intervalStartOffset) {
				err = runFn(start, end, true)
				if err != nil && !errors.Is(err, ErrStop) {
					return err
				}
			}
		}
		return nil
	}

	// The last run, or an interval started and not ended.
	if runLen > 0 {
		err = runFn(runStart, runStart+runLen, false)
		if err != nil && !errors.Is(err, ErrStop) {
			return err
		}
	} else if tracker.startedInterval && tracker.startCompareStatus {
		endSlot, intervalEndOffset := pl.findIntervalEndForStart(tracker.intervalStartOffset)
		assert(endSlot != nil, "the interval has an end")
		assert(endSlot.IsIntervalEnd(), "the end is an end")
		if compare(endSlot, intervalEndOffset) {
			err = runFn(tracker.intervalStartOffset, end, true)
			if err != nil && !errors.Is(err, ErrStop) {
				return err
			}
		}
	}

	return nil
}

// Cursor walks consecutive slots of a page list, Empty ones included, and
// stops where the slots stop being consecutive. It is Zircon's VMPLCursor.
// Nothing may be removed from the list while a cursor is in use; inserting is
// safe. The zero Cursor is not valid; a list's Lookup*Cursor methods make
// them.
type Cursor[P any] struct {
	list *PageList[P]
	node nodeEntry[P]
	// index is the slot in node. pageFanOut means the cursor is not valid.
	index uint64
}

func invalidCursor[P any](pl *PageList[P]) Cursor[P] {
	return Cursor[P]{list: pl, index: pageFanOut}
}

func (c *Cursor[P]) valid() bool { return c.index < pageFanOut }

// CurrentRef is Current with a PageOrMarkerRef, which is not Valid when the
// cursor is not.
func (c *Cursor[P]) CurrentRef() PageOrMarkerRef[P] {
	if c.valid() {
		return PageOrMarkerRef[P]{slot: &c.node.node.pages[c.index]}
	}
	return PageOrMarkerRef[P]{}
}

// Current is the slot the cursor is at, which may be Empty, or nil when the
// cursor is no longer valid. The caller tracks the offset by counting Steps,
// and must only read the slot.
func (c *Cursor[P]) Current() *PageOrMarker[P] {
	if c.valid() {
		return &c.node.node.pages[c.index]
	}
	return nil
}

// Step moves the cursor to the next slot. Current is nil after it if that
// slot is not consecutive.
func (c *Cursor[P]) Step() {
	if c.valid() {
		c.index++
		if c.index == pageFanOut {
			c.incNode()
		}
	}
}

// ForEveryContiguous calls fn on every slot from the cursor on while they are
// consecutive. It is a loop of Current and Step.
func (c *Cursor[P]) ForEveryContiguous(fn func(p *PageOrMarker[P]) error) error {
	for c.valid() {
		for c.index < pageFanOut {
			if err := fn(&c.node.node.pages[c.index]); err != nil {
				if errors.Is(err, ErrStop) {
					return nil
				}
				return err
			}
			c.index++
		}
		if !c.incNode() {
			return nil
		}
	}
	return nil
}

// Offset is the offset of the slot the cursor is at. The cursor must be
// valid.
func (c *Cursor[P]) Offset() uint64 {
	assert(c.valid(), "the cursor is valid")
	return c.node.offset + c.index*c.list.g.pageSize()
}

// incNode moves to the next node if it follows this one without a gap.
func (c *Cursor[P]) incNode() bool {
	// Only from the end of a node, or the slots would not be consecutive.
	assert(c.index == pageFanOut, "the cursor is past its node")
	prevOffset := c.node.offset
	c.node = c.list.next(c.node)
	if c.node.valid() && c.node.offset == prevOffset+c.list.g.nodeSize() {
		// The next node follows: start at its first slot, which also makes
		// the cursor valid again.
		c.index = 0
		return true
	}
	return false
}
