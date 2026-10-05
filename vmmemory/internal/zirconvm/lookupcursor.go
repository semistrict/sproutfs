// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc and vm/include/vm/vm_cow_pages.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import "context"

// LookupCursor returns successive pages over a range of an object, Zircon's
// VmCowPages::LookupCursor. The object's lock must be held from
// GetLookupCursorLocked to the cursor's last use, and the lock of the owner
// of a page returned stays held until the next operation on the cursor.
//
// By default a new zero page is a zero fork and a page returned is marked
// accessed; DisableZeroFork and DisableMarkAccessed turn these off. An
// allocation list given with GiveAllocList serves allocations first.
type LookupCursor struct {
	target    *CowPages
	offset    uint64
	endOffset uint64

	// ownerInfo is where the content at offset was found, valid when
	// isValid. ownerCursor caches ownerInfo.cursor's current slot.
	ownerInfo   pageLookup
	ownerCursor PageOrMarkerRef[VmPage]

	targetDirectlyBackedByUserPager bool
	zeroFork                        bool
	markAccessed                    bool
	isValid                         bool

	allocList *[]*VmPage
}

// RequireResult is a page a Require method returns, and whether it may be
// written.
type RequireResult struct {
	Page     *VmPage
	Writable bool
}

// GetLookupCursorLocked is a cursor over r, which is page aligned and in the
// object.
func (c *CowPages) GetLookupCursorLocked(r CowRange) (*LookupCursor, error) {
	assert(!r.IsEmpty(), "the range is not empty")
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.lifeCycle == lifeAlive, "the object is alive")
	if r.Offset >= c.size || !r.IsBoundedBy(c.size) {
		return nil, ErrOutOfRange
	}
	// Zircon refuses a discardable object that was discarded; discardable
	// objects are not ported.
	direct := c.pageSourceType() == UserPager
	return &LookupCursor{
		target:                          c,
		offset:                          r.Offset,
		endOffset:                       r.End(),
		targetDirectlyBackedByUserPager: direct,
		zeroFork:                        !direct && c.canDecommitZeroPages(),
		markAccessed:                    true,
	}, nil
}

// Release drops the lock of the owner of the last page returned. Zircon's
// destructor does it.
func (lc *LookupCursor) Release() {
	lc.invalidateCursor()
	lc.ownerInfo.owner.release()
	assert(lc.allocList == nil, "the alloc list was cleared")
}

// GiveAllocList gives the cursor pages to allocate from first. ClearAllocList
// must be called before the cursor is released.
func (lc *LookupCursor) GiveAllocList(list *[]*VmPage) {
	assert(list != nil, "there is a list")
	lc.allocList = list
}

// ClearAllocList takes the allocation list back. Pages left in it are the
// caller's to free.
func (lc *LookupCursor) ClearAllocList() {
	assert(lc.allocList != nil, "there is a list")
	lc.allocList = nil
}

// DisableZeroFork stops new zero pages being queued as zero forks.
func (lc *LookupCursor) DisableZeroFork() { lc.zeroFork = false }

// DisableMarkAccessed stops pages returned being marked accessed.
func (lc *LookupCursor) DisableMarkAccessed() { lc.markAccessed = false }

func (lc *LookupCursor) pageSize() uint64 { return lc.target.pageSize() }

// incrementCursor moves to the next offset, invalidating the cursor where the
// next slot may need looking up again.
func (lc *LookupCursor) incrementCursor() {
	ps := lc.pageSize()
	lc.offset += ps
	if lc.offset == lc.ownerInfo.visibleEnd {
		// The end of the range, or of the owner's visible part: walk again
		// for the next slot.
		lc.invalidateCursor()
		return
	}
	// Step the owner's cursor.
	lc.ownerInfo.ownerOffset += ps
	lc.ownerInfo.cursor.Step()
	lc.ownerCursor = lc.ownerInfo.cursor.CurrentRef()
	// An empty slot in the owner may still see content above it, which
	// needs a walk from the bottom again.
	owner := lc.ownerInfo.owner.lockedOr(lc.target)
	canSeeParent := func() bool {
		// Departure: a region's layer sees its identity roots through any
		// slot that holds nothing and is in no interval.
		if owner.roots != nil && (!lc.ownerCursor.Valid() || lc.ownerCursor.Get().IsEmpty()) &&
			!owner.pageList.IsOffsetInZeroInterval(lc.ownerInfo.ownerOffset) {
			return true
		}
		if owner.parent == nil {
			return false
		}
		if lc.ownerInfo.ownerOffset >= owner.parentLimit {
			return false
		}
		if owner.nodeHasParentContentMarkers() {
			return lc.ownerCursor.Get().IsParentContent()
		}
		return lc.ownerCursor.Get().IsEmpty()
	}
	if !lc.ownerCursor.Valid() || canSeeParent() {
		lc.invalidateCursor()
	}
}

