// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc and vm/include/vm/vm_cow_pages.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import "context"

// Reclamation splits pages a pager backs from anonymous pages, as the pager
// splits named pages from the overlay: a Clean page a pager backs is evicted
// and read again, an anonymous page is compressed. D2 departs from Zircon,
// which never reclaims a Dirty or AwaitingClean page of an object a user
// pager backs: here such a page is compressed, as an anonymous page is, and
// so is a page a checkpoint holds beside the page list (D1). RAM is
// checkpointed only on request, so its dirty set is bounded by what can be
// spilled, not by a writeback (plan: departures). Such a page is compressed
// into the reservation it holds (D5), which becomes its reference.

// referenceDirtyShift is where D2 keeps a spilled page's dirty state in its
// reference's metadata, above the share count Zircon keeps there. Only a
// hidden node shares a page, so the share count of a dirty tracked page is
// 0 and needs none of these bits.
const referenceDirtyShift = 32 - dirtyStateBits

// packReferenceMetadata is the metadata a compressed page carries: its share
// count, and under D2 its dirty state. An untracked page's metadata is its
// share count, as Zircon's is.
func packReferenceMetadata(shareCount uint32, state DirtyState) uint32 {
	assert(shareCount < 1<<referenceDirtyShift, "the share count fits below the dirty state")
	return shareCount | uint32(state)<<referenceDirtyShift
}

// unpackReferenceMetadata is the share count and dirty state a compressed
// page carries.
func unpackReferenceMetadata(metadata uint32) (uint32, DirtyState) {
	return metadata & (1<<referenceDirtyShift - 1), DirtyState(metadata >> referenceDirtyShift)
}

// EvictionAction says whether reclamation follows the always_need hint.
type EvictionAction uint8

const (
	// FollowHint does not evict a page hinted always needed.
	FollowHint EvictionAction = iota
	// IgnoreHint evicts it anyway.
	IgnoreHint
)

// cannotReclaimPageLocked checks that page is still at actual's slot, and
// says why not. Zircon also refuses a pinned page; pinning is not ported.
func (c *CowPages) cannotReclaimPageLocked(page *VmPage, actual *PageOrMarker[VmPage]) ReclaimFailure {
	if actual == nil || !actual.IsPage() || actual.Page() != page {
		return IncorrectPage
	}
	return ReclaimSucceeded
}

// ReclaimRangeForEviction evicts the Clean pages in the isolate queues in
// [offset, offset+len). If none could be evicted, and every failure was a
// page not isolated, the range was accessed: EvictAccessed.
func (c *CowPages) ReclaimRangeForEviction(offset, length uint64, action EvictionAction) (ReclaimSuccess, ReclaimFailure) {
	assert(c.canEvict(), "the object can evict")
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	// Remove the mappings and harvest their accessed bits first.
	c.RangeChangeUpdateLocked(CowRange{offset, length}, UnmapAndHarvest, deferred)
	pq := c.node.queues
	var numFailedQueue uint64
	canReclaimPage := func(page *VmPage) bool {
		assert(page.dirtyState != Untracked, "the page is dirty tracked")
		// Zircon refuses a high priority object; high priority is not
		// ported. Only a Clean page can be evicted; a Dirty one is in the
		// dirty queue already.
		if page.dirtyState != Clean {
			assert(pq.DebugPageIsPagerBackedDirty(page), "a page not Clean is in the dirty queue")
			return false
		}
		// Not one hinted always needed, unless told to ignore the hint. It
		// is moved out of the way of the eviction loop.
		if page.alwaysNeed && action == FollowHint {
			pq.MarkAccessed(page)
			return false
		}
		// Only an isolated page.
		if !pq.IsPageReclaimable(page) {
			numFailedQueue++
			return false
		}
		return true
	}
	remover := pageRemover{freed: deferred.freedList(c), c: c}
	var numEvictedPages, numFailedPages uint64
	_ = c.pageList.RemovePages(func(p *PageOrMarker[VmPage], _ uint64) error {
		if !p.IsPage() {
			return nil
		}
		if !canReclaimPage(p.Page()) {
			numFailedPages++
			return nil
		}
		numEvictedPages++
		remover.pushContent(p)
		return nil
	}, offset, offset+length)
	if numEvictedPages == 0 {
		// Every failure a page not isolated means the range was accessed.
		if numFailedPages == numFailedQueue {
			return ReclaimSuccess{}, EvictAccessed
		}
		return ReclaimSuccess{}, ReclaimOther
	}
	c.decrementPopulated(int64(numEvictedPages))
	c.reclamationEventCount++
	return ReclaimSuccess{Type: ReclaimEvict, NumPages: numEvictedPages}, ReclaimSucceeded
}

