// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/vm_cow_pages.cc and vm/include/vm/vm_cow_pages.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"

	"github.com/semistrict/sproutfs/platform/sim"
)

// The dirty states, the writeback and the zero intervals of an object a user
// pager backs, with the plan's departures:
//
//   - D1. A store into an AwaitingClean page does not make it Dirty again in
//     place, as Zircon's does: the page stays with the checkpoint, unchanged,
//     in the list of what the checkpoint holds beside the page list, and the
//     store gets a Dirty copy in the page list. A checkpoint must hold exactly
//     the bytes of its pause, because a fork's children and a restore read it
//     as one point in time. The zeros of an AwaitingClean zero interval are
//     recorded the same way when the writeback begins, so a store that splits
//     the interval leaves the checkpoint its zeros.
//   - D3. The pause issues the range protection, WritebackProtect, and the
//     walk behind it makes the pages AwaitingClean, WritebackBegin, with the
//     region held so no store lands between them (the caller's part).
//   - D4. A checkpoint that does not land gives its pages back:
//     WritebackAbandon makes them Dirty again and drops what the checkpoint
//     held beside them. Zircon has no abandon, and leaves them AwaitingClean
//     for the next writeback to take.
//
// The guards below put Zircon's rules back, for the tests that must catch
// them (scripts/mutation/guards.json).
const (
	// bugDirtyAwaitingCleanInPlace is Zircon's rule against D1: a store makes
	// an AwaitingClean page Dirty in place.
	bugDirtyAwaitingCleanInPlace = "zircon-dirty-awaiting-clean-in-place"
	// bugAbandonLeavesAwaitingClean is Zircon's rule against D4: an
	// abandoned writeback leaves its pages AwaitingClean.
	bugAbandonLeavesAwaitingClean = "zircon-abandon-leaves-awaiting-clean"
)

// updateDirtyStateLocked sets a page's dirty state and moves it to the queue
// the state calls for. isPendingAdd says the page is about to be added, so
// the queue is set when it is. The transitions are checked here, in one
// place.
func (c *CowPages) updateDirtyStateLocked(page *VmPage, offset uint64, state DirtyState, isPendingAdd bool) {
	assert(page != nil, "there is a page")
	assert(c.pageSourceType() == UserPager, "the object tracks dirty pages")
	assert(isPendingAdd || page.object == c, "the page is the object's")
	assert(isPendingAdd || page.pageOffset == offset, "the page is at the offset")
	updatePageQueues := false
	switch state {
	case Clean:
		// Outside an add, only an AwaitingClean page becomes Clean.
		assert(isPendingAdd || page.dirtyState == AwaitingClean, "only an AwaitingClean page is cleaned")
		updatePageQueues = !isPendingAdd
	case Dirty:
		// Outside an add, only a Clean page becomes Dirty. Zircon also makes
		// an AwaitingClean page Dirty in place; D1 gives the store a copy
		// instead (splitAwaitingCleanLocked), and D4 makes an abandoned
		// page Dirty through abandonAwaitingCleanLocked.
		assert(isPendingAdd || page.dirtyState == Clean, "only a Clean page becomes Dirty")
		// An identity root's pages never change.
		assert(!c.isIdentityRoot(), "an identity root's page is not dirtied")
		updatePageQueues = !isPendingAdd
	case AwaitingClean:
		// A new page does not start AwaitingClean, and only a Dirty page
		// becomes AwaitingClean. Zircon also asserts it is not pinned.
		assert(!isPendingAdd, "a new page does not start AwaitingClean")
		assert(page.dirtyState == Dirty, "only a Dirty page begins a writeback")
		// The page stays in the dirty queue until its writeback ends.
		assert(c.node.queues.DebugPageIsPagerBackedDirty(page), "the page is in the dirty queue")
	default:
		panic("zirconvm: not a dirty tracked state")
	}
	page.dirtyState = state & dirtyStateMask
	if updatePageQueues {
		// Clean goes to the reclaim queues to age; Dirty to the dirty queue,
		// which does not age.
		c.moveToNotPinnedLocked(page, offset)
	}
}

// splitAwaitingCleanLocked gives a store into the AwaitingClean page in slot
// a Dirty copy of it, and moves the AwaitingClean page to what the
// checkpoint holds (D1). The page keeps its place in the dirty queue, under
// the same offset. Mappings of the offset, which map the checkpoint's page
// read-only, are revoked so the next fault maps the copy. The copy is
// returned.
func (c *CowPages) splitAwaitingCleanLocked(slot PageOrMarkerRef[VmPage], offset uint64,
	deferred *DeferredOps) (*VmPage, error) {
	held := slot.Get().Page()
	assert(held.dirtyState == AwaitingClean, "the page is AwaitingClean")
	copyPage, err := c.allocateCopyPage(held, nil)
	if err != nil {
		return nil, err
	}
	c.updateDirtyStateLocked(copyPage, offset, Dirty, true)
	old := slot.SwapContent(Page(copyPage))
	assert(old.Page() == held, "the slot held the AwaitingClean page")
	c.setNotPinnedLocked(copyPage, offset)
	heldSlot, inInterval := c.held.LookupOrAllocate(offset, CheckForInterval)
	assert(!inInterval && heldSlot != nil && heldSlot.IsEmpty(), "the checkpoint holds nothing else at the offset")
	heldSlot.Set(Page(held))
	c.RangeChangeUpdateLocked(CowRange{offset, c.pageSize()}, Unmap, deferred)
	return copyPage, nil
}

// dirtyForStoreLocked makes the page in slot Dirty for a store: a Clean page
// in place, an AwaitingClean one by D1's split. It returns the page now in
// the slot.
func (c *CowPages) dirtyForStoreLocked(ctx context.Context, slot PageOrMarkerRef[VmPage], offset uint64,
	deferred *DeferredOps) (*VmPage, error) {
	page := slot.Get().Page()
	switch page.dirtyState {
	case Dirty:
		return page, nil
	case Clean:
		c.updateDirtyStateLocked(page, offset, Dirty, false)
		return page, nil
	}
	assert(page.dirtyState == AwaitingClean, "the page is AwaitingClean")
	if sim.Bug(ctx, bugDirtyAwaitingCleanInPlace) {
		// Zircon's rule: Dirty in place, in the page the checkpoint holds.
		// The page is already in the dirty queue.
		page.dirtyState = Dirty
		return page, nil
	}
	return c.splitAwaitingCleanLocked(slot, offset, deferred)
}

