package volume

import (
	"context"
	"iter"

	"github.com/semistrict/sproutfs/checkpoint"
)

// extent is one run of a volume written since the selected checkpoint. Extents
// are sorted and non-overlapping, and their payloads are immutable, so a reader
// keeps a consistent view while later writes replace the live index. A nil
// payload denotes explicit zeroes, which is how a discard and the zeroed part of
// a batched write are both recorded.
type extent struct {
	start, end uint64
	data       []byte
	generation generation
}

// extentIndex is an immutable AVL interval index. Splitting at the two
// replacement boundaries copies only their search paths; discarded subtrees are
// dropped in constant time. Joining restores balance without touching the old
// reader's nodes.
type extentIndex struct {
	item          extent
	left, right   *extentIndex
	height, count int
}

func (n *extentIndex) depth() int {
	if n == nil {
		return 0
	}
	return n.height
}

func (n *extentIndex) size() int {
	if n == nil {
		return 0
	}
	return n.count
}

func extentBranch(left *extentIndex, item extent, right *extentIndex) *extentIndex {
	return &extentIndex{item: item, left: left, right: right, height: 1 + max(left.depth(), right.depth()), count: 1 + left.size() + right.size()}
}

// balanceExtent accepts children whose heights differ by at most two.
func balanceExtent(left *extentIndex, item extent, right *extentIndex) *extentIndex {
	if left.depth() > right.depth()+1 {
		if left.left.depth() >= left.right.depth() {
			return extentBranch(left.left, left.item, extentBranch(left.right, item, right))
		}
		pivot := left.right
		return extentBranch(extentBranch(left.left, left.item, pivot.left), pivot.item, extentBranch(pivot.right, item, right))
	}
	if right.depth() > left.depth()+1 {
		if right.right.depth() >= right.left.depth() {
			return extentBranch(extentBranch(left, item, right.left), right.item, right.right)
		}
		pivot := right.left
		return extentBranch(extentBranch(left, item, pivot.left), pivot.item, extentBranch(pivot.right, right.item, right.right))
	}
	return extentBranch(left, item, right)
}

func joinExtent(left *extentIndex, item extent, right *extentIndex) *extentIndex {
	if left.depth() > right.depth()+1 {
		return balanceExtent(left.left, left.item, joinExtent(left.right, item, right))
	}
	if right.depth() > left.depth()+1 {
		return balanceExtent(joinExtent(left, item, right.left), right.item, right.right)
	}
	return extentBranch(left, item, right)
}

func splitExtents(n *extentIndex, position uint64) (*extentIndex, *extentIndex) {
	if n == nil {
		return nil, nil
	}
	item := n.item
	if position <= item.start {
		left, middle := splitExtents(n.left, position)
		return left, joinExtent(middle, item, n.right)
	}
	if position >= item.end {
		middle, right := splitExtents(n.right, position)
		return joinExtent(n.left, item, middle), right
	}
	left, right := item, item
	left.end, right.start = position, position
	if item.data != nil {
		left.data, right.data = item.data[:position-item.start], item.data[position-item.start:]
	}
	return joinExtent(n.left, left, nil), joinExtent(nil, right, n.right)
}

// replaceExtent returns the index with next covering its whole range, which is
// what one write or discard does to the ranges already there.
func replaceExtent(current *extentIndex, next extent) *extentIndex {
	left, rest := splitExtents(current, next.start)
	_, right := splitExtents(rest, next.end)
	return joinExtent(left, next, right)
}

// between visits only overlapping extents in logical order: O(log N + K).
func (n *extentIndex) between(start, end uint64) iter.Seq[extent] {
	return func(yield func(extent) bool) {
		var visit func(*extentIndex) bool
		visit = func(n *extentIndex) bool {
			if n == nil {
				return true
			}
			if n.item.start >= end {
				return visit(n.left)
			}
			if n.item.end <= start {
				return visit(n.right)
			}
			return visit(n.left) && yield(n.item) && visit(n.right)
		}
		if start < end {
			visit(n)
		}
	}
}

func (n *extentIndex) all() iter.Seq[extent] {
	return n.between(0, ^uint64(0))
}

// retain drops every extent a checkpoint published, which is every extent
// written at or below the generation that checkpoint was taken at.
func (n *extentIndex) retain(through generation) *extentIndex {
	var kept *extentIndex
	for item := range n.all() {
		if item.generation > through {
			kept = joinExtent(kept, item, nil)
		}
	}
	return kept
}

// inherited reads the bytes a VM did not write since its selected checkpoint.
type inherited func(ctx context.Context, offset uint64, dst []byte) error

