// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc and vm/include/vm/vm_cow_pages.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import "context"

// SupplyOptions is how pages are supplied, Zircon's SupplyOptions.
type SupplyOptions uint8

const (
	// PagerSupply fills only offsets with no content, as a pager answers a
	// read.
	PagerSupply SupplyOptions = iota
	// TransferData overwrites whatever is there.
	TransferData
)

// CommitRangeLocked commits r, which is page aligned and in the object,
// returning how much it committed. With content to read it returns
// ErrShouldWait with pageRequest to wait on, having committed what it could.
func (c *CowPages) CommitRangeLocked(ctx context.Context, r CowRange, deferred *DeferredOps,
	pageRequest *MultiPageRequest) (uint64, error) {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(r.IsBoundedBy(c.size), "the range is in the object")
	// With a page source the source provides the pages, and a child of one
	// does not preallocate, as something else may touch the object while it
	// waits. Otherwise allocate all the pages up front.
	var pageList []*VmPage
	if !c.rootHasPageSource() {
		count := r.Len / ps
		_ = c.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], _ uint64) error {
			if p.IsPage() {
				count--
			}
			return nil
		}, r.Offset, r.End())
		if count == 0 {
			return r.Len, nil
		}
		for range count {
			p, err := c.node.pmm.AllocPage()
			if err != nil {
				for _, q := range pageList {
					c.freePage(q)
				}
				return 0, err
			}
			pageList = append(pageList, p)
		}
	}
	defer func() {
		for _, p := range pageList {
			c.freePage(p)
		}
	}()
	cursor, err := c.GetLookupCursorLocked(r)
	if err != nil {
		return 0, err
	}
	// A commit wants pages, which should not be deduplicated to zero again.
	cursor.DisableZeroFork()
	cursor.GiveAllocList(&pageList)
	defer cursor.Release()
	offset := r.Offset
	end := r.End()
	var status error
	for offset < end {
		if _, err := cursor.RequireOwnedPage(ctx, false, (end-offset)/ps, deferred, pageRequest); err != nil {
			status = err
			break
		}
		offset += ps
	}
	cursor.ClearAllocList()
	return offset - r.Offset, status
}

// DecommitRange frees the pages in r, which then read as zero. An object
// whose empty slots do not read zero, a child or one a pager backs, does not
// support it.
func (c *CowPages) DecommitRange(r CowRange) error {
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if r.IsEmpty() {
		return nil
	}
	if c.parent != nil || c.pageSourceType() == UserPager {
		return ErrNotSupported
	}
	assert(c.pageSourceType() == Anonymous, "the object is anonymous")
	if !c.isPageAligned(r) {
		return ErrInvalidArgs
	}
	c.unmapAndFreePagesLocked(r.Offset, r.Len, deferred)
	return nil
}

// unmapAndFreePagesLocked unmaps and frees every page in [offset,
// offset+len) of an object with no parent. Zircon also returns how many it
// freed, for discardable objects, which are not ported; and a failure, for
// pinned pages, which are not either.
func (c *CowPages) unmapAndFreePagesLocked(offset, length uint64, deferred *DeferredOps) {
	// Zircon refuses a range with pinned pages; pinning is not ported.
	assert(CowRange{offset, length}.IsBoundedBy(c.size), "the range is in the object")
	assert(c.isPageRounded(offset), "the offset is page rounded")
	assert(c.isPageRounded(length) || offset+length == c.size, "the length is page rounded")
	assert(c.parent == nil, "the object has no parent")
	c.RangeChangeUpdateLocked(CowRange{offset, length}, Unmap, deferred)
	var removed int64
	remover := pageRemover{freed: deferred.freedList(c), c: c}
	_ = c.pageList.RemovePages(func(p *PageOrMarker[VmPage], _ uint64) error {
		if p.IsPageOrRef() || p.IsParentContent() {
			removed++
		}
		remover.pushContent(p)
		return nil
	}, offset, offset+length)
	c.decrementPopulated(removed)
}

