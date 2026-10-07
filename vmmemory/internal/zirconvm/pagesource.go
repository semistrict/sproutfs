// Copyright 2018 The Fuchsia Authors
// Ported from zircon/kernel/vm/page_source.cc and vm/include/vm/page_source.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"sort"
	"sync"
)

// Zircon's page source, ported for the region's layer (step 9 of the plan)
// and for the pager's faults and prefetches (step 8), whose provider is the
// PagerProxy of pagerproxy.go. Left out: the early wake of a request a supply
// has reached part of, continuations of an early waking request, the overlap
// counters, the debug dump and the provider's free of pages. Outstanding
// requests are kept sorted in a slice where Zircon keeps a WAVL tree keyed by
// their end. AppendOutstanding is not Zircon's; see there.

// PageRequestType is page_request_type.
type PageRequestType uint8

const (
	// ReadRequest asks for a page's initial contents.
	ReadRequest PageRequestType = iota
	// DirtyRequest asks to make a clean page dirty.
	DirtyRequest
	// WritebackRequest asks to write a page back.
	WritebackRequest
	numPageRequestTypes
)

// PageSourceProperties are a provider's properties.
type PageSourceProperties struct {
	// IsUserPager says the provider is a user pager: it preserves content,
	// supplies pages and tracks them dirty.
	IsUserPager bool
	// SupportsRequestType says which requests the provider answers.
	SupportsRequestType [numPageRequestTypes]bool
}

// PageProvider answers a page source's requests, Zircon's PageProvider.
type PageProvider interface {
	Properties() PageSourceProperties
	// SendAsyncRequest hands the provider a request it now owns.
	SendAsyncRequest(request *PageRequest)
	// ClearAsyncRequest takes a request back.
	ClearAsyncRequest(request *PageRequest)
	// SwapAsyncRequest gives the provider newReq in place of old.
	SwapAsyncRequest(old, newReq *PageRequest)
	// DebugIsPageOk checks a page the source supplied, for asserts.
	DebugIsPageOk(page *VmPage, offset uint64) bool
	// OnDetach and OnClose tell the provider the source is detached and
	// closed.
	OnDetach()
	OnClose()
	// WaitOnEvent waits for a request's event. Zircon's takes whether the
	// wait may be suspended; a Go wait ends with its context instead.
	WaitOnEvent(ctx context.Context, event *Event) error
}

// RequestType is the type of a request the provider owns.
func RequestType(request *PageRequest) PageRequestType {
	assert(request.providerOwned, "the provider owns the request")
	return request.typ
}

// RequestOffset is the offset of a request the provider owns.
func RequestOffset(request *PageRequest) uint64 {
	assert(request.providerOwned, "the provider owns the request")
	return request.offset
}

// RequestSource is the source a request in use was made on, whether it was
// sent or waits on another's.
func RequestSource(request *PageRequest) *PageSource {
	assert(request.isInitialized(), "the request is in use")
	return request.src
}

// RequestLen is the length of a request the provider owns.
func RequestLen(request *PageRequest) uint64 {
	assert(request.providerOwned, "the provider owns the request")
	return request.len
}

// Event is an auto-unsignaling event with a status, Zircon's
// AutounsignalEvent: a signal wakes one wait and is then consumed.
type Event struct{ ch chan error }

func newEvent() *Event { return &Event{ch: make(chan error, 1)} }

// Signal wakes a wait with status, or the next one if none waits.
func (e *Event) Signal(status error) {
	select {
	case e.ch <- status:
	default:
		// Already signaled. Zircon keeps the first status.
	}
}

// unsignal clears a signal not yet waited for.
func (e *Event) unsignal() {
	select {
	case <-e.ch:
	default:
	}
}

