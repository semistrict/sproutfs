// Copyright 2016 The Fuchsia Authors
// Copyright (c) 2014 Travis Geiselbrecht
// Ported from zircon/kernel/vm/vm_object_paged.cc, vm/include/vm/vm_object_paged.h, vm/vm_object.cc,
// vm/include/vm/vm_object.h and vm/include/vm/fault.h at fuchsia 90e54e09. MIT licence; see LICENSE
// in this directory.

package zirconvm

import (
	"context"
	"sync"
)

// The page fault flags GetPage takes, from fault.h.
const (
	// PfFlagWrite is VMM_PF_FLAG_WRITE: the access writes.
	PfFlagWrite uint = 1 << 0
	// PfFlagHwFault is VMM_PF_FLAG_HW_FAULT: the hardware asks for the page.
	PfFlagHwFault uint = 1 << 5
	// PfFlagSwFault is VMM_PF_FLAG_SW_FAULT: software asks for the page.
	PfFlagSwFault uint = 1 << 6
	// PfFlagFaultMask is either fault.
	PfFlagFaultMask = PfFlagHwFault | PfFlagSwFault
)

// EvictionHint is a hint on how to reclaim a range, VmObject::EvictionHint.
type EvictionHint uint8

const (
	// DontNeed reclaims the range first.
	DontNeed EvictionHint = iota
	// AlwaysNeed protects the range from reclamation.
	AlwaysNeed
)

// Mapping is a mapping of an object, which range changes reach. Zircon's
// VmMapping updates hardware page tables under the object's lock; the
// pager's mappings are a VMM's, reached by commands (plan: The mapping
// protocol), so here a mapping is an interface.
type Mapping interface {
	// RangeChangeUpdate applies op to the mapping's pages of [offset,
	// offset+len) of the object. It is called with the object's lock held.
	RangeChangeUpdate(offset, length uint64, op RangeChangeOp)
}

// ObjectPaged is an object a mapping maps, Zircon's VmObjectPaged: a
// CowPages and the mappings of it. Slices and references are not ported, so
// an object always sees the whole of its CowPages.
type ObjectPaged struct {
	cowPages *CowPages
	// mappings are the object's mappings, Zircon's mapping_list_.
	mappings []Mapping
	// options are the object's options. Only resizable is kept, and resizing
	// is not ported.
	resizable bool
}

// lock is the object's lock, its CowPages'.
func (o *ObjectPaged) lock() *sync.Mutex { return o.cowPages.lock }

// pageSize is the size of the object's pages.
func (o *ObjectPaged) pageSize() uint64 { return o.cowPages.pageSize() }

// maxSize is VmObject::max_size: the page list's MaxSize.
func (o *ObjectPaged) maxSize() uint64 { return o.cowPages.pageList.MaxSize() }

func newObjectPaged(cow *CowPages) *ObjectPaged {
	o := &ObjectPaged{cowPages: cow}
	cow.lock.Lock()
	defer cow.lock.Unlock()
	cow.paged = o
	cow.TransitionToAliveLocked()
	return o
}

// CreateObjectPaged is an anonymous object of size bytes: VmObjectPaged::
// Create.
func CreateObjectPaged(node *Node, size uint64) (*ObjectPaged, error) {
	if size&(node.pageSize-1) != 0 {
		return nil, ErrInvalidArgs
	}
	if size > NewPageList[VmPage](node.pageSize).MaxSize() {
		return nil, ErrOutOfRange
	}
	return newObjectPaged(createCowPages(node, optionsNone, size)), nil
}

// CreateExternal is an object src backs: VmObjectPaged::CreateExternal.
func CreateExternal(node *Node, src *PageSource, size uint64) (*ObjectPaged, error) {
	return createWithSource(node, src, size, optionsNone, nil)
}

// CreateIdentityRoot is an identity root: an object a pager backs whose
// pages are Clean and never change, which regions' lookups fall through to.
// It is ours (plan: Zircon's objects and ours).
func CreateIdentityRoot(node *Node, src *PageSource, size uint64) (*ObjectPaged, error) {
	if !src.Properties().IsUserPager {
		return nil, ErrInvalidArgs
	}
	return createWithSource(node, src, size, optionIdentityRoot, nil)
}