// prepareForWriteLocked makes a run of pages from r's start ready to write,
// and returns its length. If the source does not trap dirty transitions,
// the run of committed pages from the start is made Dirty. Otherwise the
// run already Dirty is returned, or a DIRTY request is made for the run that
// is not, returning ErrShouldWait with pageRequest to wait on.
func (c *CowPages) prepareForWriteLocked(ctx context.Context, r CowRange, pageRequest *PageRequest,
	deferred *DeferredOps) (uint64, error) {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(r.IsBoundedBy(c.size), "the range is in the object")
	assert(c.pageSource != nil, "the object has a page source")
	assert(c.pageSourceType() == UserPager, "a user pager backs the object")
	var dirtyLen uint64
	start := r.Offset
	end := r.End()
	if !c.pageSource.ShouldTrapDirtyTransitions() {
		// Mark Dirty the first run of committed pages; absent pages, markers
		// and intervals need a lookup first, which can fail, so they end the
		// run.
		var storeErr error
		err := c.pageList.ForEveryPageAndGapInRangeMutable(func(p PageOrMarkerRef[VmPage], off uint64) error {
			slot := p.Get()
			// Under D2 a reference can be in an object a pager backs; it
			// needs decompressing, which the lookup does, so it ends the run
			// as a marker does.
			if slot.IsMarker() || slot.IsIntervalZero() || slot.IsReference() {
				return ErrStop
			}
			assert(slot.IsPage(), "the slot holds a page")
			page := slot.Page()
			assert(page.dirtyState != Untracked, "the page is dirty tracked")
			assert(page.object == c && page.pageOffset == off, "the page is the object's")
			// Zircon stops at a loaned page; loaned pages are not ported.
			if _, err := c.dirtyForStoreLocked(ctx, p, off, deferred); err != nil {
				storeErr = err
				return ErrStop
			}
			assert(start+dirtyLen == off, "the run is contiguous")
			dirtyLen += ps
			return nil
		}, func(_, _ uint64) error {
			// A gap ends the run.
			return ErrStop
		}, start, end)
		assert(err == nil, "the walk does not fail")
		if storeErr != nil && dirtyLen == 0 {
			return 0, storeErr
		}
		return dirtyLen, nil
	}

	// Otherwise make a DIRTY request for the pages that must become Dirty: a
	// run of committed pages and markers not Dirty (Clean and AwaitingClean
	// alike, as the pager may need to reserve space again), and zero
	// intervals, which the kernel supplies and the pager has not seen.
	var pagesToDirtyLen uint64
	accumulateDirtyPages := func(dirtyStart, dirtyEnd uint64) error {
		if pagesToDirtyLen > 0 {
			return ErrStop
		}
		if start+dirtyLen == dirtyStart {
			dirtyLen += dirtyEnd - dirtyStart
			return nil
		}
		return ErrStop
	}
	accumulatePagesToDirty := func(toDirtyStart, toDirtyEnd uint64) error {
		if dirtyLen > 0 {
			return ErrStop
		}
		if start+pagesToDirtyLen == toDirtyStart {
			pagesToDirtyLen += toDirtyEnd - toDirtyStart
			return nil
		}
		return ErrStop
	}
	intervalStartOff := start
	unmatchedIntervalStart := false
	foundPageOrGap := false
	err := c.pageList.ForEveryPageAndGapInRange(func(p *PageOrMarker[VmPage], off uint64) error {
		foundPageOrGap = true
		if p.IsPage() {
			page := p.Page()
			assert(page.dirtyState != Untracked, "the page is dirty tracked")
			if page.dirtyState == Dirty {
				return accumulateDirtyPages(off, off+ps)
			}
			// A Clean page is marked accessed, to protect it from eviction
			// until the pager answers.
			if page.dirtyState == Clean {
				c.node.queues.MarkAccessed(page)
			}
		} else if p.IsIntervalZero() {
			if p.IsIntervalStart() || p.IsIntervalSlot() {
				unmatchedIntervalStart = true
				intervalStartOff = off
			}
			if p.IsIntervalEnd() || p.IsIntervalSlot() {
				unmatchedIntervalStart = false
				// An interval needs committing whatever its dirty state.
				return accumulatePagesToDirty(intervalStartOff, off+ps)
			}
			return nil
		} else if p.IsReference() {
			// D2: a spilled page carries its dirty state.
			if _, state := unpackReferenceMetadata(c.node.compression.GetMetadata(p.Reference())); state == Dirty {
				return accumulateDirtyPages(off, off+ps)
			}
			return accumulatePagesToDirty(off, off+ps)
		}
		assert(!p.IsParentContent(), "no parent content marker is used")
		// A marker, a clean zero page, or a page not Dirty.
		assert(p.IsMarker() || p.Page().dirtyState != Dirty, "the content is not Dirty")
		return accumulatePagesToDirty(off, off+ps)
	}, func(_, _ uint64) error {
		foundPageOrGap = true
		// A gap ends the walk.
		return ErrStop
	}, start, end)
	assert(err == nil, "the walk does not fail")
	// The last interval, if the walk ended in one.
	if unmatchedIntervalStart {
		_ = accumulatePagesToDirty(intervalStartOff, end)
	}
	// A range wholly inside an interval finds no page or gap, and wholly
	// needs a DIRTY request.
	if !foundPageOrGap {
		assert(c.pageList.IsOffsetInZeroInterval(start), "the range is in a zero interval")
		assert(c.pageList.IsOffsetInZeroInterval(end-ps), "the range is in a zero interval")
		assert(dirtyLen == 0 && pagesToDirtyLen == 0, "nothing was accumulated")
		pagesToDirtyLen = end - start
	}
	assert(dirtyLen == 0 || pagesToDirtyLen == 0, "pages are dirty or to dirty, not both")
	assert(start+dirtyLen <= end, "the dirty run is in range")
	assert(pagesToDirtyLen == 0 || start+pagesToDirtyLen <= end, "the run to dirty is in range")
	if pagesToDirtyLen == 0 {
		return dirtyLen, nil
	}
	// A run needs a DIRTY request. Later runs are asked for when the waiter
	// looks again.
	err = c.pageSource.RequestDirtyTransition(pageRequest, start, pagesToDirtyLen)
	// A page source never succeeds synchronously.
	assert(err != nil, "the dirty request does not succeed synchronously")
	return dirtyLen, err
}

