// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc and vm/include/vm/vm_cow_pages.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"errors"
	"sync"
)

// The statuses Zircon's VM returns, as errors.
var (
	// ErrShouldWait is ZX_ERR_SHOULD_WAIT: a page request was filled out and
	// the caller should wait on it and try again.
	ErrShouldWait = errors.New("zirconvm: should wait")
	// ErrOutOfRange is ZX_ERR_OUT_OF_RANGE.
	ErrOutOfRange = errors.New("zirconvm: out of range")
	// ErrInvalidArgs is ZX_ERR_INVALID_ARGS.
	ErrInvalidArgs = errors.New("zirconvm: invalid arguments")
	// ErrNotFound is ZX_ERR_NOT_FOUND.
	ErrNotFound = errors.New("zirconvm: not found")
	// ErrBadState is ZX_ERR_BAD_STATE.
	ErrBadState = errors.New("zirconvm: bad state")
	// ErrNotSupported is ZX_ERR_NOT_SUPPORTED.
	ErrNotSupported = errors.New("zirconvm: not supported")
	// ErrAlreadyExists is ZX_ERR_ALREADY_EXISTS.
	ErrAlreadyExists = errors.New("zirconvm: already exists")
	// ErrIO is ZX_ERR_IO.
	ErrIO = errors.New("zirconvm: I/O error")
	// ErrIODataIntegrity is ZX_ERR_IO_DATA_INTEGRITY.
	ErrIODataIntegrity = errors.New("zirconvm: data integrity error")
	// ErrNoSpace is ZX_ERR_NO_SPACE.
	ErrNoSpace = errors.New("zirconvm: no space")
	// ErrBufferTooSmall is ZX_ERR_BUFFER_TOO_SMALL.
	ErrBufferTooSmall = errors.New("zirconvm: buffer too small")
)

// CowRange is a range of a CowPages, Zircon's VmCowRange.
type CowRange struct{ Offset, Len uint64 }

// End is one past the range's last byte.
func (r CowRange) End() uint64 { return r.Offset + r.Len }

// IsEmpty reports whether the range has no bytes.
func (r CowRange) IsEmpty() bool { return r.Len == 0 }

// TrimmedFromStart is the range less its first amount bytes.
func (r CowRange) TrimmedFromStart(amount uint64) CowRange {
	return CowRange{r.Offset + amount, r.Len - amount}
}

// Cover is the smallest range covering r and other.
func (r CowRange) Cover(other CowRange) CowRange {
	if r.IsEmpty() {
		return other
	}
	if other.IsEmpty() {
		return r
	}
	start := min(r.Offset, other.Offset)
	end := max(r.End(), other.End())
	return CowRange{start, end - start}
}

// WithLength is the range from the same offset with a new length.
func (r CowRange) WithLength(length uint64) CowRange { return CowRange{r.Offset, length} }

// IsBoundedBy reports whether the range is within [0, limit), without
// overflowing: Zircon's InRange.
func (r CowRange) IsBoundedBy(limit uint64) bool {
	end := r.Offset + r.Len
	if end < r.Offset {
		return false
	}
	return r.Offset <= limit && end <= limit
}

// ReclaimSuccess is VmCowReclaimSuccess: how a reclamation succeeded.
type ReclaimSuccess struct {
	Type     ReclaimType
	NumPages uint64
}

// ReclaimType is how pages were reclaimed.
type ReclaimType uint8

const (
	// ReclaimEvict dropped clean pages a pager can supply again.
	ReclaimEvict ReclaimType = iota
	// ReclaimCompress compressed a page.
	ReclaimCompress
)

// ReclaimFailure is VmCowReclaimFailure: why a reclamation failed.
type ReclaimFailure uint8

const (
	// ReclaimSucceeded is no failure.
	ReclaimSucceeded ReclaimFailure = iota
	// EvictAccessed is an eviction of pages that were not reclaimable.
	EvictAccessed
	// CompressAccessed is a compression of a page accessed meanwhile.
	CompressAccessed
	// CompressFailedReclaim is a compression that failed.
	CompressFailedReclaim
	// IncorrectPage is a page not at the offset given.
	IncorrectPage
	// ReclaimOther is any other failure.
	ReclaimOther
)

// PageSourceType is what directly backs an object.
type PageSourceType uint8

const (
	// Anonymous is an object no page source backs.
	Anonymous PageSourceType = iota
	// Contiguous is an object a contiguous page source backs. Not ported:
	// kept so the type reads as Zircon's.
	Contiguous
	// UserPager is an object a user pager backs, which tracks dirty pages.
	UserPager
)

// cowPagesOptions is VmCowPagesOptions.
type cowPagesOptions uint32

const (
	optionsNone                cowPagesOptions = 0
	optionUserPagerBackedRoot  cowPagesOptions = 1 << 0
	optionPageSourceRoot       cowPagesOptions = 1 << 1
	optionCannotDecommitZeroes cowPagesOptions = 1 << 2
	// optionIdentityRoot marks an identity root: an object a pager backs
	// whose pages are Clean and never change, which regions' lookups fall
	// through to (plan: Zircon's objects and ours). It is not Zircon's.
	optionIdentityRoot cowPagesOptions = 1 << 4
)

// lifeCycle is VmCowPages::LifeCycle.
type lifeCycle uint8

const (
	lifeInit lifeCycle = iota
	lifeAlive
	lifeDying
	lifeDead
)

// CanOverwriteSlot is which content an added page may replace.
type CanOverwriteSlot uint8

const (
	// OverwriteEmpty overwrites only empty slots.
	OverwriteEmpty CanOverwriteSlot = iota
	// OverwriteEmptyOrParent also overwrites parent content markers.
	OverwriteEmptyOrParent
	// OverwriteZeroMarkerOrInterval also overwrites zero markers and zero
	// intervals.
	OverwriteZeroMarkerOrInterval
	// OverwritePageOrRef overwrites anything.
	OverwritePageOrRef
)

// RangeChangeOp is what a range change does to the mappings it reaches,
// VmObject::RangeChangeOp.
type RangeChangeOp uint8

const (
	// Unmap removes the mappings.
	Unmap RangeChangeOp = iota
	// UnmapZeroPage removes mappings the caller knows are all of the zero
	// page.
	UnmapZeroPage
	// UnmapAndHarvest removes the mappings and harvests their accessed
	// bits. A userfaultfd pager cannot read the VMM's accessed bits
	// (decision 4), so a mapping harvests nothing.
	UnmapAndHarvest
	// RemoveWrite makes the mappings read-only.
	RemoveWrite
)

// RootResolver names the identity root that holds the content of an offset
// of a region's layer, and the offset in it. It is the departure that
// replaces Zircon's walk up the parent chain for a region's layer: a
// region's content is a mosaic of many checkpoints, which no chain of
// parents describes, so Locate names the identity and the identity the root
// (plan: Zircon's objects and ours). ok is false where no root holds the
// content, and the region's own page source is asked.
type RootResolver interface {
	Locate(offset uint64) (root *CowPages, rootOffset uint64, ok bool)
}