// reclaimPageForCompression compresses page at offset, with compressor,
// which was just armed. The object's lock is dropped while the page is
// compressed, with the compressor's temporary reference in its slot. An
// anonymous page is Zircon's case; a page of an object a pager backs that is
// not Clean, or one a checkpoint holds, is D2's.
func (c *CowPages) reclaimPageForCompression(ctx context.Context, page *VmPage, offset uint64, compressor *Compressor) (ReclaimSuccess, ReclaimFailure) {
	assert(compressor != nil, "there is a compressor")
	assert(c.canDecommitZeroPages(), "the object decommits zero pages")
	pq := c.node.queues
	reclaimed := false
	// held says the page is one a checkpoint holds beside the page list
	// (D1), which D2 compresses where it is.
	held := false
	failure := func() ReclaimFailure {
		deferred := NewDeferredOps(c)
		defer deferred.Finish()
		c.lock.Lock()
		defer c.lock.Unlock()
		pageOrMarker, list := c.lookupReclaimableLocked(page, offset)
		if reason := c.cannotReclaimPageLocked(page, pageOrMarker); reason != ReclaimSucceeded {
			return reason
		}
		held = list == c.held
		// Zircon refuses an object mapped uncached or high priority; neither
		// is ported. Unmap the page, so it cannot change, harvesting its
		// accessed bit: an access since means it is not compressed.
		oldQueue := pq.queueOf(page)
		if !held {
			c.RangeChangeUpdateLocked(CowRange{offset, c.pageSize()}, UnmapAndHarvest, deferred)
		}
		if pq.queueOf(page) != oldQueue {
			return CompressAccessed
		}
		// Swap the page for the temporary reference. The compressor tracks
		// the page's metadata while it compresses: its share count, and
		// under D2 its dirty state.
		tempRef := compressor.Start(PageAndMetadata{Page: page, Metadata: packReferenceMetadata(page.shareCount, page.dirtyState)})
		compressPage := PageOrMarkerRef[VmPage]{slot: pageOrMarker}.SwapPageForReference(tempRef)
		assert(compressPage == page, "the slot held the page")
		pq.Remove(page)
		return ReclaimSucceeded
	}()
	if failure != ReclaimSucceeded {
		return ReclaimSuccess{}, failure
	}
	// The page is ours, unchanging, and the object holds the temporary
	// reference: compress with no lock held.
	compressor.Compress(ctx)
	compressionFailed := false
	func() {
		c.lock.Lock()
		defer c.lock.Unlock()
		result := compressor.TakeCompressionResult()
		// The temporary reference is still in the slot, and the slot is
		// ours, or it was replaced and the page is ours.
		list := c.pageList
		if held {
			list = c.held
		}
		slot, inInterval := list.LookupOrAllocate(offset, NoIntervals)
		assert(!inInterval, "the slot is in no interval")
		if slot != nil && slot.IsReference() && compressor.IsTempReference(slot.Reference()) {
			var oldRef ReferenceValue
			switch result.Kind {
			case CompressedToRef:
				// The compressor copied any change to the metadata to the
				// new reference.
				oldRef = PageOrMarkerRef[VmPage]{slot: slot}.SwapReferenceForReference(result.Ref)
				if page.reserved {
					// D5: the page was compressed into its reservation, which
					// the slot holds from here, so the page goes without it.
					assert(page.takeReservation() == result.Ref, "the page was compressed into its reservation")
				}
				c.reclamationEventCount++
				reclaimed = true
			case CompressFailed:
				// Put the page back, with any change to its metadata.
				assert(page == result.Src.Page, "the failure gives the page back")
				page.shareCount, page.dirtyState = unpackReferenceMetadata(result.Src.Metadata)
				oldRef = PageOrMarkerRef[VmPage]{slot: slot}.SwapReferenceForPage(page)
				c.setNotPinnedLocked(page, offset)
				// It will not be tried again.
				pq.CompressFailed(page)
				page = nil
				compressionFailed = true
			default:
				assert(result.Kind == CompressedToZero, "the page was zeros")
				oldRef = slot.ReleaseReference()
				shareCount, state := unpackReferenceMetadata(c.node.compression.GetMetadata(oldRef))
				switch {
				case held:
					// D2: the checkpoint holds zeros here.
					*slot = Marker[VmPage]()
				case state == Dirty || state == AwaitingClean:
					// D2: a dirty page of zeros is a dirty zero interval, as
					// zeroing makes one, in the emptied slot; an AwaitingClean
					// one is AwaitingClean for its page, and the checkpoint
					// holds its zeros, as a writeback begun over an interval
					// records (D1).
					c.decrementPopulated(1)
					var awaitingCleanLen uint64
					if state == AwaitingClean {
						awaitingCleanLen = c.pageSize()
					}
					err := c.pageList.addZeroIntervalInternal(offset, offset+c.pageSize(), IntervalDirty, awaitingCleanLen, true)
					assert(err == nil, "the emptied slot takes the interval")
					if state == AwaitingClean {
						if h := c.held.Lookup(offset); (h == nil || h.IsEmpty()) && !c.held.IsOffsetInZeroInterval(offset) {
							err := c.held.AddZeroInterval(offset, offset+c.pageSize(), IntervalDirty)
							assert(err == nil, "the checkpoint holds nothing else at the offset")
						}
					}
				default:
					c.decrementPopulated(1)
					// An empty slot is zero where nothing above shows
					// through and no page source supplies it; otherwise a
					// marker.
					parentHasContent := func(offset uint64) bool {
						content := c.findInitialPageContentLocked(offset)
						defer content.owner.release()
						return content.cursor.Current() != nil
					}
					if c.nodeHasParentContentMarkers() || (!c.rootHasPageSource() && !parentHasContent(offset)) {
						assert(slot.IsEmpty(), "the slot is empty")
						c.pageList.ReturnEmptySlot(offset)
					} else {
						*slot = MarkerWithShareCount[VmPage](shareCount)
					}
				}
				c.reclamationEventCount++
				reclaimed = true
			}
			// The temporary reference was replaced: give it back.
			compressor.ReturnTempReference(oldRef)
		} else {
			// The temporary reference is gone, and the object has moved on:
			// free the result and leave.
			if result.Kind == CompressedToRef {
				compressor.Free(result.Ref)
			}
			if slot != nil && slot.IsEmpty() {
				list.ReturnEmptySlot(offset)
			}
		}
	}()
	// The temporary reference is back one way or another.
	compressor.Finalize()
	if page != nil {
		c.freePage(page)
	}
	if compressionFailed {
		return ReclaimSuccess{}, CompressFailedReclaim
	}
	var n uint64
	if reclaimed {
		n = 1
	}
	return ReclaimSuccess{Type: ReclaimCompress, NumPages: n}, ReclaimSucceeded
}