// DirtyPages makes every page in r Dirty, as the pager's answer to a DIRTY
// request: markers and zero intervals become new zero pages first. It is
// all or nothing, since the pager may be reserving space by it.
func (c *CowPages) DirtyPages(ctx context.Context, r CowRange, allocList *[]*VmPage) error {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource != nil, "the object has a page source")
	if !c.pageSource.ShouldTrapDirtyTransitions() {
		return ErrNotSupported
	}
	assert(c.pageSourceType() == UserPager, "a user pager backs the object")
	start := r.Offset
	end := r.End()
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	if start > c.size {
		return ErrOutOfRange
	}
	if end < start {
		return ErrOutOfRange
	}
	// The pager answered a range of DIRTY requests, so resolve them even on
	// failure: it cannot tell which part failed.
	invalidateOnError := true
	defer func() {
		if invalidateOnError {
			assert(c.size >= start, "the object did not shrink below the range")
			c.invalidateDirtyRequestsLocked(start, min(c.size-start, r.Len))
		}
	}()
	if end > c.size {
		return ErrOutOfRange
	}
	if c.pageSource.IsDetached() {
		return ErrBadState
	}
	// Markers and zero intervals need zero pages forked to be dirtied. Count
	// them.
	var zeroPagesCount uint64
	intervalStart := start
	unmatchedIntervalStart := false
	foundPageOrGap := false
	err := c.pageList.ForEveryPageAndGapInRange(func(p *PageOrMarker[VmPage], off uint64) error {
		foundPageOrGap = true
		if p.IsMarker() {
			zeroPagesCount++
			return nil
		}
		if p.IsIntervalZero() {
			switch {
			case p.IsIntervalStart():
				intervalStart = off
				unmatchedIntervalStart = true
			case p.IsIntervalEnd():
				zeroPagesCount += (off - intervalStart + ps) / ps
				unmatchedIntervalStart = false
			default:
				assert(p.IsIntervalSlot(), "the sentinel is a slot")
				zeroPagesCount++
			}
			return nil
		}
		// Under D2 a reference may be here; it is decompressed below.
		assert(p.IsPage() || p.IsReference(), "the slot holds a page or a reference")
		return nil
	}, func(_, _ uint64) error {
		foundPageOrGap = true
		// A page not supplied yet, or evicted, or an interval written back
		// since the request: resolve the request spuriously, and let the
		// waiter look again, which reads first.
		return ErrNotFound
	}, start, end)
	if err != nil {
		return err
	}
	if unmatchedIntervalStart || !foundPageOrGap {
		assert(foundPageOrGap || intervalStart == start, "the range is in one interval")
		zeroPagesCount += (end - intervalStart) / ps
	}
	if zeroPagesCount > 0 {
		// Allocate the pages up front, keeping any from an earlier call.
		have := uint64(len(*allocList))
		if zeroPagesCount > have {
			zeroPagesCount -= have
		} else {
			zeroPagesCount = 0
		}
		for ; zeroPagesCount > 0; zeroPagesCount-- {
			p, err := c.node.pmm.AllocPage()
			if err != nil {
				return err
			}
			*allocList = append(*allocList, p)
		}
		// Populate a slot for every offset of the intervals in range first,
		// so nothing fails once pages start being added.
		nextStart := start
		for nextStart < end {
			found := false
			var intervalFoundStart, intervalFoundEnd uint64
			err := c.pageList.ForEveryPageAndContiguousRunInRange(func(p *PageOrMarker[VmPage], _ uint64) bool {
				return p.IsIntervalStart() || p.IsIntervalEnd()
			}, func(p *PageOrMarker[VmPage], _ uint64) error {
				assert(p.IsIntervalZero(), "the run is an interval")
				return nil
			}, func(runStart, runEnd uint64, isInterval bool) error {
				assert(isInterval, "the run is an interval")
				found, intervalFoundStart, intervalFoundEnd = true, runStart, runEnd
				return ErrStop
			}, nextStart, end)
			assert(err == nil, "the walk does not fail")
			if !found {
				break
			}
			assert(intervalFoundEnd-intervalFoundStart >= ps, "progress is made")
			if err := c.pageList.PopulateSlotsInInterval(intervalFoundStart, intervalFoundEnd); err != nil {
				assert(err == ErrNoMemory, "only an allocation fails")
				// Give back the slots populated so far.
				for off := start; off < intervalFoundStart; off += ps {
					if slot := c.pageList.Lookup(off); slot != nil && slot.IsIntervalSlot() {
						c.pageList.ReturnIntervalSlot(off)
					}
				}
				return err
			}
			nextStart = intervalFoundEnd
		}
		// From here on nothing fails, so the range is dirtied whole. Zero
		// pages go in place of the markers and interval slots, Clean like a
		// supplied page, to be dirtied with the rest below.
		_ = c.pageList.ForEveryPageInRange(func(p *PageOrMarker[VmPage], off uint64) error {
			if p.IsMarker() || p.IsIntervalSlot() {
				assert(len(*allocList) > 0, "a page was allocated for the slot")
				page := (*allocList)[0]
				*allocList = (*allocList)[1:]
				_, err := c.AddNewPageLocked(off, page, OverwriteZeroMarkerOrInterval, true, deferred)
				assert(err == nil, "a zero slot is replaced")
			}
			return nil
		}, start, end)
	}
	// Under D2 a spilled page not Dirty is decompressed, to be dirtied with
	// the rest. Decompression allocates, which can fail; then the range is
	// left for the pager to ask again.
	var decompressErr error
	_ = c.pageList.ForEveryPageInRangeMutable(func(p PageOrMarkerRef[VmPage], off uint64) error {
		if !p.Get().IsReference() {
			return nil
		}
		if _, state := unpackReferenceMetadata(c.node.compression.GetMetadata(p.Get().Reference())); state == Dirty {
			return nil
		}
		if err := c.replaceReferenceWithPageLocked(p, off); err != nil {
			decompressErr = err
			return ErrStop
		}
		return nil
	}, start, end)
	if decompressErr != nil {
		return decompressErr
	}
	// Dirty every page not Dirty, and resolve the requests of each run.
	var storeErr error
	err = c.pageList.ForEveryPageAndContiguousRunInRange(func(p *PageOrMarker[VmPage], _ uint64) bool {
		if p.IsPage() {
			page := p.Page()
			assert(page.dirtyState != Untracked, "the page is dirty tracked")
			return page.dirtyState != Dirty
		}
		return false
	}, func(p *PageOrMarker[VmPage], off uint64) error {
		assert(p.IsPage(), "the slot holds a page")
		if _, err := c.dirtyForStoreLocked(ctx, PageOrMarkerRef[VmPage]{slot: p}, off, deferred); err != nil {
			storeErr = err
			return ErrStop
		}
		return nil
	}, func(runStart, runEnd uint64, _ bool) error {
		c.pageSource.OnPagesDirtied(runStart, runEnd-runStart)
		return nil
	}, start, end)
	assert(err == nil || err == ErrStop, "the walk does not fail")
	if storeErr != nil {
		// Only D1's copy can fail, for want of a page.
		return storeErr
	}
	invalidateOnError = false
	return nil
}