func (lc *LookupCursor) incrementOffsetAndInvalidateCursor(delta uint64) {
	lc.offset += delta
	lc.invalidateCursor()
}

func (lc *LookupCursor) isCursorValid() bool { return lc.isValid }

// establishCursor finds the owner of the current offset, if the cursor is
// not valid.
func (lc *LookupCursor) establishCursor() {
	if lc.isCursorValid() {
		return
	}
	// Drop the previous owner's lock now that the next page is needed.
	lc.ownerInfo.owner.release()
	assert(lc.offset < lc.endOffset, "the cursor is in range")
	lc.ownerInfo = lc.target.findPageContentLocked(lc.offset, lc.endOffset-lc.offset)
	lc.ownerCursor = lc.ownerInfo.cursor.CurrentRef()
	lc.isValid = true
}

func (lc *LookupCursor) targetIsOwner() bool { return lc.ownerInfo.owner.get() == nil }

// invalidateCursor makes the next lookup walk again, keeping the owner's
// lock until then.
func (lc *LookupCursor) invalidateCursor() { lc.isValid = false }

func (lc *LookupCursor) cursorIsPage() bool {
	return lc.ownerCursor.Valid() && lc.ownerCursor.Get().IsPage()
}

func (lc *LookupCursor) cursorIsMarker() bool {
	return lc.ownerCursor.Valid() && lc.ownerCursor.Get().IsMarker()
}

func (lc *LookupCursor) cursorIsEmpty() bool {
	return !lc.ownerCursor.Valid() || lc.ownerCursor.Get().IsEmpty()
}

func (lc *LookupCursor) cursorIsParentContent() bool {
	return lc.ownerCursor.Valid() && lc.ownerCursor.Get().IsParentContent()
}

func (lc *LookupCursor) cursorIsReference() bool {
	return lc.ownerCursor.Valid() && lc.ownerCursor.Get().IsReference()
}

// cursorIsIntervalZero is whether the cursor is at a sentinel.
func (lc *LookupCursor) cursorIsIntervalZero() bool {
	return lc.ownerCursor.Valid() && lc.ownerCursor.Get().IsIntervalZero()
}

// cursorIsInIntervalZero is whether the offset is in a zero interval.
func (lc *LookupCursor) cursorIsInIntervalZero() bool {
	return lc.cursorIsIntervalZero() ||
		lc.ownerInfo.owner.lockedOr(lc.target).pageList.IsOffsetInZeroInterval(lc.ownerInfo.ownerOffset)
}

// cursorIsContentZero reports whether the content is zero: a marker, or an
// empty slot that starts zero, which is one with no page source or one in a
// zero interval.
func (lc *LookupCursor) cursorIsContentZero() bool {
	if lc.cursorIsMarker() {
		return true
	}
	if lc.ownerInfo.owner.lockedOr(lc.target).pageSource != nil {
		return lc.cursorIsInIntervalZero()
	}
	return lc.cursorIsEmpty() || lc.cursorIsParentContent()
}

// cursorIsUsablePage reports whether there is a page ready to use as it is:
// any page to read, or an owned page needing no dirty transition to write.
func (lc *LookupCursor) cursorIsUsablePage(writing bool) bool {
	return lc.cursorIsPage() && (!writing || (lc.targetIsOwner() && !lc.targetDirtyTracked()))
}