// lookupReclaimableLocked finds page at offset: in the page list, or under
// D1 among what a checkpoint holds beside it. It returns the slot and its
// list, or nil.
func (c *CowPages) lookupReclaimableLocked(page *VmPage, offset uint64) (*PageOrMarker[VmPage], *PageList[VmPage]) {
	if slot := c.pageList.Lookup(offset); slot != nil && slot.IsPage() && slot.Page() == page {
		return slot, c.pageList
	}
	if slot := c.held.Lookup(offset); slot != nil && slot.IsPage() && slot.Page() == page {
		return slot, c.held
	}
	return c.pageList.Lookup(offset), c.pageList
}

// ReclaimPage tries to reclaim page at offset. An object that can evict
// evicts the node's worth of pages around it, unless the page is not Clean,
// when with a compressor it is compressed (D2). An anonymous object
// compresses with compressor, which must have just been armed. On a failure
// the page is touched, so it stops being a candidate.
func (c *CowPages) ReclaimPage(ctx context.Context, page *VmPage, offset uint64, action EvictionAction, compressor *Compressor) (ReclaimSuccess, ReclaimFailure) {
	if c.canEvict() {
		// D2: a page not Clean, or one a checkpoint holds, is spilled.
		if compressor != nil && c.pageIsDirtyForReclaim(page, offset) {
			return c.reclaimPageForCompression(ctx, page, offset, compressor)
		}
		// Evict in node aligned batches.
		evictionLength := uint64(NodePages) * c.pageSize()
		offset = offset &^ (evictionLength - 1)
		return c.ReclaimRangeForEviction(offset, evictionLength, action)
	}
	if compressor != nil && c.pageSource == nil {
		return c.reclaimPageForCompression(ctx, page, offset, compressor)
	}
	// Zircon discards a discardable object here; discardable objects are
	// not ported. No other way: touch the page so it stops being a
	// candidate, if it is still the object's.
	c.lock.Lock()
	defer c.lock.Unlock()
	p := c.pageList.Lookup(offset)
	if p == nil || !p.IsPage() || p.Page() != page {
		return ReclaimSuccess{}, IncorrectPage
	}
	c.node.queues.MarkAccessed(page)
	return ReclaimSuccess{}, ReclaimOther
}