// EnumerateDirtyRangesLocked calls fn on every run of pages in r not Clean,
// AwaitingClean included, and on every dirty zero interval as a zero range.
func (c *CowPages) EnumerateDirtyRangesLocked(r CowRange, fn func(offset, length uint64, isZero bool) error) error {
	if c.pageSourceType() != UserPager {
		return ErrNotSupported
	}
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	start := c.roundDown(r.Offset)
	end := c.roundUp(r.End())
	return c.pageList.ForEveryPageAndContiguousRunInRange(func(p *PageOrMarker[VmPage], _ uint64) bool {
		if p.IsPage() {
			page := p.Page()
			assert(page.dirtyState != Untracked, "the page is dirty tracked")
			return page.dirtyState != Clean
		}
		if p.IsIntervalZero() {
			// Clean intervals are not supported.
			assert(!p.IsZeroIntervalClean(), "no interval is Clean")
			return p.IsZeroIntervalDirty()
		}
		if p.IsReference() {
			// D2: a spilled page carries its dirty state.
			_, state := unpackReferenceMetadata(c.node.compression.GetMetadata(p.Reference()))
			return state != Clean
		}
		assert(p.IsMarker(), "anything else is a marker")
		return false
	}, func(p *PageOrMarker[VmPage], off uint64) error {
		if p.IsPage() {
			assert(p.Page().dirtyState != Clean, "the page is not Clean")
			assert(p.Page().pageOffset == off, "the page is at its offset")
		} else if p.IsIntervalZero() {
			assert(p.IsZeroIntervalDirty(), "the interval is Dirty")
		}
		return nil
	}, func(runStart, runEnd uint64, isInterval bool) error {
		return fn(runStart, runEnd-runStart, isInterval)
	}, start, end)
}

// WritebackProtectLocked takes write access away from every mapping of r,
// so a store faults and is seen. It is the range protection D3 issues in the
// pause; the walk behind it is WritebackBeginLocked.
func (c *CowPages) WritebackProtectLocked(r CowRange) error {
	assert(c.isPageAligned(r), "the range is page aligned")
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if c.pageSourceType() != UserPager {
		return ErrNotSupported
	}
	c.RangeChangeUpdateLocked(r, RemoveWrite, nil)
	return nil
}

// WritebackBeginLocked makes the Dirty pages and dirty zero intervals in r
// AwaitingClean, and takes write access away from r's mappings. With
// isZeroRange only intervals are taken: the pager means to write zeros, and
// a page dirtied meanwhile must not be cleaned by it. What a checkpoint held
// beside the page list in r from an earlier writeback is dropped: the range
// is taken again (D1).
func (c *CowPages) WritebackBeginLocked(r CowRange, isZeroRange bool) error {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource != nil, "the object has a page source")
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if c.pageSourceType() != UserPager {
		return ErrNotSupported
	}
	start := r.Offset
	end := r.End()
	// D1: a range taken again holds what it holds now.
	freed := new(scopedPageFreedList)
	c.freeHeldLocked(start, end-start, freed)
	defer freed.freePages(c)
	// D1: the zero ranges the checkpoint holds, recorded once the walk is
	// done, since adding to the held list does not change the page list.
	type zeroRange struct{ start, end uint64 }
	var heldZeros []zeroRange
	var intervalStart PageOrMarkerRef[VmPage]
	var intervalStartOff uint64
	err := c.pageList.ForEveryPageInRangeMutable(func(p PageOrMarkerRef[VmPage], off uint64) error {
		slot := p.Get()
		// Zircon leaves a pinned page Dirty, as DMA may still write it;
		// pinning is not ported. A zero range leaves committed pages Dirty.
		if (slot.IsPage() || slot.IsReference()) && isZeroRange {
			return nil
		}
		if slot.IsPage() && slot.Page().dirtyState == Dirty {
			c.updateDirtyStateLocked(slot.Page(), off, AwaitingClean, false)
			return nil
		}
		if slot.IsReference() {
			// D2: a spilled Dirty page begins its writeback as a page does.
			compression := c.node.compression
			shareCount, state := unpackReferenceMetadata(compression.GetMetadata(slot.Reference()))
			if state == Dirty {
				compression.SetMetadata(slot.Reference(), packReferenceMetadata(shareCount, AwaitingClean))
			}
			return nil
		}
		if slot.IsIntervalZero() {
			if !slot.IsZeroIntervalDirty() {
				// The only other state supported is Untracked.
				assert(slot.IsZeroIntervalUntracked(), "the interval is Untracked")
				return nil
			}
			if slot.IsIntervalStart() || slot.IsIntervalSlot() {
				// The interval transitions once its end is found.
				assert(!intervalStart.Valid(), "no interval is open")
				intervalStart = p
				intervalStartOff = off
			}
			if slot.IsIntervalEnd() || slot.IsIntervalSlot() {
				// The whole interval is AwaitingClean: its start's
				// AwaitingClean length covers it. Zircon leaves alone an
				// interval the writeback began partway into.
				if intervalStart.Valid() {
					oldLen := intervalStart.Get().GetZeroIntervalAwaitingCleanLength()
					intervalStart.SetZeroIntervalAwaitingCleanLength(max(off-intervalStartOff+ps, oldLen))
					heldZeros = append(heldZeros, zeroRange{intervalStartOff, off + ps})
				}
				intervalStart = PageOrMarkerRef[VmPage]{}
			}
			return nil
		}
		// A marker, already clean, or a page not Dirty.
		assert(slot.IsMarker() || slot.Page().dirtyState != Dirty, "the content is not Dirty")
		return nil
	}, start, end)
	assert(err == nil, "the walk does not fail")
	// The last interval, partly in range.
	if intervalStart.Valid() {
		assert(intervalStart.Get().IsIntervalStart(), "the open interval has a start")
		oldLen := intervalStart.Get().GetZeroIntervalAwaitingCleanLength()
		intervalStart.SetZeroIntervalAwaitingCleanLength(max(end-intervalStartOff, oldLen))
		heldZeros = append(heldZeros, zeroRange{intervalStartOff, end})
	}
	for _, z := range heldZeros {
		err := c.held.AddZeroInterval(z.start, z.end, IntervalDirty)
		assert(err == nil, "the checkpoint holds nothing else in the interval")
	}
	// Make the range read-only, so the next store faults and is seen. Pages
	// AwaitingClean or Clean are read-only already. Clones need no update,
	// as they map nothing writable.
	c.RangeChangeUpdateLocked(CowRange{start, end - start}, RemoveWrite, nil)
	return nil
}