// TakePages moves the content of r into pages, at spliceOffset, leaving r
// zero, and returns how much it took. An object a page source backs cannot
// give its pages away. Content seen from a parent is copied in to be taken,
// which may return ErrShouldWait with pageRequest to wait on, having taken
// what it could.
func (c *CowPages) TakePages(ctx context.Context, r CowRange, spliceOffset uint64, pages *PageSpliceList[VmPage],
	pageRequest *MultiPageRequest) (uint64, error) {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	if !r.IsBoundedBy(c.size) {
		pages.Finalize()
		return 0, ErrOutOfRange
	}
	if c.pageSource != nil {
		pages.Finalize()
		return 0, ErrNotSupported
	}
	// Zircon refuses a range with pinned pages; pinning is not ported.
	// Unmap the whole range first, so a failure midway loses nothing.
	c.RangeChangeUpdateLocked(r, Unmap, deferred)
	compression := c.node.compression
	// With no parent and an empty splice list, whole nodes move across.
	if c.parent == nil && pages.IsEmpty() && spliceOffset == 0 {
		foundPage := false
		var removed int64
		pages.AddPagesFrom(func(src, dst *PageOrMarker[VmPage], _ uint64) {
			foundPage = true
			// Splice lists hold no intervals, and there is no parent.
			assert(!src.IsInterval(), "a splice list holds no interval")
			assert(!src.IsParentContent(), "no parent content marker is used")
			if src.IsPageOrRef() {
				removed++
			}
			if src.IsPage() {
				c.node.queues.Remove(src.Page())
			} else if src.IsReference() {
				// A reference may go in the splice list for the receiver to
				// deal with, but the temporary reference must be its page.
				if moved, ok := compression.MoveReference(src.Reference()); ok {
					initializeVmPage(moved.Page)
					moved.Page.shareCount = moved.Metadata
					ref := src.SwapReferenceForPage(moved.Page)
					assert(compression.IsTempReference(ref), "only the temporary reference moves")
				}
			}
			dst.Set(src.Take())
		}, c.pageList, r.Offset)
		// No page found means a gap or an interval; not an interval.
		assert(foundPage || !c.pageList.IsOffsetInZeroInterval(r.Offset), "the range is not in an interval")
		c.decrementPopulated(removed)
		return r.Len, nil
	}
	// Otherwise take a page at a time. A gap that sees a parent's content is
	// copied in first, and taken in a second pass, as often as gaps are
	// found.
	var processed uint64
	for {
		var removed int64
		var takenLen uint64
		removePageCallback := func(slot *PageOrMarker[VmPage], offset uint64) error {
			if slot.IsMarker() {
				// Zero already: the splice list can keep a gap.
				return nil
			}
			assert(!slot.IsParentContent(), "no parent content marker is used")
			if slot.IsReference() {
				if moved, ok := compression.MoveReference(slot.Reference()); ok {
					initializeVmPage(moved.Page)
					moved.Page.shareCount = moved.Metadata
					ref := slot.SwapReferenceForPage(moved.Page)
					assert(compression.IsTempReference(ref), "only the temporary reference moves")
				}
			} else if slot.IsPage() {
				c.node.queues.Remove(slot.Page())
			}
			assert(slot.IsPageOrRef(), "the slot holds a page or reference")
			removed++
			if err := pages.Insert(offset-r.Offset+spliceOffset, slot.Take()); err != nil {
				assert(err == ErrNoMemory, "only an allocation fails")
				takenLen = offset - r.Offset
				return err
			}
			// A marker zeroes the offset where a parent would show through.
			parentHasContent := func(offset uint64) bool {
				content := c.findInitialPageContentLocked(offset)
				defer content.owner.release()
				return content.cursor.Current() != nil
			}
			if !c.nodeHasParentContentMarkers() && (c.rootHasPageSource() || parentHasContent(offset)) {
				*slot = Marker[VmPage]()
			}
			return nil
		}
		foundGapStart := r.End()
		foundGapEnd := foundGapStart
		err := c.pageList.RemovePagesAndIterateGaps(removePageCallback, func(gapStart, gapEnd uint64) error {
			if c.nodeHasParentContentMarkers() {
				return nil
			}
			foundGapStart, foundGapEnd = gapStart, gapEnd
			return ErrStop
		}, r.Offset+processed, r.End())
		c.decrementPopulated(removed)
		if err != nil && err != ErrStop {
			return takenLen, err
		}
		if foundGapStart < foundGapEnd {
			// The gap shows a parent's content, or a pager's: copy it in to
			// take it.
			gapLen := foundGapEnd - foundGapStart
			cursor, err := c.GetLookupCursorLocked(CowRange{foundGapStart, gapLen})
			if err != nil {
				return foundGapStart - r.Offset, err
			}
			for offset := uint64(0); offset < gapLen; offset += ps {
				if _, err := cursor.RequireOwnedPage(ctx, true, (gapLen-offset)/ps, deferred, pageRequest); err != nil {
					cursor.Release()
					takenLen = foundGapStart + offset - r.Offset
					// Only a wait needs the progress kept.
					if err != ErrShouldWait || offset == 0 {
						return takenLen, err
					}
					removed = 0
					rerr := c.pageList.RemovePages(removePageCallback, foundGapStart, foundGapStart+offset)
					c.decrementPopulated(removed)
					if rerr == nil {
						return takenLen, ErrShouldWait
					}
					// Another error takes precedence over the wait.
					pageRequest.CancelRequests()
					return takenLen, rerr
				}
			}
			cursor.Release()
		}
		// Go again from the gap just committed. With no gap found the start
		// was the end, which ends the loop.
		processed = foundGapStart - r.Offset
		if processed >= r.Len {
			break
		}
	}
	pages.Finalize()
	return r.Len, nil
}