// targetZeroContentSupplyDirty reports whether zero content should become a
// Dirty page. Only call it when cursorIsContentZero.
func (lc *LookupCursor) targetZeroContentSupplyDirty(writing bool) bool {
	if !lc.targetDirtyTracked() {
		return false
	}
	if writing {
		return true
	}
	// A marker starts clean.
	if lc.cursorIsMarker() {
		return false
	}
	assert(lc.cursorIsInIntervalZero(), "zero content of a dirty tracked object is a marker or an interval")
	// A zero interval is implicitly dirty, even read.
	return true
}

func (lc *LookupCursor) targetDirtyTracked() bool { return lc.targetDirectlyBackedByUserPager }

// pageAsResultNoIncrement is a page as a result: writable if it is in the
// target and needs no dirty transition.
func (lc *LookupCursor) pageAsResultNoIncrement(page *VmPage, inTarget bool) RequireResult {
	return RequireResult{
		Page:     page,
		Writable: inTarget && (!lc.targetDirectlyBackedByUserPager || page.dirtyState == Dirty),
	}
}

// cursorAsResult is the cursor's page as a result, marked accessed, and
// moves on.
func (lc *LookupCursor) cursorAsResult() RequireResult {
	if lc.markAccessed {
		lc.target.node.queues.MarkAccessed(lc.ownerCursor.Get().Page())
	}
	result := lc.pageAsResultNoIncrement(lc.ownerCursor.Get().Page(), lc.targetIsOwner())
	lc.incrementCursor()
	return result
}

// targetAllocateCopyPageAsResult puts a copy of source in the target at the
// current offset, in dirtyState if the target is dirty tracked, and moves on.
func (lc *LookupCursor) targetAllocateCopyPageAsResult(source *VmPage, dirtyState DirtyState,
	deferred *DeferredOps) (RequireResult, error) {
	target := lc.target
	zeroPage := target.node.pmm.ZeroPage()
	outPage, err := target.allocateCopyPage(source, lc.allocList)
	if err != nil {
		return RequireResult{}, err
	}
	if lc.targetDirectlyBackedByUserPager {
		// Departure: a region's layer copies the page of an identity root
		// it falls through to, as a snapshot-on-write child copies its
		// parent's. Zircon's target with a user pager copies only the zero
		// page, which it owns.
		assert(source == zeroPage || target.roots != nil, "a paged object copies only zero, or a root's page")
		assert(source == zeroPage || !lc.targetIsOwner(), "a root's page is not the target's")
		if dirtyState == Dirty {
			// D5: the copy takes its reservation before it is dirty.
			if err := target.reserveLocked(outPage); err != nil {
				target.freePage(outPage)
				return RequireResult{}, err
			}
		}
		target.updateDirtyStateLocked(outPage, lc.offset, dirtyState, true)
	}
	// The slot found can be reused when the target owns it, there is a slot,
	// and the owner has no intervals to split.
	canReuseSlot := lc.targetIsOwner() && lc.ownerInfo.cursor.Current() != nil &&
		lc.ownerInfo.owner.lockedOr(target).pageSourceType() != UserPager
	var t addPageTransaction
	if canReuseSlot {
		t, err = target.beginAddPageWithSlotLocked(lc.offset, lc.ownerInfo.cursor.CurrentRef(), OverwriteZeroMarkerOrInterval)
	} else {
		t, err = target.beginAddPageLocked(lc.offset, OverwriteZeroMarkerOrInterval)
	}
	if err != nil {
		target.freePage(outPage)
		return RequireResult{}, err
	}
	// A fork of the zero page takes the cheaper unmap below instead.
	var completeDeferred *DeferredOps
	if source != zeroPage {
		completeDeferred = deferred
	}
	old := target.completeAddPageLocked(&t, Page(outPage), completeDeferred)
	assert(!old.IsPageOrRef(), "the copy replaced no page")
	if source == zeroPage {
		target.RangeChangeUpdateLocked(CowRange{lc.offset, lc.pageSize()}, UnmapZeroPage, deferred)
	}
	// A fork of the zero page asked to be marked goes in the zero fork
	// queue.
	if lc.zeroFork && source == zeroPage {
		target.node.queues.MoveAnonymousToAnonymousZeroFork(outPage)
	}
	if lc.targetIsOwner() {
		if !canReuseSlot {
			// A node may have been made, so look up again.
			lc.incrementOffsetAndInvalidateCursor(lc.pageSize())
		} else {
			assert(lc.cursorIsPage(), "the cursor holds the new page")
			assert(lc.ownerCursor.Get().Page() == outPage, "the cursor holds the new page")
			lc.incrementCursor()
		}
	} else {
		// The owner's page list did not change.
		lc.incrementCursor()
	}
	return lc.pageAsResultNoIncrement(outPage, true), nil
}