// WritebackEndLocked makes the AwaitingClean pages in r Clean, and removes
// the zero intervals cleaned, or clips the part cleaned. What the checkpoint
// held beside the page list in r is freed: it landed (D1).
func (c *CowPages) WritebackEndLocked(r CowRange) error {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource != nil, "the object has a page source")
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if c.pageSourceType() != UserPager {
		return ErrNotSupported
	}
	start := r.Offset
	end := r.End()
	// The end up to which zero intervals may be cleaned, a running maximum
	// across intervals: a split or a clip leaves a start's AwaitingClean
	// length longer than its interval, and it applies to any zero interval
	// in that range, as the pager means to write it all as zeros.
	var intervalAwaitingCleanEnd uint64
	removeStart := start
	for removeStart < end {
		intervalOffset := ^uint64(0)
		_ = c.pageList.RemovePages(func(p *PageOrMarker[VmPage], off uint64) error {
			if p.IsPage() && p.Page().dirtyState == AwaitingClean {
				c.updateDirtyStateLocked(p.Page(), off, Clean, false)
				return nil
			}
			if p.IsReference() {
				// D2: a spilled AwaitingClean page is cleaned as a page is.
				compression := c.node.compression
				shareCount, state := unpackReferenceMetadata(compression.GetMetadata(p.Reference()))
				if state == AwaitingClean {
					compression.SetMetadata(p.Reference(), packReferenceMetadata(shareCount, Clean))
				}
				return nil
			}
			if p.IsIntervalZero() {
				if !p.IsZeroIntervalDirty() {
					assert(p.IsZeroIntervalUntracked(), "the interval is Untracked")
					return nil
				}
				// An end means the range began inside an interval: nothing
				// to do.
				if p.IsIntervalEnd() {
					return nil
				}
				intervalOffset = off
				return ErrStop
			}
			assert(p.IsMarker() || p.Page().dirtyState != AwaitingClean, "the content is not AwaitingClean")
			return nil
		}, removeStart, end)
		if intervalOffset >= end {
			break
		}
		interval := c.pageList.Lookup(intervalOffset)
		assert(interval != nil && interval.IsIntervalZero() && interval.IsZeroIntervalDirty() && !interval.IsIntervalEnd(),
			"a dirty interval starts at the offset")
		// Where the interval ends, and where to go on from.
		intervalEnd := intervalOffset
		if interval.IsIntervalStart() {
			_ = c.pageList.ForEveryPageInRange(func(slot *PageOrMarker[VmPage], offset uint64) error {
				assert(slot.IsIntervalEnd(), "the next slot ends the interval")
				intervalEnd = offset
				return ErrStop
			}, intervalOffset+ps, c.pageList.MaxSize())
			assert(intervalEnd > intervalOffset, "the interval has an end")
		}
		removeStart = intervalEnd + ps
		intervalAwaitingCleanEnd = max(intervalAwaitingCleanEnd,
			intervalOffset+interval.GetZeroIntervalAwaitingCleanLength())
		if interval.IsIntervalSlot() {
			if intervalOffset < intervalAwaitingCleanEnd {
				c.pageList.RemoveContent(intervalOffset)
			}
			continue
		}
		assert(interval.IsIntervalStart(), "the interval starts here")
		if intervalEnd <= end && intervalAwaitingCleanEnd > intervalEnd {
			// The whole interval is cleaned, and in range: remove it.
			c.pageList.RemoveContent(intervalOffset)
			c.pageList.RemoveContent(intervalEnd)
		} else {
			// Clip the part cleaned off its start. Cleaning is best effort:
			// a failure leaves the interval for another writeback.
			cleanLength := min(intervalAwaitingCleanEnd, end) - intervalOffset
			_ = c.pageList.ClipIntervalStart(intervalOffset, cleanLength)
		}
	}
	// D1: the checkpoint landed, so what it held beside the page list goes.
	freed := new(scopedPageFreedList)
	c.freeHeldLocked(start, end-start, freed)
	freed.freePages(c)
	return nil
}

// abandonAwaitingCleanLocked makes an AwaitingClean page of an abandoned
// writeback Dirty again (D4). It stays in the dirty queue.
func (c *CowPages) abandonAwaitingCleanLocked(page *VmPage) {
	assert(page.dirtyState == AwaitingClean, "the page is AwaitingClean")
	assert(c.node.queues.DebugPageIsPagerBackedDirty(page), "the page is in the dirty queue")
	page.dirtyState = Dirty
}