// CowPages holds an object's pages in a page list and knows its parent,
// Zircon's VmCowPages. The port keeps the snapshot-on-write child only: no
// hidden nodes, no full or modified snapshots, no slices or references (plan:
// Zircon's objects and ours). An object is either a region's layer, whose
// page source is the pager and whose lookups fall through to identity roots
// through its RootResolver, or an identity root, or an anonymous object.
type CowPages struct {
	node    *Node
	options cowPagesOptions

	// lock is the lock of the whole tree of clones. Zircon locks each node
	// and walks up locking parents in order; the plan keeps one lock per
	// tree (Locking). An identity root is a tree of its own, and a region's
	// lookup takes a root's lock after its own.
	lock *sync.Mutex

	size uint64
	// parentOffset is where this object starts in its parent, parentLimit
	// where in this object it stops seeing the parent, and
	// rootParentOffset where it starts in the root of the chain.
	parentOffset, parentLimit, rootParentOffset uint64

	parent *CowPages
	// children are the clones of this object, newest first.
	children []*CowPages

	pageSource *PageSource

	reclamationEventCount uint64

	pageList *PageList[VmPage]

	// populatedSlots is the continuous attribution tracker: the pages,
	// references and parent content markers in pageList.
	populatedSlots int64

	// paged is the VmObjectPaged this object backs, Zircon's paged_ref_.
	paged *ObjectPaged

	pagerStatsModified bool

	lifeCycle lifeCycle

	// roots resolves offsets this object holds no content for to identity
	// roots. Only a region's layer has one.
	roots RootResolver

	// held are the pages and zero markers a checkpoint holds that a store
	// has since moved past: the AwaitingClean content D1 keeps beside the
	// page list, by offset. Zircon has no such list (dirty.go).
	held *PageList[VmPage]
}

func newCowPages(node *Node, options cowPagesOptions, size uint64, source *PageSource, lock *sync.Mutex) *CowPages {
	c := &CowPages{
		node:       node,
		options:    options,
		lock:       lock,
		size:       size,
		pageSource: source,
		pageList:   NewPageList[VmPage](node.pageSize),
		held:       NewPageList[VmPage](node.pageSize),
	}
	assert(c.isPageRounded(size), "the size is page rounded")
	return c
}

// createCowPages is VmCowPages::Create: an anonymous object.
func createCowPages(node *Node, options cowPagesOptions, size uint64) *CowPages {
	return newCowPages(node, options, size, nil, new(sync.Mutex))
}

// createExternalCowPages is VmCowPages::CreateExternal: an object src backs.
func createExternalCowPages(node *Node, src *PageSource, options cowPagesOptions, size uint64) *CowPages {
	return newCowPages(node, options, size, src, new(sync.Mutex))
}

func (c *CowPages) pageSize() uint64 { return c.node.pageSize }

func (c *CowPages) isPageRounded(x uint64) bool { return x&(c.pageSize()-1) == 0 }

func (c *CowPages) isPageAligned(r CowRange) bool {
	return c.isPageRounded(r.Offset) && c.isPageRounded(r.Len)
}

func (c *CowPages) roundDown(x uint64) uint64 { return x &^ (c.pageSize() - 1) }

func (c *CowPages) roundUp(x uint64) uint64 { return c.roundDown(x + c.pageSize() - 1) }

// expandTillPageAligned is the smallest page aligned range covering r.
func (c *CowPages) expandTillPageAligned(r CowRange) CowRange {
	start := c.roundDown(r.Offset)
	return CowRange{start, c.roundUp(r.End()) - start}
}

// PageSize is the size of the object's pages.
func (c *CowPages) PageSize() uint64 { return c.pageSize() }

// Lock is the object's lock, for callers that need to hold it across a
// Locked method.
func (c *CowPages) Lock() *sync.Mutex { return c.lock }

// SizeLocked is the object's size.
func (c *CowPages) SizeLocked() uint64 { return c.size }

func (c *CowPages) isRootSourceUserPagerBacked() bool {
	return c.options&optionUserPagerBackedRoot != 0
}

func (c *CowPages) rootHasPageSource() bool { return c.options&optionPageSourceRoot != 0 }

// inheritableOptions are the options a clone takes from this object.
func (c *CowPages) inheritableOptions() cowPagesOptions {
	return c.options & (optionUserPagerBackedRoot | optionPageSourceRoot)
}

func (c *CowPages) isRootSourcePreservingPageContent() bool {
	return c.options&optionUserPagerBackedRoot != 0
}

func (c *CowPages) hasNoChildrenLocked() bool { return len(c.children) == 0 }

// canEvict reports whether a page of the object can be dropped and supplied
// again.
func (c *CowPages) canEvict() bool {
	return c.pageSource != nil && c.pageSource.Properties().IsUserPager
}

func (c *CowPages) canRootSourceEvict() bool {
	result := c.isRootSourcePreservingPageContent()
	assert(result == c.isRootSourceUserPagerBacked(), "a root preserves content when a user pager backs it")
	return result
}

// isDirtyTracked reports whether the object tracks its pages dirty.
func (c *CowPages) isDirtyTracked() bool {
	return c.pageSource != nil && c.pageSource.Properties().IsUserPager
}

// isIdentityRoot reports whether the object is an identity root.
func (c *CowPages) isIdentityRoot() bool { return c.options&optionIdentityRoot != 0 }

// treeHasParentContentMarkers is whether the tree marks where a leaf sees
// its parent's content. Zircon marks it in every tree no page source backs,
// for the walk through hidden nodes. The port has no hidden nodes, and lets
// an anonymous object have a snapshot-on-write child, whose empty slots must
// read its parent; so no tree marks it, and an empty slot of a child always
// means its parent's content.
func (c *CowPages) treeHasParentContentMarkers() bool { return false }

func (c *CowPages) nodeHasParentContentMarkers() bool { return c.treeHasParentContentMarkers() }

func (c *CowPages) canDecommitZeroPages() bool { return c.options&optionCannotDecommitZeroes == 0 }

func (c *CowPages) pageSourceType() PageSourceType {
	if c.pageSource == nil {
		return Anonymous
	}
	if c.pageSource.Properties().IsUserPager {
		return UserPager
	}
	return Contiguous
}

// markModifiedLocked records a write for the pager's stats.
func (c *CowPages) markModifiedLocked() {
	if !c.isDirtyTracked() {
		return
	}
	assert(c.pageSourceType() == UserPager, "a dirty tracked object has a user pager")
	c.pagerStatsModified = true
}

// QueryPagerVmoStatsLocked reports whether the object was modified since the
// last reset, and resets that if reset.
func (c *CowPages) QueryPagerVmoStatsLocked(reset bool) (bool, error) {
	if c.pageSourceType() != UserPager {
		return false, ErrNotSupported
	}
	modified := c.pagerStatsModified
	if reset {
		c.pagerStatsModified = false
	}
	return modified, nil
}

// ReclamationEventCountLocked counts the object's reclamations.
func (c *CowPages) ReclamationEventCountLocked() uint64 { return c.reclamationEventCount }

// DebugGetPopulatedSlotsCount is the tracked count of populated slots.
func (c *CowPages) DebugGetPopulatedSlotsCount() int64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.populatedSlots
}

// incrementPopulated and decrementPopulated are the continuous attribution
// tracker's Increment and Decrement.
func (c *CowPages) incrementPopulated(n int64) { c.populatedSlots += n }

func (c *CowPages) decrementPopulated(n int64) {
	c.populatedSlots -= n
	assert(c.populatedSlots >= 0, "the populated slots do not go negative")
}