// readOverlay fills dst from one immutable view: the overlay's extents where it
// has them and the inherited bytes everywhere else. On error dst may be
// partially filled.
func readOverlay(ctx context.Context, base inherited, overlay *extentIndex, offset uint64, dst []byte) error {
	end := offset + uint64(len(dst))
	cursor := offset
	for item := range overlay.between(offset, end) {
		start := max(item.start, offset)
		if cursor < start {
			if err := base(ctx, cursor, dst[cursor-offset:start-offset]); err != nil {
				return err
			}
		}
		stop := min(item.end, end)
		target := dst[start-offset : stop-offset]
		if item.data == nil {
			clear(target)
		} else {
			copy(target, item.data[start-item.start:stop-item.start])
		}
		cursor = stop
	}
	if cursor < end {
		return base(ctx, cursor, dst[cursor-offset:])
	}
	return context.Cause(ctx)
}

// inheritedPages is inherited of only the pages a mask marks.
type inheritedPages func(ctx context.Context, offset uint64, dst []byte, wanted []bool) error

// readOverlayPages fills the pages of a range that wanted marks — one element
// per page the range touches, or nil for every one of them — and leaves the
// bytes of every other page as the caller had them.
//
// A masked read is page-wise where an unmasked one is byte-wise, because a mask
// is per page: the inherited source is asked once for every wanted page the
// overlay does not already hold whole, and the overlay is then laid over what
// came back. So a window of pages costs one read of the source however many
// pages of it the overlay holds, which is what a fault needs; the few bytes of
// a page the overlay only partly covers are read and then overwritten.
func readOverlayPages(ctx context.Context, base inheritedPages, overlay *extentIndex,
	size, offset uint64, dst []byte, wanted []bool) error {
	end := offset + uint64(len(dst))
	if end == offset {
		return context.Cause(ctx)
	}
	first := offset / size
	held := make([]uint64, (end-1)/size-first+1)
	pages := func(item extent, visit func(page, lo, hi uint64)) {
		start, stop := max(item.start, offset), min(item.end, end)
		for page := start / size; page <= (stop-1)/size; page++ {
			visit(page, max(start, page*size), min(stop, (page+1)*size))
		}
	}
	for item := range overlay.between(offset, end) {
		pages(item, func(page, lo, hi uint64) { held[page-first] += hi - lo })
	}
	inherited := make([]bool, len(held))
	asked := false
	for at := range inherited {
		page := first + uint64(at)
		span := min(end, (page+1)*size) - max(offset, page*size)
		inherited[at] = (wanted == nil || wanted[at]) && held[at] < span
		asked = asked || inherited[at]
	}
	if asked {
		if err := base(ctx, offset, dst, inherited); err != nil {
			return err
		}
	}
	for item := range overlay.between(offset, end) {
		pages(item, func(page, lo, hi uint64) {
			if wanted != nil && !wanted[page-first] {
				return
			}
			target := dst[lo-offset : hi-offset]
			if item.data == nil {
				clear(target)
				return
			}
			copy(target, item.data[lo-item.start:hi-item.start])
		})
	}
	return context.Cause(ctx)
}

// coveredUnits yields the half-open run of units of the given size that each
// extent of an overlay adds, skipping the units an earlier extent already
// reported. Extents come in logical order, so the unit one run ends at carries
// the whole deduplication and a run is one pair however many units it spans.
func coveredUnits(overlay *extentIndex, size uint64) iter.Seq2[uint64, uint64] {
	return func(yield func(uint64, uint64) bool) {
		next := uint64(0)
		for item := range overlay.all() {
			first, limit := max(item.start/size, next), (item.end-1)/size+1
			if first >= limit {
				continue
			}
			if !yield(first, limit) {
				return
			}
			next = limit
		}
	}
}

// dirtySectorCount reports how many sectors an overlay covers, which times the
// sector size is the upper bound on what the next checkpoint publishes. It is a
// count and not a list: one discard covers a whole volume, and a list of that
// is a slot per sector of it on a path that wants one number.
func dirtySectorCount(overlay *extentIndex) uint64 {
	count := uint64(0)
	for first, limit := range coveredUnits(overlay, checkpoint.SectorSize) {
		count += limit - first
	}
	return count
}

// dirtyPages reports every page of the volume's own geometry an overlay covers,
// in ascending order. These are exactly the pages a checkpoint of that overlay
// republishes whole, and the publication names them one at a time, so here the
// list is the answer.
func dirtyPages(overlay *extentIndex, geometry checkpoint.Geometry) []uint64 {
	var pages []uint64
	for first, limit := range coveredUnits(overlay, geometry.PageSize) {
		for page := first; page < limit; page++ {
			pages = append(pages, page)
		}
	}
	return pages
}

// sectorSpan is the number of bytes of whole pages a range touches, the unit
// the checkpoint trigger counts in.
func sectorSpan(offset, length uint64) uint64 {
	if length == 0 {
		return 0
	}
	return ((offset+length-1)/checkpoint.SectorSize - offset/checkpoint.SectorSize + 1) * checkpoint.SectorSize
}