// Wait waits for a signal and returns its status, or the context's error.
func (e *Event) Wait(ctx context.Context) error {
	select {
	case status := <-e.ch:
		return status
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PageSource is the source of an object's pages, Zircon's PageSource: it
// turns a lookup's need for pages into requests to its provider and wakes
// the requests a supply or a failure resolves.
type PageSource struct {
	// pagedVmoMutex serializes every operation on the objects of a hierarchy
	// this source backs, with their deferred work.
	pagedVmoMutex sync.Mutex

	// mu is page_source_mtx_.
	mu       sync.Mutex
	detached bool
	closed   bool

	properties PageSourceProperties
	provider   PageProvider

	// outstanding are the requests sent to the provider, by type, sorted by
	// their end.
	outstanding [numPageRequestTypes][]*PageRequest
}

// NewPageSource is a source over provider.
func NewPageSource(provider PageProvider) *PageSource {
	return &PageSource{properties: provider.Properties(), provider: provider}
}

// Properties are the provider's properties.
func (s *PageSource) Properties() PageSourceProperties { return s.properties }

// SupportsPageRequestType reports whether the provider answers requests of
// typ.
func (s *PageSource) SupportsPageRequestType(typ PageRequestType) bool {
	return s.properties.SupportsRequestType[typ]
}

// ShouldTrapDirtyTransitions reports whether a clean page must be asked for
// before it becomes dirty.
func (s *PageSource) ShouldTrapDirtyTransitions() bool {
	return s.SupportsPageRequestType(DirtyRequest)
}

// IsDetached reports whether the source is detached from its provider.
func (s *PageSource) IsDetached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detached
}

// DebugIsPageOk checks a page the source supplied.
func (s *PageSource) DebugIsPageOk(page *VmPage, offset uint64) bool {
	return s.provider.DebugIsPageOk(page, offset)
}

// GetPages asks for [offset, offset+len) to be supplied and returns
// ErrShouldWait with request to wait on.
func (s *PageSource) GetPages(offset, length uint64, request *PageRequest) error {
	return s.populateRequest(request, offset, length, ReadRequest)
}

// SendPages is GetPages reporting, as the source's lock held them, whether
// the request went to the provider and how much of the range it asks for.
//
// Departure: Zircon's caller waits on its request and never looks at it. Here
// the caller answers a request it sent itself, and asks what it sent. It cannot
// ask after: once the lock goes, a supply of the range may complete the
// request, and its state is no longer the caller's to read.
func (s *PageSource) SendPages(offset, length uint64, request *PageRequest) (sent bool, sentLen uint64, err error) {
	assert(request != nil, "there is a request")
	assert(length > 0, "the range is not empty")
	if !s.SupportsPageRequestType(ReadRequest) {
		return false, 0, ErrNotSupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached {
		return false, 0, ErrBadState
	}
	assert(!request.isInitialized(), "the request is not in use")
	err = s.populateRequestLocked(request, offset, length, ReadRequest)
	return request.providerOwned, request.len, err
}

// RequestDirtyTransition asks for [offset, offset+len) to be made dirty and
// returns ErrShouldWait with request to wait on.
func (s *PageSource) RequestDirtyTransition(request *PageRequest, offset, length uint64) error {
	return s.populateRequest(request, offset, length, DirtyRequest)
}

// Detach cancels every outstanding request but writebacks, and tells the
// provider.
func (s *PageSource) Detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached {
		return
	}
	s.detached = true
	// Tell the waiters the requests are complete; they fail when they try
	// the same pages again.
	for typ := range numPageRequestTypes {
		if typ == WritebackRequest || !s.SupportsPageRequestType(typ) {
			continue
		}
		for len(s.outstanding[typ]) > 0 {
			req := s.outstanding[typ][0]
			s.outstanding[typ] = s.outstanding[typ][1:]
			s.completeRequestLocked(req)
		}
	}
	// No writebacks are supported yet.
	assert(len(s.outstanding[WritebackRequest]) == 0, "no writeback is outstanding")
	s.provider.OnDetach()
}

// Close detaches the source and tells the provider it is closed.
func (s *PageSource) Close() {
	// A no-op if already detached.
	s.Detach()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.provider.OnClose()
}

// OnPagesSupplied resolves the read requests [offset, offset+len) covers.
func (s *PageSource) OnPagesSupplied(offset, length uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveRequestsLocked(ReadRequest, offset, length, nil)
}

// OnPagesDirtied resolves the dirty requests [offset, offset+len) covers.
func (s *PageSource) OnPagesDirtied(offset, length uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveRequestsLocked(DirtyRequest, offset, length, nil)
}

// OnPagesFailed fails every request [offset, offset+len) covers with status.
func (s *PageSource) OnPagesFailed(offset, length uint64, status error) {
	assert(IsValidInternalFailureCode(status), "the failure is one a request may end with")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached {
		return
	}
	for typ := range numPageRequestTypes {
		if !s.SupportsPageRequestType(typ) {
			continue
		}
		s.resolveRequestsLocked(typ, offset, length, status)
	}
}

// IsValidExternalFailureCode reports whether a provider may fail a request
// with status.
func IsValidExternalFailureCode(status error) bool {
	switch status {
	case ErrIO, ErrIODataIntegrity, ErrBadState, ErrNoSpace, ErrBufferTooSmall:
		return true
	}
	return false
}

// IsValidInternalFailureCode reports whether the kernel side may fail a
// request with status.
func IsValidInternalFailureCode(status error) bool {
	return status == ErrNoMemory || IsValidExternalFailureCode(status)
}

// RequestRange is the range an outstanding request asks for.
type RequestRange struct{ Offset, Len uint64 }

// AppendOutstanding appends to dst the range of each outstanding request of
// typ that overlaps [start, end), in order, and returns the result.
//
// Departure: Zircon's callers never look at the outstanding requests. A
// lookup sends its request and waits, and the user pager in another process
// answers it. Here the pager is in the callers' process, and a caller that
// reads pages itself asks first which of them a request already sent is
// reading, to wait on that request or to leave those pages to it.
func (s *PageSource) AppendOutstanding(dst []RequestRange, typ PageRequestType, start, end uint64) []RequestRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.outstanding[typ]
	for i := s.upperBound(typ, start); i < len(list) && list[i].offset < end; i++ {
		dst = append(dst, RequestRange{Offset: list[i].offset, Len: list[i].len})
	}
	return dst
}