// WritebackAbandonLocked gives back the pages of a writeback in r that will
// not end, because its publication failed or the region was unsealed (D4):
// AwaitingClean pages, spilled pages and zero intervals are Dirty again,
// what the checkpoint held beside the page list goes, and mappings of r are
// revoked. Zircon has no abandon.
func (c *CowPages) WritebackAbandonLocked(ctx context.Context, r CowRange, deferred *DeferredOps) error {
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource != nil, "the object has a page source")
	if !r.IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	if c.pageSourceType() != UserPager {
		return ErrNotSupported
	}
	if sim.Bug(ctx, bugAbandonLeavesAwaitingClean) {
		// Zircon's rule: the pages stay AwaitingClean for the next writeback.
		return nil
	}
	start := r.Offset
	end := r.End()
	_ = c.pageList.ForEveryPageInRangeMutable(func(p PageOrMarkerRef[VmPage], _ uint64) error {
		slot := p.Get()
		switch {
		case slot.IsPage() && slot.Page().dirtyState == AwaitingClean:
			c.abandonAwaitingCleanLocked(slot.Page())
		case slot.IsReference():
			compression := c.node.compression
			shareCount, state := unpackReferenceMetadata(compression.GetMetadata(slot.Reference()))
			if state == AwaitingClean {
				compression.SetMetadata(slot.Reference(), packReferenceMetadata(shareCount, Dirty))
			}
		case slot.IsIntervalZero() && (slot.IsIntervalStart() || slot.IsIntervalSlot()) &&
			slot.IsZeroIntervalDirty() && slot.GetZeroIntervalAwaitingCleanLength() != 0:
			p.SetZeroIntervalAwaitingCleanLength(0)
		}
		return nil
	}, start, end)
	c.freeHeldLocked(start, end-start, deferred.freedList(c))
	c.RangeChangeUpdateLocked(CowRange{start, end - start}, Unmap, deferred)
	return nil
}

// ReadWritebackLocked reads into buf, from offset, what a writeback in
// progress holds: what the checkpoint holds beside the page list, or else
// the AwaitingClean page or zero interval in it. It is what an upload reads,
// which D1 keeps the bytes of the pause. An offset no writeback holds is
// ErrBadState. offset is page rounded and buf whole pages.
func (c *CowPages) ReadWritebackLocked(offset uint64, buf []byte) error {
	ps := c.pageSize()
	assert(c.isPageRounded(offset), "the offset is page rounded")
	assert(c.isPageRounded(uint64(len(buf))), "the buffer is whole pages")
	if !(CowRange{offset, uint64(len(buf))}).IsBoundedBy(c.size) {
		return ErrOutOfRange
	}
	for done := uint64(0); done < uint64(len(buf)); done += ps {
		off := offset + done
		dst := buf[done : done+ps]
		if held := c.held.LookupMutable(off); held.Valid() && !held.Get().IsEmpty() {
			if held.Get().IsReference() {
				// D2: the checkpoint's copy was spilled.
				if err := c.replaceReferenceWithPageLocked(held, off); err != nil {
					return err
				}
			}
			if held.Get().IsPage() {
				copy(dst, held.Get().Page().data)
			} else {
				clear(dst)
			}
			continue
		}
		if c.held.IsOffsetInZeroInterval(off) {
			clear(dst)
			continue
		}
		slot := c.pageList.LookupMutable(off)
		if !slot.Valid() || slot.Get().IsEmpty() {
			return ErrBadState
		}
		if slot.Get().IsReference() {
			_, state := unpackReferenceMetadata(c.node.compression.GetMetadata(slot.Get().Reference()))
			if state != AwaitingClean {
				return ErrBadState
			}
			if err := c.replaceReferenceWithPageLocked(slot, off); err != nil {
				return err
			}
		}
		if !slot.Get().IsPage() || slot.Get().Page().dirtyState != AwaitingClean {
			return ErrBadState
		}
		copy(dst, slot.Get().Page().data)
	}
	return nil
}

// invalidateReadRequestsLocked resolves the READ requests of the gaps in
// [offset, offset+len) spuriously.
func (c *CowPages) invalidateReadRequestsLocked(offset, length uint64) {
	assert(c.isPageRounded(offset) && c.isPageRounded(length), "the range is page rounded")
	assert(c.pageSource != nil, "the object has a page source")
	_ = c.pageList.ForEveryPageAndGapInRange(func(*PageOrMarker[VmPage], uint64) error { return nil },
		func(gapStart, gapEnd uint64) error {
			c.pageSource.OnPagesSupplied(gapStart, gapEnd-gapStart)
			return nil
		}, offset, offset+length)
}

// invalidateDirtyRequestsLocked resolves the DIRTY requests in [offset,
// offset+len) spuriously: of content not Dirty, and of gaps.
func (c *CowPages) invalidateDirtyRequestsLocked(offset, length uint64) {
	assert(c.isPageRounded(offset) && c.isPageRounded(length), "the range is page rounded")
	assert(c.pageSourceType() == UserPager, "a user pager backs the object")
	assert(c.pageSource.ShouldTrapDirtyTransitions(), "the source traps dirty transitions")
	start := offset
	end := offset + length
	err := c.pageList.ForEveryPageAndContiguousRunInRange(func(p *PageOrMarker[VmPage], _ uint64) bool {
		// A marker is a clean zero page and an interval an uncommitted one;
		// either may have a request outstanding.
		if p.IsMarker() || p.IsIntervalZero() {
			return true
		}
		if p.IsReference() {
			_, state := unpackReferenceMetadata(c.node.compression.GetMetadata(p.Reference()))
			return state != Dirty
		}
		assert(!p.IsParentContent(), "no parent content marker is used")
		page := p.Page()
		assert(page.dirtyState != Untracked, "the page is dirty tracked")
		return page.dirtyState != Dirty
	}, func(*PageOrMarker[VmPage], uint64) error { return nil },
		func(runStart, runEnd uint64, _ bool) error {
			c.pageSource.OnPagesDirtied(runStart, runEnd-runStart)
			return nil
		}, start, end)
	assert(err == nil, "the walk does not fail")
	// A gap may have a request outstanding too: the page was evicted, or an
	// interval written back, after the request was made.
	err = c.pageList.ForEveryPageAndGapInRange(func(*PageOrMarker[VmPage], uint64) error { return nil },
		func(gapStart, gapEnd uint64) error {
			c.pageSource.OnPagesDirtied(gapStart, gapEnd-gapStart)
			return nil
		}, start, end)
	assert(err == nil, "the walk does not fail")
}