// DebugValidateContinuousAttribution checks the tracked count against the
// page list.
func (c *CowPages) DebugValidateContinuousAttribution() bool {
	var count int64
	_ = c.pageList.ForEveryPage(func(p *PageOrMarker[VmPage], _ uint64) error {
		if p.IsPageOrRef() || p.IsParentContent() {
			count++
		}
		return nil
	})
	return count == c.populatedSlots
}

// lockedPtr is VmCowPages::LockedPtr: an object held locked. Within one tree
// the tree's lock is already held, so only an object of another tree, an
// identity root, is locked here.
type lockedPtr struct {
	ptr  *CowPages
	held bool
}

// lockPtr locks ptr for self, which is locked.
func lockPtr(self, ptr *CowPages) lockedPtr {
	if ptr.lock != self.lock {
		ptr.lock.Lock()
		return lockedPtr{ptr: ptr, held: true}
	}
	return lockedPtr{ptr: ptr}
}

// release unlocks the object, if this locked it, and empties the pointer.
func (l *lockedPtr) release() {
	if l.held {
		l.ptr.lock.Unlock()
	}
	*l = lockedPtr{}
}

// lockedOr is the object held, or self if none is.
func (l *lockedPtr) lockedOr(self *CowPages) *CowPages {
	if l.ptr != nil {
		return l.ptr
	}
	return self
}

func (l *lockedPtr) get() *CowPages { return l.ptr }

// TransitionToAliveLocked makes a new object usable.
func (c *CowPages) TransitionToAliveLocked() {
	assert(c.lifeCycle == lifeInit, "the object is new")
	c.lifeCycle = lifeAlive
}

func (c *CowPages) shouldDeadTransitionLocked() bool {
	return c.paged == nil && len(c.children) == 0 && c.lifeCycle == lifeAlive
}

// MaybeDeadTransition makes the object dead if nothing reaches it any more,
// and returns its parent, which may then need the same. Zircon locks the
// parent and a sibling here; the tree shares one lock.
func (c *CowPages) MaybeDeadTransition() *CowPages {
	c.lock.Lock()
	defer c.lock.Unlock()
	if !c.shouldDeadTransitionLocked() {
		return nil
	}
	return c.deadTransitionLocked()
}

// deadTransitionLocked frees the object's pages and leaves its parent.
func (c *CowPages) deadTransitionLocked() *CowPages {
	assert(c.lifeCycle == lifeAlive, "the object is alive")
	// Dying, so no other attempt is made while the lock might be dropped.
	c.lifeCycle = lifeDying
	if c.pageSource != nil {
		c.pageSource.Close()
	}
	freed := new(scopedPageFreedList)
	c.releaseOwnedPagesLocked(0, freed)
	c.freeHeldLocked(0, c.size, freed)
	freed.freePages(c)
	assert(c.pageList.IsEmpty(), "the dead object holds nothing")
	var deferred *CowPages
	if c.parent != nil {
		c.parent.removeChildLocked(c)
		deferred = c.parent
		c.parent = nil
	}
	assert(c.lifeCycle == lifeDying, "the object is still dying")
	c.lifeCycle = lifeDead
	return deferred
}

// AttributionCounts is the memory attributed to an object, Zircon's
// vm::AttributionCounts. Zircon scales shared pages by fractional bytes;
// only hidden nodes share pages and the port has none, so every count here
// is whole bytes and the scaled counts equal the private ones.
type AttributionCounts struct {
	UncompressedBytes        uint64
	CompressedBytes          uint64
	PrivateUncompressedBytes uint64
	PrivateCompressedBytes   uint64
	ScaledUncompressedBytes  uint64
	ScaledCompressedBytes    uint64
}

// TotalBytes is all the bytes counted.
func (a AttributionCounts) TotalBytes() uint64 { return a.UncompressedBytes + a.CompressedBytes }

// TotalPrivateBytes is the bytes no other object shares.
func (a AttributionCounts) TotalPrivateBytes() uint64 {
	return a.PrivateUncompressedBytes + a.PrivateCompressedBytes
}

// PrivateAttributionCounts is make_private_attribution_counts: counts of an
// object with private content only.
func PrivateAttributionCounts(uncompressed, compressed uint64) AttributionCounts {
	return AttributionCounts{
		UncompressedBytes:        uncompressed,
		CompressedBytes:          compressed,
		PrivateUncompressedBytes: uncompressed,
		PrivateCompressedBytes:   compressed,
		ScaledUncompressedBytes:  uncompressed,
		ScaledCompressedBytes:    compressed,
	}
}

// forEveryOwnedHierarchyPageInRangeLocked calls fn for every slot in [offset,
// offset+size) that is not Empty and that this object owns, with the
// object's offset. Zircon walks up into hidden parents, which partly own
// what their children see; the port has none, so an object owns exactly what
// its own page list holds, and a parent content marker never appears.
func (c *CowPages) forEveryOwnedHierarchyPageInRangeLocked(fn func(p *PageOrMarker[VmPage], offset uint64) error,
	offset, size uint64) error {
	assert(c.isPageRounded(offset), "the offset is page rounded")
	assert(c.isPageRounded(size), "the size is page rounded")
	return c.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], off uint64) error {
		assert(!p.IsParentContent(), "no parent content marker is used")
		return fn(p, off)
	}, offset, offset+size)
}

// GetAttributedMemoryInRangeLocked is the memory attributed to the object in
// r.
func (c *CowPages) GetAttributedMemoryInRangeLocked(r CowRange) AttributionCounts {
	var counts AttributionCounts
	ps := c.pageSize()
	err := c.forEveryOwnedHierarchyPageInRangeLocked(func(p *PageOrMarker[VmPage], _ uint64) error {
		// The owner is this object, so the share count is not read: it is 0
		// and the page is private.
		if p.IsPage() {
			counts.UncompressedBytes += ps
			counts.PrivateUncompressedBytes += ps
			counts.ScaledUncompressedBytes += ps
		} else if p.IsReference() {
			counts.CompressedBytes += ps
			counts.PrivateCompressedBytes += ps
			counts.ScaledCompressedBytes += ps
		}
		return nil
	}, r.Offset, r.Len)
	assert(err == nil, "attribution does not fail")
	return counts
}

// scopedPageFreedList holds pages to be freed once the lock is dropped,
// Zircon's ScopedPageFreedList.
type scopedPageFreedList struct{ pages []*VmPage }

func (l *scopedPageFreedList) add(p *VmPage) { l.pages = append(l.pages, p) }

// freePages frees the pages held to the object's pmm.
func (l *scopedPageFreedList) freePages(c *CowPages) {
	for _, p := range l.pages {
		c.freePage(p)
	}
	l.pages = nil
}

// pageRemover is BatchPQRemove: it takes pages out of the page queues into a
// freed list. Zircon batches them for its queue spinlock; one at a time is
// the same here.
type pageRemover struct {
	freed *scopedPageFreedList
	c     *CowPages
}

func (r *pageRemover) push(page *VmPage) {
	r.c.node.queues.Remove(page)
	r.freed.add(page)
}

// pushContent takes whatever a slot holds and empties it: a page is removed,
// a reference freed.
func (r *pageRemover) pushContent(p *PageOrMarker[VmPage]) {
	switch {
	case p.IsPage():
		r.push(p.ReleasePage())
	case p.IsReference():
		r.c.freeReference(p.ReleaseReference())
	default:
		*p = Empty[VmPage]()
	}
}