// upperBound is the index of the first outstanding request of typ whose end
// is past offset.
func (s *PageSource) upperBound(typ PageRequestType, offset uint64) int {
	list := s.outstanding[typ]
	return sort.Search(len(list), func(i int) bool { return list[i].end() > offset })
}

func (s *PageSource) insertOutstanding(req *PageRequest) {
	i := s.upperBound(req.typ, req.end()-1)
	list := append(s.outstanding[req.typ], nil)
	copy(list[i+1:], list[i:])
	list[i] = req
	s.outstanding[req.typ] = list
}

func (s *PageSource) eraseOutstanding(req *PageRequest) {
	list := s.outstanding[req.typ]
	for i, r := range list {
		if r == req {
			s.outstanding[req.typ] = append(list[:i], list[i+1:]...)
			return
		}
	}
	panic("zirconvm: the request is not outstanding")
}

// resolveRequestsLocked counts [offset, offset+len) against the requests it
// covers and completes those it finishes, failing them with status if it is
// not nil.
func (s *PageSource) resolveRequestsLocked(typ PageRequestType, offset, length uint64, status error) {
	end := offset + length
	assert(end >= offset, "the range does not overflow")
	if s.detached {
		return
	}
	// The first request this can fulfill has the smallest end past offset.
	for i := s.upperBound(typ, offset); i < len(s.outstanding[typ]); {
		cur := s.outstanding[typ][i]
		if cur.offset >= end {
			break
		}
		reqOffset, reqEnd := cur.trimRangeToRequestSpace(offset, end)
		if status != nil {
			if reqOffset == 0 {
				cur.completeStatus = status
			}
			for _, overlap := range cur.overlap {
				if !overlap.rangeOverlaps(offset, end) {
					continue
				}
				if overlapStart, _ := overlap.trimRangeToRequestSpace(offset, end); overlapStart == 0 {
					overlap.completeStatus = status
				}
			}
		}
		fulfill := reqEnd - reqOffset
		if fulfill < cur.pendingSize {
			// Zircon wakes a request early here if this reaches its wake
			// offset. Early wake is not ported.
			cur.pendingSize -= fulfill
			i++
			continue
		}
		s.outstanding[typ] = append(s.outstanding[typ][:i], s.outstanding[typ][i+1:]...)
		s.completeRequestLocked(cur)
	}
}