// cursorReferenceToPage decompresses the cursor's reference in its owner.
func (lc *LookupCursor) cursorReferenceToPage(ctx context.Context) error {
	assert(lc.cursorIsReference(), "the cursor is at a reference")
	return lc.ownerInfo.owner.lockedOr(lc.target).replaceReferenceWithPageLocked(ctx, lc.ownerCursor, lc.ownerInfo.ownerOffset)
}

// readRequest asks the owner's page source for the content from the current
// offset, up to maxRequestPages.
func (lc *LookupCursor) readRequest(maxRequestPages uint64, pageRequest *PageRequest) error {
	owner := lc.ownerInfo.owner.lockedOr(lc.target)
	ps := lc.pageSize()
	assert(owner.pageSource != nil, "the owner has a page source")
	assert(lc.cursorIsEmpty(), "the content is absent")
	assert(!lc.cursorIsInIntervalZero(), "the content is not in a zero interval")
	assert(lc.offset+ps*maxRequestPages <= lc.endOffset, "the request is in the cursor's range")
	assert(maxRequestPages > 0, "a page is requested")
	requestSize := maxRequestPages * ps
	if !lc.targetIsOwner() {
		assert(lc.ownerInfo.visibleEnd > lc.offset, "the owner is visible")
		requestSize = min(requestSize, lc.ownerInfo.visibleEnd-lc.offset)
	} else {
		// Departure: a region's layer asks its own source only for the run
		// no identity root holds.
		requestSize = max(ps, owner.unresolvedRunLocked(lc.offset, requestSize))
	}
	// Request only absent pages: stop at the first present in the owner.
	if requestSize > ps {
		_ = owner.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], offset uint64) error {
			assert(!p.IsParentContent(), "no parent content marker is used")
			assert(offset > lc.ownerInfo.ownerOffset, "the first offset was empty")
			assert(!p.IsInterval() || p.IsIntervalSlot() || p.IsIntervalStart(), "an interval found starts in range")
			newSize := offset - lc.ownerInfo.ownerOffset
			assert(newSize < requestSize, "the request is trimmed")
			requestSize = newSize
			return ErrStop
		}, lc.ownerInfo.ownerOffset, lc.ownerInfo.ownerOffset+requestSize)
	}
	assert(requestSize >= ps, "a page is requested")
	err := owner.pageSource.GetPages(lc.ownerInfo.ownerOffset, requestSize, pageRequest)
	// A pager never supplies a page synchronously.
	assert(err != nil, "the read did not succeed synchronously")
	return err
}

// dirtyRequest prepares the target's range from the current offset for
// writing.
func (lc *LookupCursor) dirtyRequest(ctx context.Context, maxRequestPages uint64, pageRequest *PageRequest,
	deferred *DeferredOps) error {
	ps := lc.pageSize()
	assert(lc.targetIsOwner(), "the target owns what it dirties")
	assert(lc.target.parent == nil, "a dirty tracked target has no parent")
	assert(lc.target.pageSource != nil, "the target has a page source")
	assert(maxRequestPages > 0, "a page is dirtied")
	assert(lc.offset+ps*maxRequestPages <= lc.endOffset, "the request is in the cursor's range")
	dirtyLen, err := lc.target.prepareForWriteLocked(ctx, CowRange{lc.offset, ps * maxRequestPages}, pageRequest, deferred)
	if err == nil {
		assert(dirtyLen != 0 && dirtyLen <= maxRequestPages*ps, "something was dirtied")
	} else {
		assert(dirtyLen == 0, "nothing was dirtied")
	}
	return err
}

// MaybePage is the current page if it is usable as it is, or nil, and moves
// on either way.
func (lc *LookupCursor) MaybePage(willWrite bool) *VmPage {
	lc.establishCursor()
	var page *VmPage
	if lc.cursorIsUsablePage(willWrite) {
		page = lc.ownerCursor.Get().Page()
	}
	if page != nil && lc.markAccessed {
		lc.target.node.queues.MarkAccessed(page)
	}
	lc.incrementCursor()
	return page
}