// pageIsDirtyForReclaim reports whether page at offset is one D2 spills: a
// page not Clean in the page list, or one a checkpoint holds.
func (c *CowPages) pageIsDirtyForReclaim(page *VmPage, offset uint64) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	slot, list := c.lookupReclaimableLocked(page, offset)
	if slot == nil || !slot.IsPage() || slot.Page() != page {
		return false
	}
	return list == c.held || page.dirtyState == Dirty || page.dirtyState == AwaitingClean
}

// DedupZeroPage replaces page at offset with zeros if it holds only zeros.
// It fails for a page not the object's at offset, and for a page not Clean
// of an object that tracks dirty pages.
func (c *CowPages) DedupZeroPage(page *VmPage, offset uint64) bool {
	ps := c.pageSize()
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	// Zircon refuses a high priority object and one its VmObjectPaged
	// cannot dedup, which is one mapped uncached; neither is ported.
	pageOrMarker := c.pageList.LookupMutable(offset)
	if !pageOrMarker.Valid() || !pageOrMarker.Get().IsPage() || pageOrMarker.Get().Page() != page ||
		(page.dirtyState != Untracked && page.dirtyState != Clean) {
		return false
	}
	// Most pages are not zero: check with write access still granted first.
	if !isZeroPage(page) {
		return false
	}
	c.RangeChangeUpdateLocked(CowRange{offset, ps}, RemoveWrite, nil)
	// Clones map nothing writable, so need no update.
	if !isZeroPage(page) {
		return false
	}
	var oldPage PageOrMarker[VmPage]
	if c.nodeHasParentContentMarkers() {
		c.RangeChangeUpdateLocked(CowRange{offset, ps}, Unmap, deferred)
		oldPage = c.pageList.RemoveContent(offset)
		c.decrementPopulated(1)
	} else {
		t, err := c.beginAddPageWithSlotLocked(offset, pageOrMarker, OverwritePageOrRef)
		assert(err == nil, "the page's slot takes a marker")
		shareCount := page.shareCount
		oldPage = c.completeAddPageLocked(&t, MarkerWithShareCount[VmPage](shareCount), deferred)
	}
	assert(oldPage.IsPage(), "a page was replaced")
	c.removePageLocked(oldPage.ReleasePage(), deferred)
	c.reclamationEventCount++
	return true
}