// CreateRegionLayer is a region's layer: an object the pager backs, which
// tracks its pages dirty, and whose offsets it holds no content for read
// the identity root roots names. It is ours (plan: Zircon's objects and
// ours).
func CreateRegionLayer(node *Node, src *PageSource, size uint64, roots RootResolver) (*ObjectPaged, error) {
	if !src.Properties().IsUserPager || roots == nil {
		return nil, ErrInvalidArgs
	}
	return createWithSource(node, src, size, optionsNone, roots)
}

func createWithSource(node *Node, src *PageSource, size uint64, options cowPagesOptions, roots RootResolver) (*ObjectPaged, error) {
	if size&(node.pageSize-1) != 0 {
		return nil, ErrInvalidArgs
	}
	if size > NewPageList[VmPage](node.pageSize).MaxSize() {
		return nil, ErrOutOfRange
	}
	options |= optionPageSourceRoot
	if src.Properties().IsUserPager {
		options |= optionUserPagerBackedRoot
	}
	cow := createExternalCowPages(node, src, options, size)
	cow.roots = roots
	return newObjectPaged(cow), nil
}

// Destroy is the object's destructor, which Go does not have: the CowPages
// is let go, and dies with its parents as far as nothing else reaches them.
func (o *ObjectPaged) Destroy() {
	o.cowPages.lock.Lock()
	o.cowPages.paged = nil
	o.cowPages.lock.Unlock()
	for deferred := o.cowPages; deferred != nil; {
		deferred = deferred.MaybeDeadTransition()
	}
}

// Size is the object's size.
func (o *ObjectPaged) Size() uint64 {
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.size
}

// IsResizable reports whether the object may be resized. Resizing is not
// ported.
func (o *ObjectPaged) IsResizable() bool { return o.resizable }

// DebugGetCowPages is the object's CowPages, for tests.
func (o *ObjectPaged) DebugGetCowPages() *CowPages { return o.cowPages }

// DebugGetPage is the page at offset, or nil.
func (o *ObjectPaged) DebugGetPage(offset uint64) *VmPage { return o.cowPages.DebugGetPage(offset) }

// AddMapping makes m a mapping of the object.
func (o *ObjectPaged) AddMapping(m Mapping) {
	o.lock().Lock()
	defer o.lock().Unlock()
	o.mappings = append(o.mappings, m)
}

// RemoveMapping takes m off the object's mappings.
func (o *ObjectPaged) RemoveMapping(m Mapping) {
	o.lock().Lock()
	defer o.lock().Unlock()
	for i, x := range o.mappings {
		if x == m {
			o.mappings = append(o.mappings[:i], o.mappings[i+1:]...)
			return
		}
	}
	panic("zirconvm: the mapping is not the object's")
}

// rangeChangeUpdateLocked applies op to every mapping of r.
func (o *ObjectPaged) rangeChangeUpdateLocked(r CowRange, op RangeChangeOp) {
	assert(o.cowPages.isPageAligned(r), "the range is page aligned")
	assert(r.Len != 0, "the range is not empty")
	for _, m := range o.mappings {
		m.RangeChangeUpdate(r.Offset, r.Len, op)
	}
}

// RangeChangeUpdate applies op to r of every mapping of the object and its
// clones that see it, as a range change from the object's own changes does.
func (o *ObjectPaged) RangeChangeUpdate(r CowRange, op RangeChangeOp) {
	deferred := NewDeferredOps(o.cowPages)
	defer deferred.Finish()
	o.lock().Lock()
	defer o.lock().Unlock()
	o.cowPages.RangeChangeUpdateLocked(r, op, deferred)
}

// getCowRange translates a range of the object to one of its CowPages, or
// fails if it overflows.
func (o *ObjectPaged) getCowRange(offset, length uint64) (CowRange, bool) {
	r := CowRange{offset, length}
	if r.End() < offset {
		return CowRange{}, false
	}
	return r, true
}