// SkipMissingPages moves past pages MaybePage would not return, and returns
// how many.
func (lc *LookupCursor) SkipMissingPages() uint64 {
	lc.establishCursor()
	if !lc.cursorIsEmpty() || lc.cursorIsInIntervalZero() {
		return 0
	}
	ps := lc.pageSize()
	owner := lc.ownerInfo.owner.lockedOr(lc.target)
	possiblyEmpty := lc.ownerInfo.visibleEnd - lc.offset
	if lc.targetIsOwner() {
		// Departure: up to the next offset an identity root holds.
		possiblyEmpty = max(ps, owner.unresolvedRunLocked(lc.offset, possiblyEmpty))
	}
	if possiblyEmpty > ps {
		_ = owner.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], offset uint64) error {
			assert(offset > lc.ownerInfo.ownerOffset, "the first offset was empty")
			assert(!p.IsInterval() || p.IsIntervalSlot() || p.IsIntervalStart(), "an interval found starts in range")
			newSize := offset - lc.ownerInfo.ownerOffset
			assert(newSize < possiblyEmpty, "the run is trimmed")
			possiblyEmpty = newSize
			return ErrStop
		}, lc.ownerInfo.ownerOffset, lc.ownerInfo.ownerOffset+possiblyEmpty)
	}
	assert(possiblyEmpty >= ps, "at least a page was empty")
	assert(possiblyEmpty+lc.offset <= lc.endOffset, "the run is in range")
	lc.incrementOffsetAndInvalidateCursor(possiblyEmpty)
	return possiblyEmpty / ps
}

// IfExistPages fills pages with up to maxPages contiguous pages usable as
// they are, and moves past them. It returns how many.
func (lc *LookupCursor) IfExistPages(willWrite bool, maxPages uint64, pages []*VmPage) uint64 {
	ps := lc.pageSize()
	assert(lc.offset+ps*maxPages <= lc.endOffset, "the pages are in range")
	assert(uint64(len(pages)) >= maxPages, "there is room for the pages")
	lc.establishCursor()
	// Only pages ready to use, with no transitions and no access to mark.
	if !lc.cursorIsUsablePage(willWrite) || lc.markAccessed {
		return 0
	}
	if !lc.targetIsOwner() {
		maxPages = min(maxPages, (lc.ownerInfo.visibleEnd-lc.offset)/ps)
	}
	assert(maxPages > 0, "a page is wanted")
	var n uint64
	_ = lc.ownerInfo.cursor.ForEveryContiguous(func(p *PageOrMarker[VmPage]) error {
		if p.IsPage() {
			pages[n] = p.Page()
			n++
			if n == maxPages {
				return ErrStop
			}
			return nil
		}
		return ErrStop
	})
	lc.incrementOffsetAndInvalidateCursor(n * ps)
	return n
}