// ProcessPagesForSupply turns the references in pages into pages, for an
// object a page source backs, which holds pages only.
func (c *CowPages) ProcessPagesForSupply(ctx context.Context, pages *PageSpliceList[VmPage]) error {
	if c.pageSource == nil {
		return nil
	}
	assert(c.pageSourceType() == UserPager, "a user pager backs the object")
	return pages.MutatePages(func(slot PageOrMarkerRef[VmPage], _ uint64) error {
		if slot.Get().IsReference() {
			// Zircon waits for the pmm here and goes again; a Go allocation
			// succeeds or fails at once.
			return c.makePageFromReference(ctx, slot)
		}
		return nil
	}, 0)
}

// SupplyPagesLocked puts the content of pages in r. A pager's supply fills
// only offsets with no content, freeing what it does not use; a transfer
// overwrites. Gaps in the splice list are zeros, as markers.
func (c *CowPages) SupplyPagesLocked(r CowRange, pages *PageSpliceList[VmPage], options SupplyOptions,
	deferred *DeferredOps) error {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(options != PagerSupply || c.pageSource != nil, "a pager supplies to an object it backs")
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if options == TransferData {
		if c.pageSource != nil {
			return ErrNotSupported
		}
		// Zircon refuses a range with pinned pages; pinning is not ported.
	}
	if c.pageSource != nil && c.pageSource.IsDetached() {
		return ErrBadState
	}
	overwritePolicy := OverwriteEmpty
	if options == TransferData {
		overwritePolicy = OverwritePageOrRef
	}
	start := r.Offset
	end := r.End()
	// [newPagesStart, newPagesStart+newPagesLen) is the run of new content.
	newPagesStart := start
	var newPagesLen, suppliedPagesLen uint64
	initialPosition := pages.Position()
	compression := c.node.compression
	handleAddPageResult := func(oldPage PageOrMarker[VmPage], _ uint64) {
		if oldPage.IsPage() {
			page := oldPage.ReleasePage()
			c.node.queues.Remove(page)
			deferred.freedList(c).add(page)
		} else if oldPage.IsReference() {
			compression.Free(oldPage.ReleaseReference())
		}
		// Zircon gives back the share of a hidden ancestor's page that a
		// parent content marker, or an empty slot of a child of a hidden
		// node, saw; there are none.
		assert(!oldPage.IsParentContent(), "no parent content marker is used")
	}
	handleAddPageError := func(err error, offset uint64) error {
		if c.nodeHasParentContentMarkers() {
			return err
		}
		if err == ErrAlreadyExists {
			// The end of a run of absent pages: tell the source of the run.
			if newPagesLen > 0 {
				c.RangeChangeUpdateLocked(CowRange{newPagesStart, newPagesLen}, Unmap, deferred)
				if c.pageSource != nil {
					c.pageSource.OnPagesSupplied(newPagesStart, newPagesLen)
				}
			}
			newPagesStart = offset + ps
			newPagesLen = 0
			suppliedPagesLen += ps
			return nil
		}
		// Only an allocation of a page list node can fail.
		assert(err == ErrNoMemory, "only an allocation fails")
		return err
	}
	err := pages.RemovePagesAndIterateGaps(func(slot PageOrMarker[VmPage], srcOffset uint64) error {
		assert(!slot.IsInterval(), "a splice list holds no interval")
		assert(!slot.IsEmpty(), "the slot holds content")
		// A page source takes pages only.
		assert(!slot.IsReference() || c.pageSource == nil, "references were made pages")
		dstOffset := start + srcOffset
		// A supplied page starts Clean.
		if slot.IsPage() && c.pageSourceType() == UserPager {
			c.updateDirtyStateLocked(slot.Page(), dstOffset, Clean, true)
		}
		assert(overwritePolicy == OverwriteEmpty || c.pageSource == nil, "a pager overwrites nothing")
		old, err := c.addPageLocked(dstOffset, slot, overwritePolicy, nil)
		if err != nil {
			return handleAddPageError(err, dstOffset)
		}
		assert(overwritePolicy != OverwriteEmpty || old.IsEmpty(), "nothing was overwritten")
		handleAddPageResult(old, dstOffset)
		newPagesLen += ps
		assert(newPagesStart+newPagesLen <= end, "the run is in range")
		suppliedPagesLen += ps
		return nil
	}, func(gapStart, gapEnd uint64) error {
		gapDstStart := gapStart + start
		gapDstEnd := gapEnd + start
		// A gap is zeros, which a marker makes explicit; the source of a
		// supply has no page source, so a gap is zeros.
		for zeroOffset := gapDstStart; zeroOffset < gapDstEnd; zeroOffset += ps {
			old, err := c.addPageLocked(zeroOffset, Marker[VmPage](), overwritePolicy, nil)
			if err != nil {
				if addErr := handleAddPageError(err, zeroOffset); addErr != nil {
					return addErr
				}
				continue
			}
			handleAddPageResult(old, zeroOffset)
			newPagesLen += ps
			suppliedPagesLen += ps
		}
		return nil
	})
	assert(start+suppliedPagesLen == end || err != nil, "every page of the splice list was supplied")
	if newPagesLen > 0 {
		c.RangeChangeUpdateLocked(CowRange{newPagesStart, newPagesLen}, Unmap, deferred)
		if c.pageSource != nil {
			c.pageSource.OnPagesSupplied(newPagesStart, newPagesLen)
		}
	}
	assert(pages.Position()-initialPosition == suppliedPagesLen || err != nil, "the splice list moved as far as supplied")
	return err
}