// freePage gives a page that is in no queue back to the pmm, and its
// reservation back to the storage (D5).
func (c *CowPages) freePage(page *VmPage) {
	assert(page.queue == nil, "a freed page is in no queue")
	c.node.releaseReservation(page)
	page.shareCount = 0
	page.alwaysNeed = false
	page.dirtyState = Untracked
	c.node.pmm.FreePage(page)
}

// freeReference frees a compressed reference, Zircon's FreeReference.
func (c *CowPages) freeReference(ref ReferenceValue) {
	assert(c.node.compression != nil, "a reference has a compression to free it to")
	c.node.compression.Free(ref)
}

// removePageLocked takes a page out of the queues for deferred freeing.
func (c *CowPages) removePageLocked(page *VmPage, ops *DeferredOps) {
	c.node.queues.Remove(page)
	ops.freedList(c).add(page)
}

// initializeVmPage gets a page ready to go in an object, InitializeVmPage.
func initializeVmPage(p *VmPage) {
	assert(p.queue == nil, "the page is in no queue")
	assert(!p.reserved, "the page holds no reservation")
	p.shareCount = 0
	p.alwaysNeed = false
	p.dirtyState = Untracked
	p.object = nil
	p.pageOffset = 0
	p.pageQueue = pageQueueNone
}

// allocPage is a new page from the pmm, ready to go in an object.
func (c *CowPages) allocPage() (*VmPage, error) {
	p, err := c.node.pmm.AllocPage()
	if err != nil {
		return nil, err
	}
	initializeVmPage(p)
	return p, nil
}

// allocateCopyPage is a new page holding a copy of parent, or zeros if
// parent is the zero page. A page from allocList is used first.
func (c *CowPages) allocateCopyPage(parent *VmPage, allocList *[]*VmPage) (*VmPage, error) {
	assert(c.pageSourceType() == Anonymous || c.pageSourceType() == UserPager, "the object allocates its own pages")
	var clone *VmPage
	if allocList != nil && len(*allocList) > 0 {
		clone = (*allocList)[0]
		*allocList = (*allocList)[1:]
		initializeVmPage(clone)
	} else {
		p, err := c.allocPage()
		if err != nil {
			return nil, err
		}
		clone = p
	}
	if parent != c.node.pmm.ZeroPage() {
		copy(clone.data, parent.data)
	} else {
		clear(clone.data)
	}
	return clone, nil
}

// isZeroPage reports whether a page's bytes are all zero.
func isZeroPage(p *VmPage) bool {
	for _, b := range p.data {
		if b != 0 {
			return false
		}
	}
	return true
}

// makePageFromReference decompresses a slot's reference into a new page in
// the slot. The page is not put in a page queue. A page Dirty or
// AwaitingClean holds its reservation again (D5): the reference itself, or
// for the temporary reference the reservation of the page being compressed.
// Zircon's cannot fail once it has a page; reading the storage can, and then
// the slot keeps its reference.
func (c *CowPages) makePageFromReference(ctx context.Context, slot PageOrMarkerRef[VmPage]) error {
	assert(slot.Get().IsReference(), "the slot holds a reference")
	compression := c.node.compression
	assert(compression != nil, "a reference has a compression")
	p, err := c.allocPage()
	if err != nil {
		return err
	}
	ref := slot.Get().Reference()
	var metadata uint32
	if _, state := unpackReferenceMetadata(compression.GetMetadata(ref)); state == Dirty || state == AwaitingClean {
		var reservation ReferenceValue
		metadata, reservation, err = compression.DecompressReserved(ctx, ref, p.data)
		if err == nil {
			p.setReservation(reservation)
		}
	} else {
		metadata, err = compression.Decompress(ctx, ref, p.data)
	}
	if err != nil {
		c.freePage(p)
		return err
	}
	slot.SwapReferenceForPage(p)
	// The reference carried the page's share count, and under D2 its dirty
	// state (reclaim.go).
	p.shareCount, p.dirtyState = unpackReferenceMetadata(metadata)
	return nil
}

// replaceReferenceWithPageLocked decompresses a reference in the page list
// and queues the page.
func (c *CowPages) replaceReferenceWithPageLocked(ctx context.Context, slot PageOrMarkerRef[VmPage], offset uint64) error {
	if err := c.makePageFromReference(ctx, slot); err != nil {
		return err
	}
	// References are never pinned.
	c.setNotPinnedLocked(slot.Get().Page(), offset)
	return nil
}

// reserveLocked gives a page about to be dirty its reservation, where the
// node keeps them and the page holds none yet (D5). It fails with ErrNoSpace
// where the storage has none left: a store that could not be spilled is not
// taken.
func (c *CowPages) reserveLocked(page *VmPage) error {
	if !c.node.reserves() || page.reserved {
		return nil
	}
	ref, ok := c.node.compression.Reserve()
	if !ok {
		return ErrNoSpace
	}
	page.setReservation(ref)
	return nil
}

// setNotPinnedLocked queues a page newly in the object.
func (c *CowPages) setNotPinnedLocked(page *VmPage, offset uint64) {
	pq := c.node.queues
	if c.pageSourceType() == UserPager {
		assert(page.dirtyState != Untracked, "a page a pager backs is dirty tracked")
		// Only Clean pages age in the reclaim queues, since only they can be
		// evicted. Zircon puts them in the high priority queue for a high
		// priority object; high priority is not ported.
		if page.dirtyState == Clean {
			pq.SetReclaim(page, c, offset)
		} else {
			pq.SetPagerBackedDirty(page, c, offset)
		}
		return
	}
	assert(c.pageSourceType() == Anonymous, "the object is anonymous")
	if c.canDecommitZeroPages() {
		// Zircon skips reclaim for a discardable object not unlocked and for
		// one mapped uncached; neither is ported.
		pq.SetAnonymous(page, c, offset, false)
	} else {
		pq.SetWired(page, c, offset)
	}
}

// moveToNotPinnedLocked moves a queued page to the queue its state calls for.
func (c *CowPages) moveToNotPinnedLocked(page *VmPage, offset uint64) {
	pq := c.node.queues
	if c.pageSourceType() == UserPager {
		assert(page.dirtyState != Untracked, "a page a pager backs is dirty tracked")
		if page.dirtyState == Clean {
			pq.MoveToReclaim(page)
		} else {
			pq.MoveToPagerBackedDirty(page)
		}
		return
	}
	if c.canDecommitZeroPages() {
		pq.MoveToAnonymous(page, false)
	} else {
		pq.MoveToWired(page)
	}
}

// addPageTransaction is a page about to be added: the slot it goes in,
// allocated but not yet changed.
type addPageTransaction struct {
	slot      PageOrMarkerRef[VmPage]
	offset    uint64
	overwrite CanOverwriteSlot
}

// complete puts p in the slot and returns what was there.
func (t *addPageTransaction) complete(p PageOrMarker[VmPage]) PageOrMarker[VmPage] {
	ret := t.slot.SwapContent(p)
	t.slot = PageOrMarkerRef[VmPage]{}
	return ret
}