// zeroPagesDirectUserPagerLocked zeroes r of an object a user pager backs,
// with zero intervals: Dirty if dirtyTrack, Untracked otherwise. It returns
// how much it zeroed, which may be some even on an error.
func (c *CowPages) zeroPagesDirectUserPagerLocked(ctx context.Context, r CowRange, dirtyTrack bool,
	deferred *DeferredOps, pageRequest *MultiPageRequest) (uint64, error) {
	ps := c.pageSize()
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(r.IsBoundedBy(c.size), "the range is in the object")
	assert(c.pageSourceType() == UserPager, "a user pager backs the object")
	start := r.Offset
	end := r.End()
	var processedLen uint64
	if start == end {
		return 0, nil
	}
	// Zircon checks for pinned pages here; pinning is not ported. Unmap
	// everything once up front, on the assumption pages go.
	c.RangeChangeUpdateLocked(r, Unmap, deferred)
	requiredState := IntervalUntracked
	if dirtyTrack {
		requiredState = IntervalDirty
	}
	// Adding an interval can change the page list under a walk, so each
	// walk finds one range to make an interval, stops, adds it, and the next
	// walk goes on past it.
	nextStartOffset := start
	for {
		inInterval := false
		intervalStart := nextStartOffset
		prevStartOffset := nextStartOffset
		var state struct {
			addZeroInterval   bool
			start, end        uint64
			replacePage       bool
			overwriteInterval bool
			freeReference     bool
		}
		err := c.pageList.RemovePagesAndIterateGaps(func(p *PageOrMarker[VmPage], off uint64) error {
			if p.IsPage() || p.IsReference() {
				// A page, or under D2 a spilled page, goes and an interval
				// takes its place. Zircon looks up and zeroes a pinned page
				// instead; pinning is not ported.
				state.addZeroInterval = true
				state.start, state.end = off, off
				state.replacePage = p.IsPage()
				state.freeReference = p.IsReference()
				return ErrStop
			}
			// A marker or an interval holds zeros already, but its dirty
			// state may need changing.
			assert(p.IsMarker() || p.IsIntervalZero(), "the slot holds zeros")
			if p.IsIntervalStart() {
				intervalStart = off
				inInterval = true
				if p.GetZeroIntervalDirtyState() != requiredState {
					// If the end is found, state.end becomes its offset.
					state.addZeroInterval = true
					state.start, state.end = intervalStart, ^uint64(0)
					state.overwriteInterval = true
				}
			} else if p.IsIntervalEnd() {
				if p.GetZeroIntervalDirtyState() != requiredState {
					state.addZeroInterval = true
					if inInterval {
						state.start = intervalStart
					} else {
						state.start = ^uint64(0)
					}
					state.end = off
					state.overwriteInterval = true
					return ErrStop
				}
				processedLen += off + ps - intervalStart
				inInterval = false
			} else {
				// A single interval slot, or a marker. Overwrite a slot in
				// another state, and a marker if not dirty tracking, as a
				// marker is a clean zero page.
				if p.IsMarker() && !dirtyTrack {
					*p = Empty[VmPage]()
				}
				if p.IsEmpty() || (p.IsIntervalSlot() && p.GetZeroIntervalDirtyState() != requiredState) {
					state.addZeroInterval = true
					state.start, state.end = off, off
					state.overwriteInterval = p.IsIntervalSlot()
					return ErrStop
				}
				processedLen += ps
			}
			nextStartOffset = off + ps
			return nil
		}, func(gapStart, gapEnd uint64) error {
			// The gap becomes a zero interval, so resolve its READ requests.
			c.pageSource.OnPagesSupplied(gapStart, gapEnd-gapStart)
			state.addZeroInterval = true
			state.start, state.end = gapStart, gapEnd-ps
			return ErrStop
		}, nextStartOffset, end)
		if err != nil && err != ErrStop {
			return processedLen, err
		}
		if state.addZeroInterval {
			switch {
			case state.replacePage:
				assert(state.start == state.end, "one page is replaced")
				page := c.pageList.ReplacePageWithZeroInterval(state.start, requiredState)
				c.removePageLocked(page, deferred)
				c.decrementPopulated(1)
			case state.freeReference:
				// D2: the reference goes, and an interval takes its slot.
				ref := c.pageList.RemoveContent(state.start)
				c.freeReference(ref.ReleaseReference())
				c.decrementPopulated(1)
				err = c.pageList.AddZeroInterval(state.start, state.start+ps, requiredState)
			case state.overwriteInterval:
				oldStart, oldEnd := state.start, state.end
				if state.start == ^uint64(0) {
					state.start = nextStartOffset
				}
				if state.end == ^uint64(0) {
					state.end = end - ps
				}
				err = c.pageList.OverwriteZeroInterval(oldStart, oldEnd, state.start, state.end, requiredState)
			default:
				err = c.pageList.AddZeroInterval(state.start, state.end+ps, requiredState)
			}
			if err != nil {
				assert(err == ErrNoMemory, "only an allocation fails")
				return processedLen, err
			}
			processedLen += state.end - state.start + ps
			nextStartOffset = state.end + ps
		} else if inInterval || nextStartOffset == prevStartOffset {
			// The last interval, partly in range, or a range wholly in an
			// interval. One in another state is not split just to change
			// its state: the range counts as processed.
			assert(nextStartOffset != prevStartOffset || c.pageList.IsOffsetInZeroInterval(nextStartOffset),
				"a range not advanced is in an interval")
			processedLen += end - intervalStart
			nextStartOffset = end
		}
		assert(nextStartOffset > prevStartOffset, "progress is made")
		if nextStartOffset >= end {
			break
		}
	}
	return processedLen, nil
}