// populateRequest sends a new request for [offset, offset+len) of typ.
func (s *PageSource) populateRequest(request *PageRequest, offset, length uint64, typ PageRequestType) error {
	assert(request != nil, "there is a request")
	assert(length > 0, "the range is not empty")
	if !s.SupportsPageRequestType(typ) {
		return ErrNotSupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached {
		return ErrBadState
	}
	// Zircon allows an initialized request here only if it wakes early and
	// this continues it. Early wake is not ported.
	assert(!request.isInitialized(), "the request is not in use")
	return s.populateRequestLocked(request, offset, length, typ)
}

func (s *PageSource) populateRequestLocked(request *PageRequest, offset, length uint64, typ PageRequestType) error {
	assert(!request.isInitialized(), "the request is not in use")
	request.init(s, offset, typ)
	request.len += length
	assert(request.len >= length, "the length does not overflow")
	curEnd := request.offset + request.len
	assert(curEnd >= request.offset, "the range does not overflow")
	if i := s.upperBound(typ, request.offset); i < len(s.outstanding[typ]) {
		node := s.outstanding[typ][i]
		if request.offset >= node.offset && curEnd >= node.end() {
			// The start is covered by a request already: end there, and
			// wait for that one first.
			request.len = node.end() - request.offset
		} else if request.offset < node.offset && curEnd >= node.offset {
			// End where the next request starts.
			request.len = node.offset - request.offset
		}
	}
	s.sendRequestToProviderLocked(request)
	return ErrShouldWait
}

func (s *PageSource) sendRequestToProviderLocked(request *PageRequest) {
	assert(request.isInitialized(), "the request is in use")
	assert(s.SupportsPageRequestType(request.typ), "the provider answers the request")
	// A request starting inside an outstanding one is contained in it, and
	// waits on it.
	if i := s.upperBound(request.typ, request.offset); i < len(s.outstanding[request.typ]) &&
		s.outstanding[request.typ][i].offset <= request.offset {
		overlap := s.outstanding[request.typ][i]
		overlap.overlap = append(overlap.overlap, request)
		request.overlapping = overlap
		return
	}
	assert(!request.providerOwned, "the provider does not own the request yet")
	request.pendingSize = request.len
	request.providerOwned = true
	s.provider.SendAsyncRequest(request)
	s.insertOutstanding(request)
}

// completeRequestLocked takes a request back from the provider and wakes it
// and every request waiting on it.
func (s *PageSource) completeRequestLocked(request *PageRequest) {
	assert(s.SupportsPageRequestType(request.typ), "the provider answers the request")
	s.provider.ClearAsyncRequest(request)
	request.providerOwned = false
	for _, waiter := range request.overlap {
		assert(!waiter.providerOwned, "a waiting request is not the provider's")
		waiter.offset = noRequestOffset
		waiter.overlapping = nil
		waiter.event.Signal(waiter.completeStatus)
	}
	request.overlap = nil
	request.offset = noRequestOffset
	request.event.Signal(request.completeStatus)
}

// cancelRequest withdraws a request.
func (s *PageSource) cancelRequest(request *PageRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelRequestLocked(request)
}

func (s *PageSource) cancelRequestLocked(request *PageRequest) {
	if !request.isInitialized() {
		return
	}
	assert(s.SupportsPageRequestType(request.typ), "the provider answers the request")
	switch {
	case request.overlapping != nil:
		// It waits on another request, so just leave that one.
		main := request.overlapping
		for i, r := range main.overlap {
			if r == request {
				main.overlap = append(main.overlap[:i], main.overlap[i+1:]...)
				break
			}
		}
		request.overlapping = nil
	case len(request.overlap) > 0:
		// Others wait on it: the first of them takes its place.
		newNode := request.overlap[0]
		assert(!newNode.providerOwned, "a waiting request is not the provider's")
		newNode.overlap = request.overlap[1:]
		for _, w := range newNode.overlap {
			w.overlapping = newNode
		}
		newNode.overlapping = nil
		request.overlap = nil
		newNode.offset = request.offset
		newNode.len = request.len
		newNode.pendingSize = request.pendingSize
		assert(newNode.typ == request.typ, "the requests are of one type")
		s.eraseOutstanding(request)
		s.insertOutstanding(newNode)
		newNode.providerOwned = true
		s.provider.SwapAsyncRequest(request, newNode)
		request.providerOwned = false
	case request.providerOwned:
		s.eraseOutstanding(request)
		s.provider.ClearAsyncRequest(request)
		request.providerOwned = false
	}
	request.offset = noRequestOffset
}

// noRequestOffset marks a request not in use, Zircon's UINT64_MAX offset_.
const noRequestOffset = ^uint64(0)

// PageRequest is a request for pages a lookup waits on, Zircon's PageRequest.
type PageRequest struct {
	typ            PageRequestType
	providerOwned  bool
	completeStatus error
	src            *PageSource
	event          *Event
	offset         uint64
	len            uint64
	pendingSize    uint64
	// overlap are the requests waiting on this one, and overlapping the one
	// this waits on: Zircon's intrusive overlap_ list.
	overlap     []*PageRequest
	overlapping *PageRequest
}

// NewPageRequest is a request not yet in use.
func NewPageRequest() *PageRequest {
	return &PageRequest{offset: noRequestOffset, event: newEvent()}
}

func (r *PageRequest) isInitialized() bool { return r.offset != noRequestOffset }

func (r *PageRequest) init(src *PageSource, offset uint64, typ PageRequestType) {
	assert(!r.isInitialized(), "the request is not in use")
	r.len = 0
	r.offset = offset
	r.typ = typ
	r.src = src
	r.completeStatus = nil
	r.event.unsignal()
}

func (r *PageRequest) end() uint64 {
	end := r.offset + r.len
	assert(end >= r.offset, "the request does not overflow")
	return end
}

func (r *PageRequest) rangeOverlaps(start, end uint64) bool {
	return end > r.offset && start < r.end()
}

// trimRangeToRequestSpace is [start, end) relative to the request, clipped
// to it.
func (r *PageRequest) trimRangeToRequestSpace(start, end uint64) (uint64, uint64) {
	var reqOffset, reqEnd uint64
	if start >= r.offset {
		reqOffset = start - r.offset
	}
	if end < r.end() {
		assert(end >= r.offset, "the range ends after the request starts")
		reqEnd = end - r.offset
	} else {
		reqEnd = r.len
	}
	assert(reqEnd >= reqOffset, "the trimmed range is not backwards")
	return reqOffset, reqEnd
}

// Wait waits for the request to be resolved. A status other than one a
// request may fail with cancels the request.
func (r *PageRequest) Wait(ctx context.Context) error {
	status := r.src.provider.WaitOnEvent(ctx, r.event)
	if status != nil && !IsValidInternalFailureCode(status) {
		r.src.cancelRequest(r)
	}
	return status
}

// CancelRequest withdraws the request if it is in use.
func (r *PageRequest) CancelRequest() {
	if r.src == nil {
		return
	}
	r.src.cancelRequest(r)
}

// MultiPageRequest is the one request a lookup may make, of whichever kind it
// needs, Zircon's MultiPageRequest. Zircon's anonymous request waits for the
// pmm to have pages; a Pmm here fails or succeeds at once, so it is not kept.
type MultiPageRequest struct {
	readActive, dirtyActive bool
	pageRequest             *PageRequest
}

// NewMultiPageRequest is a request not yet in use.
func NewMultiPageRequest() *MultiPageRequest {
	return &MultiPageRequest{}
}

func (m *MultiPageRequest) noRequestActive() bool { return !m.readActive && !m.dirtyActive }

// getReadRequest is the request for a read, which is then active.
func (m *MultiPageRequest) getReadRequest() *PageRequest {
	assert(m.noRequestActive(), "no request is active")
	m.readActive = true
	return m.getLazy()
}

// getLazyDirtyRequest is the request a dirty request would use, Zircon's
// LazyPageRequest.
func (m *MultiPageRequest) getLazy() *PageRequest {
	if m.pageRequest == nil {
		m.pageRequest = NewPageRequest()
	}
	return m.pageRequest
}

// ReadRequest is the read request a lookup made, which is active. Zircon's
// page fault handler waits on it; the pager, which is in the faulting
// process, asks its provider whether it was sent (PagerProxy.Holds), answers
// it where it was, and waits on it where it waits on another.
func (m *MultiPageRequest) ReadRequest() *PageRequest {
	assert(m.readActive, "a read request is active")
	return m.pageRequest
}

// madeDirtyRequest marks the dirty request active.
func (m *MultiPageRequest) madeDirtyRequest() {
	assert(m.noRequestActive(), "no request is active")
	m.dirtyActive = true
}

// Wait waits on whichever request is active.
func (m *MultiPageRequest) Wait(ctx context.Context) error {
	// Exactly one of read and dirty is active.
	assert(m.dirtyActive != m.readActive, "one request is active")
	m.readActive = false
	m.dirtyActive = false
	return m.pageRequest.Wait(ctx)
}

// CancelRequests withdraws any request in use.
func (m *MultiPageRequest) CancelRequests() {
	if m.pageRequest != nil {
		m.pageRequest.CancelRequest()
	}
	m.readActive = false
	m.dirtyActive = false
}