// getCowRangeSizeCheckLocked is getCowRange that also checks the range is in
// the object.
func (o *ObjectPaged) getCowRangeSizeCheckLocked(offset, length uint64) (CowRange, bool) {
	r := CowRange{offset, length}
	if !r.IsBoundedBy(o.cowPages.size) {
		return CowRange{}, false
	}
	return r, true
}

// GetAttributedMemory is the memory attributed to the whole object.
func (o *ObjectPaged) GetAttributedMemory() AttributionCounts {
	return o.GetAttributedMemoryInRange(0, o.Size())
}

// GetAttributedMemoryInRange is the memory attributed to [offset,
// offset+len), trimmed to the object.
func (o *ObjectPaged) GetAttributedMemoryInRange(offset, length uint64) AttributionCounts {
	o.lock().Lock()
	defer o.lock().Unlock()
	size := o.cowPages.size
	if offset >= size {
		return AttributionCounts{}
	}
	length = min(length, size-offset)
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return AttributionCounts{}
	}
	// The counts are of whole pages.
	r = o.cowPages.expandTillPageAligned(r)
	return o.cowPages.GetAttributedMemoryInRangeLocked(r)
}

// ReclamationEventCount counts the object's reclamations.
func (o *ObjectPaged) ReclamationEventCount() uint64 {
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.ReclamationEventCountLocked()
}

// HintRange applies an eviction hint to [offset, offset+len). Only objects a
// pager backs take hints; others ignore them.
func (o *ObjectPaged) HintRange(ctx context.Context, offset, length uint64, hint EvictionHint) error {
	if !o.cowPages.canRootSourceEvict() {
		return nil
	}
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	switch hint {
	case DontNeed:
		return o.cowPages.PromoteRangeForReclamation(r)
	case AlwaysNeed:
		// Hints are best effort, so errors paging in are ignored.
		return o.cowPages.ProtectRangeFromReclamation(ctx, r, true, true)
	}
	return nil
}

// PrefetchRange brings [offset, offset+len) in: read from the pager for an
// object a pager backs, decompressed otherwise.
func (o *ObjectPaged) PrefetchRange(ctx context.Context, offset, length uint64) error {
	end := offset + length
	if end < offset {
		return ErrOutOfRange
	}
	ps := o.pageSize()
	endPage := (end + ps - 1) &^ (ps - 1)
	if endPage < end {
		return ErrOutOfRange
	}
	offset &^= ps - 1
	length = endPage - offset
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	if o.cowPages.isRootSourceUserPagerBacked() {
		return o.cowPages.ProtectRangeFromReclamation(ctx, r, false, false)
	}
	return o.cowPages.DecompressInRange(ctx, r)
}

// CommitRange commits [offset, offset+len), waiting for any pages to be
// supplied.
func (o *ObjectPaged) CommitRange(ctx context.Context, offset, length uint64) error {
	return o.commitRangeInternal(ctx, offset, length)
}

// commitRangeInternal is CommitRangeInternal without pinning, which is not
// ported.
func (o *ObjectPaged) commitRangeInternal(ctx context.Context, offset, length uint64) error {
	ps := o.pageSize()
	// Round to pages.
	end := offset + length
	if end < offset {
		return ErrOutOfRange
	}
	endPage := (end + ps - 1) &^ (ps - 1)
	if endPage < end {
		return ErrOutOfRange
	}
	offset &^= ps - 1
	length = endPage - offset
	// The range must start in the object.
	o.lock().Lock()
	if !(CowRange{offset, length}).IsBoundedBy(o.cowPages.size) {
		o.lock().Unlock()
		return ErrOutOfRange
	}
	o.lock().Unlock()
	if length == 0 {
		return nil
	}
	pageRequest := NewMultiPageRequest()
	for length > 0 {
		committedLen, shrunk, status := func() (uint64, bool, error) {
			deferred := NewDeferredOps(o.cowPages)
			defer deferred.Finish()
			o.lock().Lock()
			defer o.lock().Unlock()
			// The object may have shrunk while the lock was dropped, which
			// ends a commit that does not pin.
			size := o.cowPages.size
			if offset >= size {
				return 0, true, nil
			}
			length = min(length, size-offset)
			r, _ := o.getCowRange(offset, length)
			committed, err := o.cowPages.CommitRangeLocked(ctx, r, deferred, pageRequest)
			return committed, false, err
		}()
		if shrunk {
			return nil
		}
		if status == ErrShouldWait {
			status = pageRequest.Wait(ctx)
		}
		if status != nil {
			return status
		}
		offset += committedLen
		length -= committedLen
	}
	return nil
}