// zeroPagesNoDirectPageSourceLocked zeroes r of an object no page source
// directly backs: a root's pages are freed, and a child's replaced with zero
// markers, as are the gaps where it sees its parent. It returns how much it
// zeroed. Zircon's anonymous trees use parent content markers, where an
// empty slot of a leaf is zero; the port uses none
// (treeHasParentContentMarkers), so a child zeroes as Zircon zeroes a child
// in a tree a pager backs.
func (c *CowPages) zeroPagesNoDirectPageSourceLocked(r CowRange, deferred *DeferredOps) (uint64, error) {
	ps := c.pageSize()
	assert(r.IsBoundedBy(c.size), "the range is in the object")
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSource == nil, "no page source backs the object")
	assert(c.canDecommitZeroPages(), "zero pages can be decommitted")
	start := r.Offset
	end := r.End()
	canSeeParent := func(offset, length uint64) bool {
		if c.parent == nil {
			return false
		}
		return offset < c.parentLimit && offset+length <= c.parentLimit
	}
	// replaceWithMarker puts a marker at offset, freeing what was there.
	replaceWithMarker := func(offset uint64) error {
		assert(c.parent != nil && !c.nodeHasParentContentMarkers(), "a marker hides a parent")
		released, err := c.addPageLocked(offset, Marker[VmPage](), OverwritePageOrRef, nil)
		if err != nil {
			assert(err == ErrNoMemory, "only an allocation fails")
			return err
		}
		if released.IsPage() {
			c.removePageLocked(released.ReleasePage(), deferred)
		} else if released.IsReference() {
			c.freeReference(released.ReleaseReference())
		}
		return nil
	}
	var zeroedLen uint64
	alreadyUnmapped := false
	doUnmap := func() {
		if !alreadyUnmapped {
			alreadyUnmapped = true
			// The whole range left, at once.
			c.RangeChangeUpdateLocked(r.TrimmedFromStart(zeroedLen), Unmap, deferred)
		}
	}
	currentStart := start
	var status error
	for status == nil && currentStart < end {
		var gap struct {
			found      bool
			start, end uint64
		}
		status = c.pageList.RemovePagesAndIterateGaps(func(slot *PageOrMarker[VmPage], offset uint64) error {
			assert(!slot.IsInterval(), "an anonymous object has no intervals")
			if slot.IsMarker() {
				zeroedLen += ps
				return nil
			}
			// Zircon zeroes a pinned page in place; pinning is not ported.
			doUnmap()
			if slot.IsPageOrRef() {
				if c.parent == nil {
					// A root's empty slot is zero, as an anonymous root's is
					// in Zircon, where parent content markers are used: the
					// slot can just be emptied.
					if slot.IsPage() {
						c.removePageLocked(slot.ReleasePage(), deferred)
					} else {
						c.freeReference(slot.ReleaseReference())
					}
					c.decrementPopulated(1)
					zeroedLen += ps
					return nil
				}
				if err := replaceWithMarker(offset); err != nil {
					return err
				}
				zeroedLen += ps
				return nil
			}
			panic("zirconvm: no parent content marker is used")
		}, func(gapStart, gapEnd uint64) error {
			// A gap that sees no parent is zero already. Departure: Zircon
			// asks whether the whole gap sees the parent, so a gap across the
			// parent limit is taken as zero, and the part before the limit
			// keeps showing the parent. It reaches this only in a tree a pager
			// backs; anonymous trees reach it here too, having no parent
			// content markers. Any part seeing the parent sends the gap to the
			// walk below, which looks at each offset.
			if c.parent == nil || gapStart >= c.parentLimit {
				zeroedLen += gapEnd - gapStart
				return nil
			}
			// Each offset of a gap that sees the parent needs looking at,
			// which may change the page list: stop the walk.
			gap.found, gap.start, gap.end = true, gapStart, gapEnd
			return ErrStop
		}, currentStart, end)
		if status == ErrStop {
			status = nil
		}
		if status != nil || !gap.found {
			break
		}
		currentStart = gap.end
		for offset := gap.start; offset < gap.end && status == nil; offset, zeroedLen = offset+ps, zeroedLen+ps {
			if !canSeeParent(offset, ps) {
				continue
			}
			doUnmap()
			t, err := c.beginAddPageLocked(offset, OverwritePageOrRef)
			if err != nil {
				status = err
				break
			}
			// Zircon gives back the share it had of a hidden ancestor's page
			// here; there are none.
			old := c.completeAddPageLocked(&t, Marker[VmPage](), nil)
			assert(old.IsEmpty(), "the gap was empty")
		}
	}
	return zeroedLen, status
}

// ZeroPagesLocked makes r read as zeros, freeing pages where it can. With
// dirtyTrack, zeros in an object a pager backs start Dirty. It returns how
// much it zeroed, which may be some even on an error.
func (c *CowPages) ZeroPagesLocked(ctx context.Context, r CowRange, dirtyTrack bool, deferred *DeferredOps,
	pageRequest *MultiPageRequest) (uint64, error) {
	assert(r.IsBoundedBy(c.size), "the range is in the object")
	assert(c.isPageAligned(r), "the range is page aligned")
	assert(c.pageSourceType() == Anonymous || c.parent == nil, "an object a pager backs has no parent")
	if c.pageSourceType() == UserPager {
		return c.zeroPagesDirectUserPagerLocked(ctx, r, dirtyTrack, deferred, pageRequest)
	}
	assert(c.pageSource == nil, "no page source backs the object")
	return c.zeroPagesNoDirectPageSourceLocked(r, deferred)
}

// DetachSource detaches the page source, so later faults fail, and frees the
// pages that can be read again: everything but pages not Clean, which the
// pager may still write back, and what a checkpoint holds.
func (c *CowPages) DetachSource() {
	deferred := NewDeferredOps(c)
	defer deferred.Finish()
	c.lock.Lock()
	defer c.lock.Unlock()
	assert(c.pageSource != nil, "the object has a page source")
	c.pageSource.Detach()
	assert(c.parent == nil, "the object is a root")
	// Unmap everything at once; only the pager reads it from here on.
	c.RangeChangeUpdateLocked(CowRange{0, c.size}, Unmap, deferred)
	var removed int64
	remover := pageRemover{freed: deferred.freedList(c), c: c}
	_ = c.pageList.RemovePages(func(p *PageOrMarker[VmPage], _ uint64) error {
		// A marker is a clean zero page.
		if p.IsMarker() {
			*p = Empty[VmPage]()
			return nil
		}
		// Intervals are dirty, and stay.
		if p.IsIntervalZero() {
			assert(!p.IsZeroIntervalClean(), "no interval is Clean")
			return nil
		}
		if p.IsReference() {
			// D2: a spilled page not Clean stays for the pager.
			_, state := unpackReferenceMetadata(c.node.compression.GetMetadata(p.Reference()))
			if state != Clean {
				return nil
			}
			removed++
			remover.pushContent(p)
			return nil
		}
		assert(p.IsPage(), "the slot holds a page")
		if p.Page().dirtyState != Untracked && p.Page().dirtyState != Clean {
			return nil
		}
		remover.push(p.ReleasePage())
		removed++
		return nil
	}, 0, c.size)
	c.decrementPopulated(removed)
}