// RequireOwnedPage is the page at the current offset, owned by the target:
// content elsewhere is copied in, references decompressed, zeros forked, and
// absent content asked for, returning ErrShouldWait with pageRequest to wait
// on. If willWrite, the page is made ready to write, which may make a dirty
// request. On success the cursor moves on.
func (lc *LookupCursor) RequireOwnedPage(ctx context.Context, willWrite bool, maxRequestPages uint64,
	deferred *DeferredOps, pageRequest *MultiPageRequest) (RequireResult, error) {
	assert(pageRequest != nil, "there is a page request")
	lc.establishCursor()
	// Decompress a reference in place.
	if lc.cursorIsReference() {
		if err := lc.cursorReferenceToPage(ctx); err != nil {
			return RequireResult{}, err
		}
	}
	// A page in the target is the only case where an existing page may be
	// dirtied.
	if lc.targetIsOwner() && lc.cursorIsPage() {
		if willWrite && lc.targetDirectlyBackedByUserPager {
			// Zircon replaces a loaned page here first; loaned pages are not
			// ported. A page not dirty yet is prepared for the write.
			if lc.ownerCursor.Get().Page().dirtyState != Dirty {
				if err := lc.dirtyRequest(ctx, maxRequestPages, pageRequest.getLazy(), deferred); err != nil {
					if err == ErrShouldWait {
						pageRequest.madeDirtyRequest()
					}
					return RequireResult{}, err
				}
			}
		}
		return lc.cursorAsResult(), nil
	}
	// A page the target does not own is copied into it: copy on write.
	if lc.cursorIsPage() {
		assert(!lc.targetIsOwner(), "the page is not the target's")
		// Forking is an access, whether or not the result is marked.
		lc.target.node.queues.MarkAccessed(lc.ownerCursor.Get().Page())
		// Zircon forks a hidden parent's page with CloneCowPageLocked; there
		// are no hidden nodes, so the page is copied from its visible owner.
		dirtyState := Untracked
		if lc.targetDirectlyBackedByUserPager {
			// Departure: a region's layer copying an identity root's page
			// for a write needs it dirty, and asks first if its source traps
			// dirty transitions; a copy to read is Clean.
			dirtyState = Clean
			if willWrite {
				dirtyState = Dirty
				if lc.target.pageSource.ShouldTrapDirtyTransitions() {
					err := lc.dirtyRequest(ctx, 1, pageRequest.getLazy(), deferred)
					assert(err != nil, "a trapped dirty transition does not succeed synchronously")
					if err == ErrShouldWait {
						pageRequest.madeDirtyRequest()
					}
					return RequireResult{}, err
				}
			}
		}
		return lc.targetAllocateCopyPageAsResult(lc.ownerCursor.Get().Page(), dirtyState, deferred)
	}
	// Zero content may need a dirty request even to read, and the page it
	// makes may or may not be dirty.
	if lc.cursorIsContentZero() {
		targetPageDirty := lc.targetZeroContentSupplyDirty(willWrite)
		if targetPageDirty && lc.target.pageSource.ShouldTrapDirtyTransitions() {
			err := lc.dirtyRequest(ctx, maxRequestPages, pageRequest.getLazy(), deferred)
			// A source that traps never succeeds synchronously.
			assert(err != nil, "a trapped dirty transition does not succeed synchronously")
			if err == ErrShouldWait {
				pageRequest.madeDirtyRequest()
			}
			return RequireResult{}, err
		}
		// Zircon forks a marker of a hidden node here; there are none.
		state := Clean
		if targetPageDirty {
			state = Dirty
		}
		return lc.targetAllocateCopyPageAsResult(lc.target.node.pmm.ZeroPage(), state, deferred)
	}
	assert(lc.cursorIsEmpty(), "the content is absent")
	// Ask the owner's source for the content. Even a write reads first, then
	// dirties.
	return RequireResult{}, lc.readRequest(maxRequestPages, pageRequest.getReadRequest())
}

// RequireReadPage is the page that reads at the current offset, which may be
// the zero page or a parent's page. Absent content is asked for, returning
// ErrShouldWait with pageRequest to wait on. On success the cursor moves on.
func (lc *LookupCursor) RequireReadPage(ctx context.Context, maxRequestPages uint64, deferred *DeferredOps,
	pageRequest *MultiPageRequest) (RequireResult, error) {
	assert(pageRequest != nil, "there is a page request")
	lc.establishCursor()
	if lc.cursorIsPage() || lc.cursorIsReference() {
		if lc.cursorIsReference() {
			if err := lc.cursorReferenceToPage(ctx); err != nil {
				return RequireResult{}, err
			}
			assert(lc.cursorIsPage(), "the reference is a page now")
		}
		return lc.cursorAsResult(), nil
	}
	if lc.cursorIsContentZero() {
		lc.incrementCursor()
		return RequireResult{Page: lc.target.node.pmm.ZeroPage()}, nil
	}
	return RequireResult{}, lc.readRequest(maxRequestPages, pageRequest.getReadRequest())
}

// RequirePage is RequireOwnedPage to write and RequireReadPage to read.
func (lc *LookupCursor) RequirePage(ctx context.Context, willWrite bool, maxRequestPages uint64,
	deferred *DeferredOps, pageRequest *MultiPageRequest) (RequireResult, error) {
	if willWrite {
		return lc.RequireOwnedPage(ctx, true, maxRequestPages, deferred, pageRequest)
	}
	return lc.RequireReadPage(ctx, maxRequestPages, deferred, pageRequest)
}