// cancel gives back a slot allocated and left Empty.
func (t *addPageTransaction) cancel(pl *PageList[VmPage]) {
	assert(t.slot.Valid(), "the transaction has a slot")
	if t.slot.Get().IsEmpty() {
		pl.ReturnEmptySlot(t.offset)
	}
	t.slot = PageOrMarkerRef[VmPage]{}
}

// beginAddPageWithSlotLocked starts an add into a slot the caller found.
func (c *CowPages) beginAddPageWithSlotLocked(offset uint64, slot PageOrMarkerRef[VmPage],
	overwrite CanOverwriteSlot) (addPageTransaction, error) {
	if err := c.checkOverwriteConditionsLocked(offset, slot.Get(), overwrite); err != nil {
		return addPageTransaction{}, err
	}
	assert(c.pageSourceType() == Anonymous || !slot.Get().IsEmpty() || !c.pageList.IsOffsetInZeroInterval(offset),
		"an empty slot given is in no interval")
	return addPageTransaction{slot: slot, offset: offset, overwrite: overwrite}, nil
}

// beginAddPageLocked finds and allocates the slot for an add at offset.
func (c *CowPages) beginAddPageLocked(offset uint64, overwrite CanOverwriteSlot) (addPageTransaction, error) {
	handling := NoIntervals
	// An object a user pager backs may have intervals, which a slot must be
	// split out of before it can be changed.
	if c.pageSourceType() == UserPager {
		if overwrite != OverwriteEmpty {
			handling = SplitInterval
		} else {
			handling = CheckForInterval
		}
	}
	slot, inInterval := c.pageList.LookupOrAllocate(offset, handling)
	if inInterval {
		assert(handling != NoIntervals, "intervals were expected")
		if handling != SplitInterval {
			assert(slot == nil, "no slot is returned in an interval that cannot be split")
			return addPageTransaction{}, ErrAlreadyExists
		}
		assert(slot != nil && slot.IsIntervalSlot(), "an interval split out a slot")
	}
	if slot == nil {
		return addPageTransaction{}, ErrNoMemory
	}
	if err := c.checkOverwriteConditionsLocked(offset, *slot, overwrite); err != nil {
		if slot.IsEmpty() {
			c.pageList.ReturnEmptySlot(offset)
		}
		return addPageTransaction{}, err
	}
	return addPageTransaction{slot: PageOrMarkerRef[VmPage]{slot: slot}, offset: offset, overwrite: overwrite}, nil
}

func (c *CowPages) checkOverwriteConditionsLocked(offset uint64, slot PageOrMarker[VmPage], overwrite CanOverwriteSlot) error {
	// Pages can be added while the object is new, but not once it is dead.
	assert(c.lifeCycle != lifeDead, "the object is not dead")
	if offset >= c.size {
		return ErrOutOfRange
	}
	if overwrite == OverwriteEmpty && !slot.IsEmpty() {
		return ErrAlreadyExists
	}
	if overwrite == OverwriteEmptyOrParent && !slot.IsEmpty() && !slot.IsParentContent() {
		return ErrAlreadyExists
	}
	if overwrite == OverwriteZeroMarkerOrInterval && slot.IsPageOrRef() {
		assert(c.pageSource == nil || !slot.IsPage() || c.pageSource.DebugIsPageOk(slot.Page(), offset),
			"the page source accepts its page")
		return ErrAlreadyExists
	}
	assert(overwrite == OverwritePageOrRef || !slot.IsPageOrRef(), "a page or reference is overwritten only when allowed")
	return nil
}

// completeAddPageLocked puts p in the transaction's slot, queues a page, and
// unmaps the offset where mappings may have covered what was there, unless
// deferred is nil. It returns what the slot held.
func (c *CowPages) completeAddPageLocked(t *addPageTransaction, p PageOrMarker[VmPage], deferred *DeferredOps) PageOrMarker[VmPage] {
	assert(!p.IsPageOrRef() || c.pageSource == nil || !p.IsPage() || c.pageSource.DebugIsPageOk(p.Page(), t.offset),
		"the page source accepts its page")
	// Markers are never put in a node using parent content markers.
	assert(!p.IsMarker() || !c.nodeHasParentContentMarkers(), "no marker goes where parent content markers are used")
	if p.IsPage() {
		c.setNotPinnedLocked(p.Page(), t.offset)
	}
	afterIsPopulated := p.IsPageOrRef()
	offset := t.offset
	old := t.complete(p)
	beforeWasPopulated := old.IsPageOrRef() || old.IsParentContent()
	if beforeWasPopulated && !afterIsPopulated {
		c.decrementPopulated(1)
	} else if !beforeWasPopulated && afterIsPopulated {
		c.incrementPopulated(1)
	}
	if deferred != nil && !old.IsReference() {
		// A reference cannot be mapped, so needs no range update. Nor can
		// an empty slot of an object a user pager backs, whose content was
		// unknown; the deferred ops still serialize the change. Departure:
		// an empty slot of a region's layer may be mapped to the identity
		// root's page it falls through to, which must be unmapped.
		if !(old.IsEmpty() && c.pageSourceType() == UserPager && c.roots == nil) {
			c.RangeChangeUpdateLocked(CowRange{offset, c.pageSize()}, Unmap, deferred)
		}
	}
	return old
}

func (c *CowPages) cancelAddPageLocked(t *addPageTransaction) { t.cancel(c.pageList) }

// addPageLocked adds p at offset, freeing p if it cannot.
func (c *CowPages) addPageLocked(offset uint64, p PageOrMarker[VmPage], overwrite CanOverwriteSlot,
	deferred *DeferredOps) (PageOrMarker[VmPage], error) {
	t, err := c.beginAddPageLocked(offset, overwrite)
	if err != nil {
		if p.IsPage() {
			c.freePage(p.ReleasePage())
		} else if p.IsReference() {
			c.freeReference(p.ReleaseReference())
		}
		return PageOrMarker[VmPage]{}, err
	}
	return c.completeAddPageLocked(&t, p, deferred), nil
}

// AddNewPageLocked adds a newly allocated page at offset, zeroed if zero.
// On an error the caller keeps the page. What the slot held is returned.
func (c *CowPages) AddNewPageLocked(offset uint64, page *VmPage, overwrite CanOverwriteSlot, zero bool,
	deferred *DeferredOps) (PageOrMarker[VmPage], error) {
	t, err := c.beginAddPageLocked(offset, overwrite)
	if err != nil {
		return PageOrMarker[VmPage]{}, err
	}
	return c.completeAddNewPageLocked(&t, page, zero, deferred), nil
}

func (c *CowPages) completeAddNewPageLocked(t *addPageTransaction, page *VmPage, zero bool, deferred *DeferredOps) PageOrMarker[VmPage] {
	assert(c.isPageRounded(t.offset), "the offset is page rounded")
	initializeVmPage(page)
	if zero {
		clear(page.data)
	}
	// A new page of an object a pager backs starts Clean, and only a zero
	// page may be added new.
	if c.pageSourceType() == UserPager {
		assert(zero || isZeroPage(page), "a new page of a paged object is zero")
		c.updateDirtyStateLocked(page, t.offset, Clean, true)
	}
	return c.completeAddPageLocked(t, Page(page), deferred)
}

