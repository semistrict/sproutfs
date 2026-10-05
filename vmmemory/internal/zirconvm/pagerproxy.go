// Copyright 2021 The Fuchsia Authors
// Ported from zircon/kernel/object/pager_proxy.cc and object/include/object/pager_proxy.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
)

// PagerProxy is the provider of a page source whose pager is in this
// process, Zircon's PagerProxy.
//
// Zircon's PagerProxy hands each request the source sends it to the user
// pager as a packet on a port. It has one packet, so it queues the requests
// that come while the packet is out, and sends the next when the pager has
// read it. The pager reads the port on threads of its own, reads the pages,
// and supplies or fails their range on the VMO.
//
// Departure: no port and no packet. The pager here is in the process that
// makes the requests, and the caller that sent a request answers it: a fault
// or a prefetch reads the pages on a goroutine, its own or one it starts, and
// supplies or fails the request's range on the source. So every request is
// answered at once, on a goroutine of its own, where Zircon's are answered
// one packet at a time. What is kept is the proxy's account of the requests
// the source has handed it and not taken back, which tells a caller whether
// its request was sent or waits on another (Holds), and the counts of the
// waits on its requests. The overtime warnings, the dump, the koid and the
// dispatcher's close are not ported: a wait here ends with its context.
type PagerProxy struct {
	trapDirty bool

	// mu is mtx_.
	mu sync.Mutex
	// sourceClosed is page_source_closed_: the source has closed and sends
	// nothing more.
	sourceClosed bool
	// completePending is complete_pending_: the source has detached, which
	// Zircon tells the pager with a COMPLETE packet. Nothing reads it here.
	completePending bool
	// requests is every request the source has sent and not taken back:
	// Zircon's active_request_ followed by its pending_requests_.
	requests []*PageRequest

	// counts are the dispatcher.pager kcounters, per proxy.
	counts struct{ total, succeeded, failed atomic.Uint64 }
}

// NewPagerProxy is a proxy for a pager that answers read requests, and dirty
// requests too where trapDirty says so: Zircon's kTrapDirty option.
func NewPagerProxy(trapDirty bool) *PagerProxy { return &PagerProxy{trapDirty: trapDirty} }

// Properties are a user pager's.
func (p *PagerProxy) Properties() PageSourceProperties {
	return PageSourceProperties{
		IsUserPager:         true,
		SupportsRequestType: [numPageRequestTypes]bool{true, p.trapDirty, false},
	}
}

// SendAsyncRequest takes a request the source sends. Zircon queues its
// packet; here the caller that made it answers it.
func (p *PagerProxy) SendAsyncRequest(request *PageRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	assert(!p.sourceClosed, "the source is open")
	assert(RequestType(request) == ReadRequest || p.trapDirty, "the proxy answers the request's type")
	p.requests = append(p.requests, request)
}

// ClearAsyncRequest gives a request back to the source.
func (p *PagerProxy) ClearAsyncRequest(request *PageRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	assert(!p.sourceClosed, "the source is open")
	if i := slices.Index(p.requests, request); i >= 0 {
		p.requests = slices.Delete(p.requests, i, i+1)
	}
}

// SwapAsyncRequest holds newReq where it held old.
func (p *PagerProxy) SwapAsyncRequest(old, newReq *PageRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	assert(!p.sourceClosed, "the source is open")
	if i := slices.Index(p.requests, old); i >= 0 {
		p.requests[i] = newReq
	}
}

// DebugIsPageOk accepts every page, as Zircon's does.
func (p *PagerProxy) DebugIsPageOk(*VmPage, uint64) bool { return true }

// OnDetach notes that the source has detached and sends nothing more but
// writebacks. Zircon queues the COMPLETE packet here.
func (p *PagerProxy) OnDetach() {
	p.mu.Lock()
	defer p.mu.Unlock()
	assert(!p.sourceClosed, "the source is open")
	p.completePending = true
}

// OnClose notes that the source has closed.
func (p *PagerProxy) OnClose() {
	p.mu.Lock()
	defer p.mu.Unlock()
	assert(!p.sourceClosed, "the source is open")
	p.sourceClosed = true
}

// WaitOnEvent waits for a request's event, counting the wait as Zircon's
// kcounters do: every wait, those that end with the request supplied, and
// those that end otherwise.
func (p *PagerProxy) WaitOnEvent(ctx context.Context, event *Event) error {
	p.counts.total.Add(1)
	status := event.Wait(ctx)
	if status == nil {
		p.counts.succeeded.Add(1)
	} else {
		p.counts.failed.Add(1)
	}
	return status
}

// Holds reports whether the source has sent request and not taken it back:
// whether its caller is the one to answer it, rather than one waiting on
// another's.
func (p *PagerProxy) Holds(request *PageRequest) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.requests, request)
}

// PagerWaits counts the waits on a proxy's requests: Zircon's
// dispatcher.pager.total_requests, succeeded_requests and failed_requests.
type PagerWaits struct{ Total, Succeeded, Failed uint64 }

// Waits counts the waits on this proxy's requests.
func (p *PagerProxy) Waits() PagerWaits {
	return PagerWaits{Total: p.counts.total.Load(), Succeeded: p.counts.succeeded.Load(),
		Failed: p.counts.failed.Load()}
}
