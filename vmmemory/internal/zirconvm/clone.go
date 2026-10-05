// Copyright 2020 The Fuchsia Authors
// Copyright 2016 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc, vm/include/vm/vm_cow_pages.h and
// vm/include/vm/vm_object.h at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

// SnapshotType is the kind of clone, Zircon's SnapshotType.
type SnapshotType uint8

const (
	// SnapshotFull is a snapshot of everything, which needs a hidden parent
	// in Zircon. Not ported: CreateCloneLocked refuses it.
	SnapshotFull SnapshotType = iota
	// SnapshotModified snapshots what the object modified of its parent's
	// content. Of an object with no parent it is a snapshot-on-write child;
	// of one with a parent it needs a hidden parent, and is refused.
	SnapshotModified
	// SnapshotOnWrite reads the parent until the child writes, and then
	// copies. It is the one kind the port takes (plan: Fork sharing).
	SnapshotOnWrite
)

// The port takes the snapshot-on-write child and nothing else of Zircon's
// clone tree: no hidden parents, no merge, no full or modified snapshots
// (plan: Zircon's objects and ours). A fork's child relates to each page it
// inherits as a snapshot-on-write child relates to a parent that never
// changes: it reads the parent's page until it writes, and then copies it.

// addChildLocked hangs child under this object, seeing its parent from
// offset for parentLimit bytes. The child shares the tree's lock.
func (c *CowPages) addChildLocked(child *CowPages, offset, parentLimit uint64) {
	// The child stops seeing its parent at the end of its size at most.
	assert(parentLimit <= child.size, "the limit is in the child")
	// Offsets projected to the root must not overflow.
	rootParentOffset := checkedAdd(offset, c.rootParentOffset)
	checkedAdd(rootParentOffset, child.size)
	child.rootParentOffset = rootParentOffset
	child.parentOffset = offset
	child.parentLimit = parentLimit
	// Zircon counts a high priority child against its parent; high priority
	// is not ported.
	child.parent = c
	child.lock = c.lock
	c.children = append([]*CowPages{child}, c.children...)
}

// dropChildLocked takes child off this object's children.
func (c *CowPages) dropChildLocked(child *CowPages) {
	for i, ch := range c.children {
		if ch == child {
			c.children = append(c.children[:i], c.children[i+1:]...)
			return
		}
	}
	panic("zirconvm: the child is not this object's")
}

// removeChildLocked takes a dying child off this object. Zircon merges a
// hidden parent with its remaining child here; there are none.
func (c *CowPages) removeChildLocked(removed *CowPages) {
	c.dropChildLocked(removed)
}

// checkedAdd adds, asserting no overflow, Zircon's CheckedAdd.
func checkedAdd(a, b uint64) uint64 {
	result := a + b
	assert(result >= a, "the sum does not overflow")
	return result
}

// clampedLimit clamps limit so that offset+limit is at most maxLimit, and to
// zero if offset is past it, Zircon's ClampedLimit.
func clampedLimit(offset, limit, maxLimit uint64) uint64 {
	offsetLimit := checkedAdd(offset, limit)
	return max(min(offsetLimit, maxLimit), offset) - offset
}

// parentAndRange is the node a clone hangs from and the clone's window on it.
type parentAndRange struct {
	parent       *CowPages
	parentOffset uint64
	parentLimit  uint64
	size         uint64
}

// findParentAndRangeForCloneLocked walks up from this object to the most
// distant node that can be the clone's parent: the first one holding
// content in the clone's range, or the root. The window is trimmed so the
// clone sees no more than it would hanging from this object. Zircon's walk
// may also have to stop at a hidden node; there are none.
func (c *CowPages) findParentAndRangeForCloneLocked(offset, size uint64) parentAndRange {
	// The clone sees its parent up to its size, and no further than the
	// parent's size.
	parentLimit := clampedLimit(offset, size, c.size)
	parent := c
	for next := parent.parent; next != nil; next = parent.parent {
		// A node holding content in the range must be the parent, or the
		// clone could not snapshot what it would have seen.
		if parentLimit > 0 && parent.pageList.AnyOwnedPagesOrIntervalsInRange(offset, offset+parentLimit) {
			break
		}
		assert(checkedAdd(next.rootParentOffset, parent.parentOffset) == parent.rootParentOffset,
			"the offset projected to the root is unchanged")
		// Move the window to the next node, seeing no more of it than of
		// this one.
		parentLimit = clampedLimit(offset, parentLimit, parent.parentLimit)
		offset = checkedAdd(parent.parentOffset, offset)
		parent = next
	}
	return parentAndRange{parent: parent, parentOffset: offset, parentLimit: parentLimit, size: size}
}

// cloneChildLocked makes a snapshot-on-write child of this object. Unlike a
// clone through a hidden parent, the range stays writable here and is copied
// on write in the child; the child also sees what this object sees from its
// own parent.
func (c *CowPages) cloneChildLocked(offset, limit, size uint64) *CowPages {
	clone := newCowPages(c.node, c.inheritableOptions(), size, nil, c.lock)
	c.addChildLocked(clone, offset, limit)
	return clone
}

// canUnidirectionalCloneLocked reports whether a snapshot-on-write child can
// be made. Zircon makes one only in a tree a user pager backs, keeping
// anonymous trees for full snapshots through hidden parents. The port has
// neither hidden parents nor full snapshots, so any object can have one.
func (c *CowPages) canUnidirectionalCloneLocked() bool { return true }

// CreateCloneLocked makes a clone of r of the kind asked for, which must be
// one a snapshot-on-write child can be: SnapshotOnWrite, or SnapshotModified
// of an object with no parent, which has nothing modified to snapshot. A
// full snapshot, or a modified one of a child, needs a hidden parent and is
// refused. The new object shares the tree's lock and is not yet alive.
func (c *CowPages) CreateCloneLocked(typ SnapshotType, requireUnidirectional bool, r CowRange) (*CowPages, error) {
	assert(c.isPageAligned(r), "the range is page aligned")
	// A full snapshot of a tree a user pager backs is refused by Zircon too:
	// its unidirectional clones could change the content under it.
	if typ == SnapshotFull && c.canRootSourceEvict() {
		return nil, ErrNotSupported
	}
	var requireBidirectional bool
	switch typ {
	case SnapshotFull:
		requireBidirectional = true
	case SnapshotModified:
		// A parent's content this object modified needs a snapshot of it.
		requireBidirectional = c.parent != nil
	case SnapshotOnWrite:
		requireBidirectional = false
	}
	// Offsets in the clone must not overflow projected to the root.
	childRootParentOffset := c.rootParentOffset + r.Offset
	if childRootParentOffset < c.rootParentOffset {
		return nil, ErrInvalidArgs
	}
	if childRootParentOffset+r.Len < childRootParentOffset {
		return nil, ErrInvalidArgs
	}
	if requireBidirectional && requireUnidirectional {
		return nil, ErrNotSupported
	}
	unidirectional := !requireBidirectional && c.canUnidirectionalCloneLocked()
	// Only objects that free their pages to the pmm may have clones.
	assert(c.pageSourceType() == Anonymous || c.pageSourceType() == UserPager, "the object frees to the pmm")
	if !unidirectional {
		// Zircon makes the clone through a hidden parent here. There are
		// none (plan: Fork sharing).
		return nil, ErrNotSupported
	}
	childRange := c.findParentAndRangeForCloneLocked(r.Offset, r.Len)
	return childRange.parent.cloneChildLocked(childRange.parentOffset, childRange.parentLimit, childRange.size), nil
}