// FailPageRequestsLocked fails the outstanding requests in r with status. It
// changes nothing in the object.
func (c *CowPages) FailPageRequestsLocked(r CowRange, status error) error {
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource != nil, "the object has a page source")
	if !IsValidInternalFailureCode(status) {
		return ErrInvalidArgs
	}
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if c.pageSource.IsDetached() {
		return ErrBadState
	}
	c.pageSource.OnPagesFailed(r.Offset, r.Len, status)
	return nil
}

// PromoteRangeForReclamation moves the clean pages a pager backs in r to the
// don't-need queue, to be reclaimed first. Only pages present are moved.
func (c *CowPages) PromoteRangeForReclamation(r CowRange) error {
	ps := c.pageSize()
	// Hints apply only to objects a pager backs.
	if !c.canRootSourceEvict() {
		return nil
	}
	if r.IsEmpty() {
		return nil
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	startOffset := c.roundDown(r.Offset)
	endOffset := c.roundUp(r.End())
	cursor, err := c.GetLookupCursorLocked(CowRange{startOffset, endOffset - startOffset})
	if err != nil {
		return err
	}
	defer cursor.Release()
	// The aim is to reclaim them, not to use them.
	cursor.DisableMarkAccessed()
	for ; startOffset < endOffset; startOffset += ps {
		// Look the page up if present, allocating nothing.
		page := cursor.MaybePage(false)
		if page == nil {
			continue
		}
		// Only a page of the root, which has the page source, and only a
		// Clean one. The always_need hint is sticky and is not cleared.
		owner := page.object
		assert(owner != nil, "the page has an owner")
		if owner.pageSource != nil && page.dirtyState == Clean {
			c.node.queues.MoveToReclaimDontNeed(page)
		}
	}
	return nil
}

// ProtectRangeFromReclamation looks up every page of r, reading absent ones,
// which marks them accessed, and sets their always_need hint if setAlwaysNeed.
// With ignoreErrors a page that fails is skipped; otherwise the first error
// ends it.
func (c *CowPages) ProtectRangeFromReclamation(ctx context.Context, r CowRange, setAlwaysNeed, ignoreErrors bool) error {
	ps := c.pageSize()
	if !c.canRootSourceEvict() {
		return nil
	}
	c.lock.Lock()
	if !r.IsBoundedBy(c.size) {
		c.lock.Unlock()
		return ErrOutOfRange
	}
	if r.IsEmpty() {
		c.lock.Unlock()
		return nil
	}
	c.lock.Unlock()
	r = c.expandTillPageAligned(r)
	pageRequest := NewMultiPageRequest()
	for !r.IsEmpty() {
		status := func() error {
			deferred := NewDeferredOps(c)
			defer deferred.Finish()
			c.lock.Lock()
			defer c.lock.Unlock()
			// The object may have shrunk while the lock was dropped.
			if r.Offset >= c.size {
				r = CowRange{}
				return nil
			}
			if !r.IsBoundedBy(c.size) {
				r = r.WithLength(c.size - r.Offset)
			}
			cursor, err := c.GetLookupCursorLocked(r)
			if err != nil {
				return err
			}
			defer cursor.Release()
			for ; !r.IsEmpty(); r = r.TrimmedFromStart(ps) {
				// Fault the page in from the parent if need be, without
				// allocating one here.
				result, err := cursor.RequirePage(ctx, false, r.Len/ps, deferred, pageRequest)
				if err != nil {
					return err
				}
				page := result.Page
				// Only a page of the root, which has the page source; the
				// zero page has no owner.
				owner := page.object
				if owner == nil || owner.pageSource == nil {
					continue
				}
				// Zircon replaces a loaned page here; loaned pages are not
				// ported.
				if setAlwaysNeed {
					// The lookup marked it accessed already.
					page.alwaysNeed = true
				}
			}
			return nil
		}()
		if status == nil {
			continue
		}
		if status == ErrShouldWait {
			status = pageRequest.Wait(ctx)
			if status == nil {
				// The page is present now: look again.
				continue
			}
		}
		if !ignoreErrors {
			return status
		}
		// Skip the offset that failed.
		pageRequest.CancelRequests()
		r = r.TrimmedFromStart(ps)
	}
	return nil
}

// DecompressInRange decompresses every reference the object owns in r,
// committing nothing else.
func (c *CowPages) DecompressInRange(ctx context.Context, r CowRange) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if r.IsEmpty() {
		return nil
	}
	curOffset := c.roundDown(r.Offset)
	endOffset := c.roundUp(r.End())
	// Zircon goes again after waiting for the pmm; a Go allocation succeeds
	// or fails at once.
	return c.pageList.ForEveryPageInRangeMutable(func(p PageOrMarkerRef[VmPage], offset uint64) error {
		if !p.Get().IsReference() {
			return nil
		}
		return c.replaceReferenceWithPageLocked(ctx, p, offset)
	}, curOffset, endOffset)
}