// AddNewPagesLocked adds pages from startOffset on. The object takes the
// pages whatever happens.
func (c *CowPages) AddNewPagesLocked(startOffset uint64, pages []*VmPage, overwrite CanOverwriteSlot, zero bool,
	deferred *DeferredOps) error {
	assert(overwrite != OverwritePageOrRef, "pages or references are not overwritten")
	assert(c.isPageRounded(startOffset), "the offset is page rounded")
	offset := startOffset
	for i, p := range pages {
		// The range update is done once at the end.
		if _, err := c.AddNewPageLocked(offset, p, overwrite, zero, nil); err != nil {
			// Take back the pages placed so far.
			if offset > startOffset {
				freed := new(scopedPageFreedList)
				remover := pageRemover{freed: freed, c: c}
				var removed int64
				_ = c.pageList.RemovePages(func(slot *PageOrMarker[VmPage], _ uint64) error {
					assert(slot.IsPage(), "only pages were added")
					removed++
					remover.pushContent(slot)
					return nil
				}, startOffset, offset)
				freed.freePages(c)
				c.decrementPopulated(removed)
			}
			for _, q := range pages[i:] {
				c.freePage(q)
			}
			return err
		}
		offset += c.pageSize()
	}
	if deferred != nil {
		c.RangeChangeUpdateLocked(CowRange{startOffset, offset - startOffset}, Unmap, deferred)
	}
	return nil
}

// pageLookup is where the content of an offset was found: a cursor at its
// slot in the owner, the owner (empty when it is the object looked in), the
// offset in the owner, and the end of the range from the start that can be
// read from the owner without looking again.
type pageLookup struct {
	cursor      Cursor[VmPage]
	owner       lockedPtr
	ownerOffset uint64
	visibleEnd  uint64
}

// findPageContentLocked finds the content of offset: in this object, in its
// parents, or in the identity root a region's layer falls through to.
func (c *CowPages) findPageContentLocked(offset, maxOwnerLength uint64) pageLookup {
	thisOffset := offset
	ps := c.pageSize()
	var cur lockedPtr
	// Search up the clone chain for committed content.
	for offset < cur.lockedOr(c).parentLimit {
		owner := cur.lockedOr(c)
		parent := owner.parent
		assert(parent != nil, "an object that sees a parent has one")
		cursor := owner.pageList.LookupNearestMutableCursor(offset)
		p := cursor.Current()
		correctOffset := p != nil && cursor.Offset() == offset
		// Content here is the answer.
		if correctOffset && !p.IsEmpty() && !p.IsParentContent() {
			return pageLookup{cursor: cursor, owner: cur, ownerOffset: offset, visibleEnd: maxOwnerLength + thisOffset}
		}
		// Walking up: trim the length the owner is visible for to the parent
		// limit and to the next content here.
		if maxOwnerLength > ps {
			maxOwnerLength = min(maxOwnerLength, owner.parentLimit-offset)
			if maxOwnerLength > ps && p != nil {
				_ = owner.pageList.ForEveryPageInCursorRange(func(slot *PageOrMarker[VmPage], slotOffset uint64) error {
					assert(!slot.IsEmpty() && slotOffset >= offset, "the next content is after the offset")
					newOwnerLength := slotOffset - offset
					assert(newOwnerLength > 0 && newOwnerLength <= maxOwnerLength, "the next content shortens the length")
					maxOwnerLength = newOwnerLength
					return ErrStop
				}, cursor, offset+maxOwnerLength)
			}
		}
		offset += owner.parentOffset
		cur = lockPtr(c, parent)
	}
	owner := cur.lockedOr(c)
	// Departure: a region's layer that holds nothing at the offset, and is
	// not zero there by an interval, falls through to the identity root its
	// resolver names, in place of the parent walk above. The root is visible
	// for this page alone, since the next may belong to another root.
	if owner.roots != nil && owner.seesRootLocked(offset) {
		if root, rootOffset, ok := owner.roots.Locate(offset); ok {
			assert(root.isIdentityRoot(), "a region falls through to an identity root")
			cur.release()
			rootPtr := lockPtr(c, root)
			return pageLookup{
				cursor:      root.pageList.LookupMutableCursor(rootOffset),
				owner:       rootPtr,
				ownerOffset: rootOffset,
				visibleEnd:  thisOffset + ps,
			}
		}
	}
	return pageLookup{
		cursor:      owner.pageList.LookupMutableCursor(offset),
		owner:       cur,
		ownerOffset: offset,
		visibleEnd:  maxOwnerLength + thisOffset,
	}
}

// seesRootLocked reports whether a region's layer holds no content of its
// own at offset, so the offset's content is an identity root's.
func (c *CowPages) seesRootLocked(offset uint64) bool {
	slot := c.pageList.Lookup(offset)
	if slot != nil && !slot.IsEmpty() {
		return false
	}
	return !c.pageList.IsOffsetInZeroInterval(offset)
}

// unresolvedRunLocked is how much of [offset, offset+length) of a region's
// layer, from offset, no identity root holds, so the layer's own page source
// is to be asked for. An object with no resolver resolves nothing.
func (c *CowPages) unresolvedRunLocked(offset, length uint64) uint64 {
	if c.roots == nil {
		return length
	}
	for run := uint64(0); run < length; run += c.pageSize() {
		if _, _, ok := c.roots.Locate(offset + run); ok {
			return run
		}
	}
	return length
}

// findInitialPageContentLocked finds the content that would first populate
// offset, ignoring what the object holds there.
func (c *CowPages) findInitialPageContentLocked(offset uint64) pageLookup {
	ps := c.pageSize()
	if c.parent != nil && offset < c.parentLimit {
		parent := lockPtr(c, c.parent)
		out := c.parent.findPageContentLocked(offset+c.parentOffset, ps)
		if out.owner.get() == nil {
			out.owner = parent
		} else {
			parent.release()
		}
		return out
	}
	// Departure: a region's layer populates from its identity root.
	if c.roots != nil {
		if root, rootOffset, ok := c.roots.Locate(offset); ok {
			return pageLookup{
				cursor:      root.pageList.LookupMutableCursor(rootOffset),
				owner:       lockPtr(c, root),
				ownerOffset: rootOffset,
				visibleEnd:  offset + ps,
			}
		}
	}
	return pageLookup{cursor: invalidCursor(c.pageList), ownerOffset: offset, visibleEnd: offset + ps}
}

// PageWouldReadZeroLocked reports whether an offset holds no page and would
// read as zero.
func (c *CowPages) PageWouldReadZeroLocked(pageOffset uint64) bool {
	assert(c.isPageRounded(pageOffset), "the offset is page rounded")
	assert(pageOffset < c.size, "the offset is in the object")
	slot := c.pageList.Lookup(pageOffset)
	if slot != nil && slot.IsMarker() {
		return true
	}
	if c.pageSourceType() == UserPager &&
		((slot != nil && slot.IsIntervalZero()) || c.pageList.IsOffsetInZeroInterval(pageOffset)) {
		// The kernel supplies zeros in a zero interval.
		return true
	}
	// No page or reference here: look above.
	if slot == nil || !slot.IsPageOrRef() {
		content := c.findInitialPageContentLocked(pageOffset)
		defer content.owner.release()
		if content.cursor.Current() == nil {
			// Nothing above either: zero, unless a pager supplies it.
			return !c.isRootSourceUserPagerBacked()
		}
	}
	// Content here or above, assumed not zero.
	return false
}