// DecommitRange frees the pages of [offset, offset+len).
func (o *ObjectPaged) DecommitRange(offset, length uint64) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.DecommitRange(r)
}

// ZeroRange makes [offset, offset+len) read as zeros, Dirty in an object a
// pager backs.
func (o *ObjectPaged) ZeroRange(ctx context.Context, offset, length uint64) error {
	return o.zeroRangeInternal(ctx, offset, length, true)
}

// ZeroRangeUntracked makes whole pages read as zeros that are not dirty
// tracked.
func (o *ObjectPaged) ZeroRangeUntracked(ctx context.Context, offset, length uint64) error {
	ps := o.pageSize()
	if offset&(ps-1) != 0 || length&(ps-1) != 0 {
		return ErrInvalidArgs
	}
	return o.zeroRangeInternal(ctx, offset, length, false)
}

// zeroPartialPage zeroes [zeroStart, zeroEnd) of the page at pageBase.
func (o *ObjectPaged) zeroPartialPage(ctx context.Context, pageBase, zeroStart, zeroEnd uint64) error {
	assert(zeroStart <= zeroEnd, "the range is not backwards")
	assert(zeroEnd <= o.pageSize(), "the range is in a page")
	o.lock().Lock()
	if pageBase >= o.cowPages.size {
		o.lock().Unlock()
		return ErrOutOfRange
	}
	if o.cowPages.PageWouldReadZeroLocked(pageBase) {
		// Zero already.
		o.lock().Unlock()
		return nil
	}
	o.lock().Unlock()
	return o.readWriteInternal(ctx, pageBase+zeroStart, zeroEnd-zeroStart, true, func(page []byte, _ uint64) {
		clear(page)
	})
}

func (o *ObjectPaged) zeroRangeInternal(ctx context.Context, offset, length uint64, dirtyTrack bool) error {
	ps := o.pageSize()
	for length > 0 {
		// A start not page aligned is zeroed alone.
		if offset&(ps-1) != 0 {
			assert(dirtyTrack, "a partial page is dirty tracked")
			pageBase := offset &^ (ps - 1)
			zeroStart := offset - pageBase
			zeroLen := min(ps-zeroStart, length)
			if err := o.zeroPartialPage(ctx, pageBase, zeroStart, zeroStart+zeroLen); err != nil {
				return err
			}
			offset += zeroLen
			length -= zeroLen
			continue
		}
		// So is a last part shorter than a page.
		if length < ps {
			assert(dirtyTrack, "a partial page is dirty tracked")
			return o.zeroPartialPage(ctx, offset, 0, length)
		}
		// Decommit first, which works by the pages held, where the object
		// allows it.
		if r, ok := o.getCowRange(offset, length&^(ps-1)); !ok {
			return ErrOutOfRange
		} else if err := o.cowPages.DecommitRange(r); err == nil {
			offset += r.Len
			length -= r.Len
			continue
		}
		pageRequest := NewMultiPageRequest()
		zeroedLen, status := func() (uint64, error) {
			deferred := NewDeferredOps(o.cowPages)
			defer deferred.Finish()
			o.lock().Lock()
			defer o.lock().Unlock()
			r, ok := o.getCowRangeSizeCheckLocked(offset, length&^(ps-1))
			if !ok {
				return 0, ErrOutOfRange
			}
			zeroedLen, err := o.cowPages.ZeroPagesLocked(ctx, r, dirtyTrack, deferred, pageRequest)
			if zeroedLen != 0 {
				o.cowPages.markModifiedLocked()
			}
			return zeroedLen, err
		}()
		if status == ErrShouldWait {
			status = pageRequest.Wait(ctx)
		}
		if status != nil {
			return status
		}
		offset += zeroedLen
		length -= zeroedLen
	}
	return nil
}