// LookupLocked calls fn on every page in r, with its offset.
func (c *CowPages) LookupLocked(r CowRange, fn func(offset uint64, page *VmPage) error) error {
	if r.IsEmpty() {
		return ErrInvalidArgs
	}
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	start := c.roundDown(r.Offset)
	end := c.roundUp(r.End())
	return c.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], off uint64) error {
		if !p.IsPage() {
			return nil
		}
		return fn(off, p.Page())
	}, start, end)
}

// LookupReadableLocked calls fn on every page that reads in r, wherever it
// is held, with the offset it reads at here.
func (c *CowPages) LookupReadableLocked(r CowRange, fn func(offset uint64, page *VmPage) error) error {
	if r.IsEmpty() {
		return ErrInvalidArgs
	}
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	current := c.roundDown(r.Offset)
	end := c.roundUp(r.End())
	ps := c.pageSize()
	for current != end {
		// First the pages here. Anything not a page is skipped.
		err := c.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], offset uint64) error {
			if offset != current {
				if !p.IsIntervalEnd() && !c.nodeHasParentContentMarkers() {
					// A gap before this offset: find its content above.
					return ErrStop
				}
				// Advance to the gap or interval end.
				offset = current
			}
			if p.IsParentContent() {
				return ErrStop
			}
			assert(offset == current, "the walk is at the current offset")
			current = offset + ps
			if !p.IsPage() {
				return nil
			}
			return fn(offset, p.Page())
		}, current, end)
		if err != nil && err != ErrStop {
			return err
		}
		if current == end {
			break
		}
		// See whether a parent, or an identity root, holds the content.
		content := c.findPageContentLocked(current, end-current)
		assert(content.visibleEnd > current, "the content is visible for some length")
		ownerLength := content.visibleEnd - current
		base := current
		err = content.owner.lockedOr(c).pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], offset uint64) error {
			if !p.IsPage() {
				return nil
			}
			return fn(offset-content.ownerOffset+base, p.Page())
		}, content.ownerOffset, content.ownerOffset+ownerLength)
		content.owner.release()
		if err != nil && err != ErrStop {
			return err
		}
		if err == ErrStop {
			return nil
		}
		current += ownerLength
	}
	return nil
}

// DebugLookupReadable is LookupReadableLocked with the lock taken.
func (c *CowPages) DebugLookupReadable(r CowRange, fn func(offset uint64, page *VmPage) error) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.LookupReadableLocked(r, fn)
}

// releaseOwnedPagesLocked frees every page from start to the end, and stops
// the object seeing its parent from there.
func (c *CowPages) releaseOwnedPagesLocked(start uint64, freed *scopedPageFreedList) {
	c.releaseOwnedPagesRangeLocked(start, c.size-start, freed)
}

// releaseOwnedPagesRangeLocked frees every page the object owns in [offset,
// offset+len). Zircon also gives back its share of content in hidden
// parents; there are none.
func (c *CowPages) releaseOwnedPagesRangeLocked(offset, length uint64, freed *scopedPageFreedList) {
	assert(offset <= c.size, "the offset is in the object")
	assert(offset+length <= c.size, "the range is in the object")
	var removed int64
	remover := pageRemover{freed: freed, c: c}
	if offset == 0 && length == c.size {
		c.pageList.RemoveAllContent(func(p PageOrMarker[VmPage]) {
			if p.IsPageOrRef() || p.IsParentContent() {
				removed++
			}
			remover.pushContent(&p)
		})
	} else {
		_ = c.pageList.RemovePages(func(p *PageOrMarker[VmPage], _ uint64) error {
			if p.IsPageOrRef() || p.IsParentContent() {
				removed++
			}
			remover.pushContent(p)
			return nil
		}, offset, offset+length)
	}
	c.decrementPopulated(removed)
	// The object no longer sees its parent in the range freed.
	if offset+length >= c.parentLimit {
		c.parentLimit = min(c.parentLimit, offset)
	}
}

// DebugGetPageCountLocked counts the pages and references held.
func (c *CowPages) DebugGetPageCountLocked() uint64 {
	var count uint64
	_ = c.pageList.ForEveryPage(func(p *PageOrMarker[VmPage], _ uint64) error {
		if p.IsPageOrRef() {
			count++
		}
		return nil
	})
	return count
}

// DebugIsPage reports whether a page is at offset.
func (c *CowPages) DebugIsPage(offset uint64) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	p := c.pageList.Lookup(offset)
	return p != nil && p.IsPage()
}

// DebugIsMarker reports whether a zero marker is at offset.
func (c *CowPages) DebugIsMarker(offset uint64) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	p := c.pageList.Lookup(offset)
	return p != nil && p.IsMarker()
}

// DebugIsReference reports whether a compressed reference is at offset.
// Zircon's tests read this through attribution counts.
func (c *CowPages) DebugIsReference(offset uint64) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	p := c.pageList.Lookup(offset)
	return p != nil && p.IsReference()
}

// DebugIsEmpty reports whether nothing is at offset.
func (c *CowPages) DebugIsEmpty(offset uint64) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	p := c.pageList.Lookup(offset)
	return p == nil || p.IsEmpty()
}

// DebugGetPage is the page at offset, or nil.
func (c *CowPages) DebugGetPage(offset uint64) *VmPage {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.DebugGetPageLocked(offset)
}

// DebugGetPageLocked is DebugGetPage with the lock held.
func (c *CowPages) DebugGetPageLocked(offset uint64) *VmPage {
	assert(c.isPageRounded(offset), "the offset is page rounded")
	p := c.pageList.Lookup(offset)
	if p != nil && p.IsPage() {
		return p.Page()
	}
	return nil
}

// DebugGetParent is the object's parent, for tests.
func (c *CowPages) DebugGetParent() *CowPages {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.parent
}

// DebugLookupDepthLocked counts the parents up to the root.
func (c *CowPages) DebugLookupDepthLocked() int {
	depth := 0
	for p := c.parent; p != nil; p = p.parent {
		depth++
	}
	return depth
}

// DeferredOps is the work an operation leaves for after the object's lock is
// dropped, Zircon's VmCowPages::DeferredOps: the range change of the
// object's clones, and the freeing of pages. For a hierarchy a page source
// backs it also holds the source's lock across the operation, which
// serializes every change to the hierarchy.
type DeferredOps struct {
	self    *CowPages
	rangeOp *deferredRangeOp
	freed   scopedPageFreedList
	source  *PageSource
}

type deferredRangeOp struct {
	op RangeChangeOp
	r  CowRange
}

// NewDeferredOps is the deferred work of an operation on self, which must be
// made before self's lock is taken. Finish runs it after the lock is
// dropped.
func NewDeferredOps(self *CowPages) *DeferredOps {
	d := &DeferredOps{self: self}
	if self.rootHasPageSource() {
		self.lock.Lock()
		if self.lifeCycle != lifeAlive {
			// A dead object is out of the tree and has nothing to serialize
			// with.
			self.lock.Unlock()
			return d
		}
		cur := self
		for cur.parent != nil {
			cur = cur.parent
		}
		source := cur.pageSource
		self.lock.Unlock()
		assert(source != nil, "the root has its page source")
		source.pagedVmoMutex.Lock()
		d.source = source
	}
	return d
}

// addRange records a range change for the object's clones. Only one range
// is kept, covering all those asked for, with an UnmapZeroPage made an Unmap.
func (d *DeferredOps) addRange(self *CowPages, r CowRange, op RangeChangeOp) {
	assert(self == d.self, "the range is of the deferred ops' object")
	if d.rangeOp != nil {
		if d.rangeOp.op != op {
			if d.rangeOp.op == UnmapZeroPage && op == Unmap {
				d.rangeOp.op = op
			} else {
				assert(d.rangeOp.op == Unmap && op == UnmapZeroPage, "only unmaps combine")
			}
		}
		d.rangeOp.r = d.rangeOp.r.Cover(r)
		return
	}
	d.rangeOp = &deferredRangeOp{op: op, r: r}
}

// freedList is where pages to free go.
func (d *DeferredOps) freedList(self *CowPages) *scopedPageFreedList {
	assert(self == d.self, "the pages are of the deferred ops' object")
	return &d.freed
}

// Finish runs the deferred work, with the object's lock not held: Zircon's
// destructor. The pages are freed after the range change and before the
// page source's lock is dropped.
func (d *DeferredOps) Finish() {
	if d.rangeOp != nil {
		rangeChangeUpdateCowChildren(d.self, d.rangeOp.r, d.rangeOp.op)
	}
	d.freed.freePages(d.self)
	if d.source != nil {
		d.source.pagedVmoMutex.Unlock()
		d.source = nil
	}
}

// RangeChangeUpdateLocked applies op to every mapping of r: this object's
// now, its clones' through deferred. Without deferred only RemoveWrite may
// be asked for, which clones need not see as they map nothing writable.
func (c *CowPages) RangeChangeUpdateLocked(r CowRange, op RangeChangeOp, deferred *DeferredOps) {
	if len(c.children) != 0 || c.rootHasPageSource() {
		if deferred != nil {
			deferred.addRange(c, r, op)
		} else {
			assert(op == RemoveWrite, "only a write removal needs no deferred ops")
		}
	}
	if c.paged != nil && !r.IsEmpty() {
		r = c.expandTillPageAligned(r)
		c.rangeChangeUpdateMappingsLocked(c.paged, r, op)
	}
}

// rangeChangeUpdateMappingsLocked applies op to paged's mappings of r. Zircon
// skips pinned pages; pinning is not ported.
func (c *CowPages) rangeChangeUpdateMappingsLocked(paged *ObjectPaged, r CowRange, op RangeChangeOp) {
	assert(c.isPageAligned(r), "the range is page aligned")
	paged.rangeChangeUpdateLocked(r, op)
}

// rangeChangeUpdateCowChildren applies op to r in every clone below self
// that can see self's content there, skipping any clone, and its subtree,
// that holds content of its own over the range. Zircon walks the subtree
// with a cursor that survives nodes dying as it drops and takes their locks;
// the tree shares one lock here, so it walks the tree under it.
func rangeChangeUpdateCowChildren(self *CowPages, r CowRange, op RangeChangeOp) {
	if r.IsEmpty() {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	var walk func(node *CowPages, accumulativeOffset uint64)
	walk = func(node *CowPages, accumulativeOffset uint64) {
		for _, child := range node.children {
			childOffset := accumulativeOffset + child.parentOffset
			if checkRangeChangeCandidate(child, childOffset, r, op) {
				walk(child, childOffset)
			}
		}
	}
	walk(self, 0)
}

// checkRangeChangeCandidate applies the range change to candidate's mappings
// where it sees its parent, and reports whether its children need walking.
func checkRangeChangeCandidate(candidate *CowPages, accumulativeOffset uint64, r CowRange, op RangeChangeOp) bool {
	candidateOffset, candidateLen, ok := getIntersect(accumulativeOffset, candidate.size, r.Offset, r.Len)
	if !ok {
		// No intersection, so skip the node and its subtree.
		return false
	}
	assert(candidateOffset >= accumulativeOffset, "the intersection is in the candidate")
	candidateOffset -= accumulativeOffset
	assert(candidateOffset+candidateLen <= candidate.size, "the intersection is in range")
	// Find the gaps in the range, where the candidate sees its parent.
	firstGapStart := ^uint64(0)
	var lastGapEnd uint64
	_ = candidate.pageList.ForEveryPageAndGapInRange(func(page *PageOrMarker[VmPage], offset uint64) error {
		// A parent content marker is like a gap. Anything else hides the
		// parent, from this node and its children.
		if page.IsParentContent() {
			firstGapStart = min(firstGapStart, offset)
			lastGapEnd = max(lastGapEnd, offset+candidate.pageSize())
		}
		return nil
	}, func(start, end uint64) error {
		// A gap shows the parent, unless parent content markers are in use.
		if !candidate.nodeHasParentContentMarkers() {
			firstGapStart = min(firstGapStart, start)
			lastGapEnd = max(lastGapEnd, end)
		}
		return nil
	}, candidateOffset, candidateOffset+candidateLen)
	if firstGapStart >= lastGapEnd {
		return false
	}
	// Update the one range covering the gaps.
	if candidate.paged != nil {
		candidate.rangeChangeUpdateMappingsLocked(candidate.paged,
			CowRange{firstGapStart, lastGapEnd - firstGapStart}, op)
	}
	return true
}

// getIntersect is the intersection of two ranges, Zircon's GetIntersect.
func getIntersect(offset1, len1, offset2, len2 uint64) (uint64, uint64, bool) {
	end1 := offset1 + len1
	end2 := offset2 + len2
	start := max(offset1, offset2)
	end := min(end1, end2)
	if start >= end {
		return 0, 0, false
	}
	return start, end - start, true
}

// freeHeldLocked frees what a checkpoint holds in [offset, offset+len) of
// the D1 list: its pages, references and markers, and the part of any zero
// interval in the range. It is ours (dirty.go).
func (c *CowPages) freeHeldLocked(offset, length uint64, freed *scopedPageFreedList) {
	ps := c.pageSize()
	end := offset + length
	remover := pageRemover{freed: freed, c: c}
	_ = c.held.RemovePages(func(p *PageOrMarker[VmPage], _ uint64) error {
		if p.IsInterval() {
			// Taken out below, so the intervals stay whole.
			return nil
		}
		remover.pushContent(p)
		return nil
	}, offset, end)
	// The zero intervals' parts in range, found first as taking them out
	// changes the list.
	type part struct{ start, end uint64 }
	var parts []part
	_ = c.held.ForEveryPageAndContiguousRunInRange(func(p *PageOrMarker[VmPage], _ uint64) bool {
		return p.IsInterval()
	}, func(*PageOrMarker[VmPage], uint64) error { return nil },
		func(runStart, runEnd uint64, isInterval bool) error {
			assert(isInterval, "only intervals are left in the range")
			parts = append(parts, part{runStart, runEnd})
			return nil
		}, offset, end)
	for _, p := range parts {
		// Make every offset of the part a slot of its own, splitting the
		// interval at the part's ends, and take the slots out.
		err := c.held.PopulateSlotsInInterval(p.start, p.end)
		assert(err == nil, "a Go allocation does not fail")
		for off := p.start; off < p.end; off += ps {
			removed := c.held.RemoveContent(off)
			assert(removed.IsIntervalSlot(), "the part is interval slots")
		}
	}
}

// HeapAllocationBytesLocked is the memory the page list takes.
func (c *CowPages) HeapAllocationBytesLocked() uint64 { return c.pageList.HeapAllocationBytes() }