// readWriteInternal copies to or from [offset, offset+len) a page at a
// time, calling copyFn with each page's part of the range and its offset
// from offset. Zircon also returns how much it copied, for user copies that
// fault part way; there are none, so no caller needs it.
func (o *ObjectPaged) readWriteInternal(ctx context.Context, offset, length uint64, write bool,
	copyFn func(page []byte, destOffset uint64)) error {
	ps := o.pageSize()
	endOffset := offset + length
	if endOffset < offset {
		return ErrOutOfRange
	}
	srcOffset := offset
	var destOffset uint64
	pageRequest := NewMultiPageRequest()
	for {
		done, status := func() (bool, error) {
			deferred := NewDeferredOps(o.cowPages)
			defer deferred.Finish()
			o.lock().Lock()
			defer o.lock().Unlock()
			// Zircon refuses an object mapped uncached; cache policy is not
			// ported.
			if endOffset > o.cowPages.size {
				return true, ErrOutOfRange
			}
			if srcOffset >= endOffset {
				return true, nil
			}
			firstPageOffset := srcOffset &^ (ps - 1)
			lastPageOffset := (endOffset - 1) &^ (ps - 1)
			remainingPages := (lastPageOffset-firstPageOffset)/ps + 1
			cursor, err := o.cowPages.GetLookupCursorLocked(CowRange{firstPageOffset, remainingPages * ps})
			if err != nil {
				return true, err
			}
			defer cursor.Release()
			// The caller asked for the access: no zero forks.
			cursor.DisableZeroFork()
			modified := false
			var status error
			for remainingPages > 0 {
				pageOffset := srcOffset % ps
				toCopy := min(ps-pageOffset, endOffset-srcOffset)
				// Wait for as many pages as the access spans, but cap a
				// write, which must read and then dirty.
				const maxWriteWaitPages = 256
				maxWaitPages := ^uint64(0)
				if write {
					maxWaitPages = maxWriteWaitPages
				}
				result, err := cursor.RequirePage(ctx, write, min(remainingPages, maxWaitPages), deferred, pageRequest)
				if err != nil {
					status = err
					break
				}
				copyFn(result.Page.data[pageOffset:pageOffset+toCopy], destOffset)
				srcOffset += toCopy
				destOffset += toCopy
				remainingPages--
				modified = write
				// Zircon yields a contested lock every 16 pages.
			}
			if modified {
				o.cowPages.markModifiedLocked()
			}
			return false, status
		}()
		if done {
			return status
		}
		if status == ErrShouldWait {
			status = pageRequest.Wait(ctx)
		}
		if status != nil {
			return status
		}
		if srcOffset >= endOffset {
			return nil
		}
	}
}

// Read copies [offset, offset+len(buf)) of the object into buf.
func (o *ObjectPaged) Read(ctx context.Context, buf []byte, offset uint64) error {
	return o.readWriteInternal(ctx, offset, uint64(len(buf)), false, func(page []byte, destOffset uint64) {
		copy(buf[destOffset:], page)
	})
}

// Write copies buf into [offset, offset+len(buf)) of the object.
func (o *ObjectPaged) Write(ctx context.Context, buf []byte, offset uint64) error {
	return o.readWriteInternal(ctx, offset, uint64(len(buf)), true, func(page []byte, destOffset uint64) {
		copy(page, buf[destOffset:])
	})
}

// Lookup calls fn on every page the object holds in [offset, offset+len).
func (o *ObjectPaged) Lookup(offset, length uint64, fn func(offset uint64, page *VmPage) error) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.LookupLocked(r, fn)
}

// LookupContiguous is the page at offset if every page of [offset,
// offset+len) is held and contiguous. Only a contiguous object, not ported,
// may ask for more than one page.
func (o *ObjectPaged) LookupContiguous(offset, length uint64) (*VmPage, error) {
	ps := o.pageSize()
	if length == 0 || offset&(ps-1) != 0 {
		return nil, ErrInvalidArgs
	}
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRangeSizeCheckLocked(offset, length)
	if !ok {
		return nil, ErrOutOfRange
	}
	if r.Len != ps {
		return nil, ErrBadState
	}
	var first *VmPage
	count := uint64(0)
	err := o.cowPages.LookupLocked(r, func(_ uint64, page *VmPage) error {
		count++
		if first == nil {
			first = page
		}
		return nil
	})
	assert(err == nil, "the lookup does not fail")
	if count != r.Len/ps {
		return nil, ErrNotFound
	}
	return first, nil
}

// TakePages moves the content of [offset, offset+len) into pages, which it
// initializes, leaving the range zero.
func (o *ObjectPaged) TakePages(ctx context.Context, offset, length uint64, pages *PageSpliceList[VmPage]) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	pages.Initialize(r.Len)
	var spliceOffset uint64
	pageRequest := NewMultiPageRequest()
	for !r.IsEmpty() {
		takenLen, status := o.cowPages.TakePages(ctx, r, spliceOffset, pages, pageRequest)
		if status != ErrShouldWait && status != nil {
			return status
		}
		assert(takenLen > 0 || status == ErrShouldWait, "progress is made")
		assert(status != nil || takenLen == r.Len, "all was taken")
		assert(takenLen <= r.Len, "no more than asked was taken")
		spliceOffset += takenLen
		r = r.TrimmedFromStart(takenLen)
		if status == ErrShouldWait {
			if err := pageRequest.Wait(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// SupplyPages puts pages in [offset, offset+len).
func (o *ObjectPaged) SupplyPages(ctx context.Context, offset, length uint64, pages *PageSpliceList[VmPage], options SupplyOptions) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	if r.IsEmpty() {
		return nil
	}
	// References become pages first, where the object needs pages.
	if err := o.cowPages.ProcessPagesForSupply(ctx, pages); err != nil {
		return err
	}
	deferred := NewDeferredOps(o.cowPages)
	defer deferred.Finish()
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.SupplyPagesLocked(r, pages, options, deferred)
}

// FailPageRequests fails the outstanding requests of [offset, offset+len).
func (o *ObjectPaged) FailPageRequests(offset, length uint64, status error) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.FailPageRequestsLocked(r, status)
}

// DirtyPages makes [offset, offset+len) Dirty, as the pager's answer to a
// DIRTY request.
func (o *ObjectPaged) DirtyPages(ctx context.Context, offset, length uint64) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	// Pages allocated by a call that could not finish are kept for the next.
	var allocList []*VmPage
	defer func() {
		for _, p := range allocList {
			o.cowPages.node.pmm.FreePage(p)
		}
	}()
	// Zircon goes again after waiting for the pmm; a Go allocation succeeds
	// or fails at once.
	return o.cowPages.DirtyPages(ctx, r, &allocList)
}

// EnumerateDirtyRanges calls fn on every run of [offset, offset+len) not
// Clean.
func (o *ObjectPaged) EnumerateDirtyRanges(offset, length uint64, fn func(offset, length uint64, isZero bool) error) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.EnumerateDirtyRangesLocked(r, fn)
}

// WritebackProtect takes write access away from [offset, offset+len): the
// range protection of a pause (D3).
func (o *ObjectPaged) WritebackProtect(offset, length uint64) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.WritebackProtectLocked(r)
}

// WritebackBegin begins a writeback of [offset, offset+len).
func (o *ObjectPaged) WritebackBegin(offset, length uint64, isZeroRange bool) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.WritebackBeginLocked(r, isZeroRange)
}

// WritebackEnd ends a writeback of [offset, offset+len).
func (o *ObjectPaged) WritebackEnd(offset, length uint64) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	return o.cowPages.WritebackEndLocked(r)
}

// WritebackAbandon gives back the pages of a writeback of [offset,
// offset+len) that will not end (D4).
func (o *ObjectPaged) WritebackAbandon(ctx context.Context, offset, length uint64) error {
	r, ok := o.getCowRange(offset, length)
	if !ok {
		return ErrOutOfRange
	}
	deferred := NewDeferredOps(o.cowPages)
	defer deferred.Finish()
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.WritebackAbandonLocked(ctx, r, deferred)
}

// ReadWriteback reads what a writeback in progress holds of whole pages
// from offset into buf: the bytes of its pause (D1).
func (o *ObjectPaged) ReadWriteback(ctx context.Context, buf []byte, offset uint64) error {
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.ReadWritebackLocked(ctx, offset, buf)
}

// QueryPagerVmoStats reports whether the object was modified, and resets
// that if reset.
func (o *ObjectPaged) QueryPagerVmoStats(reset bool) (bool, error) {
	o.lock().Lock()
	defer o.lock().Unlock()
	return o.cowPages.QueryPagerVmoStatsLocked(reset)
}

// DetachSource detaches the object's page source.
func (o *ObjectPaged) DetachSource() { o.cowPages.DetachSource() }

// GetPage is the page at offset. Without a fault flag only a page usable as
// it is is returned, or ErrNotFound; with one, absent content is asked for,
// returning ErrShouldWait with pageRequest to wait on.
func (o *ObjectPaged) GetPage(ctx context.Context, offset uint64, pfFlags uint, pageRequest *MultiPageRequest) (*VmPage, error) {
	deferred := NewDeferredOps(o.cowPages)
	defer deferred.Finish()
	o.lock().Lock()
	defer o.lock().Unlock()
	write := pfFlags&PfFlagWrite != 0
	r, ok := o.getCowRange(offset, o.pageSize())
	if !ok {
		return nil, ErrOutOfRange
	}
	cursor, err := o.cowPages.GetLookupCursorLocked(r)
	if err != nil {
		return nil, err
	}
	defer cursor.Release()
	// A hardware fault updates access times separately.
	if pfFlags&PfFlagHwFault != 0 {
		cursor.DisableMarkAccessed()
	}
	if pfFlags&PfFlagFaultMask == 0 {
		p := cursor.MaybePage(write)
		if p == nil {
			return nil, ErrNotFound
		}
		return p, nil
	}
	result, err := cursor.RequirePage(ctx, write, 1, deferred, pageRequest)
	if err != nil {
		return nil, err
	}
	return result.Page, nil
}

// GetPageBlocking is GetPage that waits for any request and goes again.
func (o *ObjectPaged) GetPageBlocking(ctx context.Context, offset uint64, pfFlags uint) (*VmPage, error) {
	pageRequest := NewMultiPageRequest()
	for {
		page, err := o.GetPage(ctx, offset, pfFlags, pageRequest)
		if err != ErrShouldWait {
			return page, err
		}
		if err := pageRequest.Wait(ctx); err != nil {
			return nil, err
		}
	}
}

// CreateClone is a clone of [offset, offset+size). Only SnapshotOnWrite is
// ported.
func (o *ObjectPaged) CreateClone(typ SnapshotType, offset, size uint64) (*ObjectPaged, error) {
	ps := o.pageSize()
	if offset&(ps-1) != 0 || size&(ps-1) != 0 {
		return nil, ErrInvalidArgs
	}
	if size > o.maxSize() {
		return nil, ErrOutOfRange
	}
	r, ok := o.getCowRange(offset, size)
	if !ok {
		return nil, ErrOutOfRange
	}
	deferred := NewDeferredOps(o.cowPages)
	defer deferred.Finish()
	o.lock().Lock()
	// Zircon refuses an object mapped uncached; cache policy is not ported.
	child, err := o.cowPages.CreateCloneLocked(typ, false, r)
	if err != nil {
		o.lock().Unlock()
		return nil, err
	}
	clone := &ObjectPaged{cowPages: child}
	child.paged = clone
	child.TransitionToAliveLocked()
	o.lock().Unlock()
	return clone, nil
}
