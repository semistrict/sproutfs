// Copyright 2018 The Fuchsia Authors
// Copyright 2021 The Fuchsia Authors
// Ported from zircon/kernel/vm/page_source.cc and object/pager_proxy.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
)

// Cases of the port's own for a page source over a PagerProxy, the way the
// pager's faults and prefetches use one: requests that batch, a supply or a
// failure that wakes every request waiting on the one it resolves, a waiter
// that gives up, and the proxy's account of what it holds. Zircon has no
// unit test of page_source.cc or pager_proxy.cc; its pager cases reach them
// through syscalls.

// proxySource is a page source over a PagerProxy, in pages of ps.
type proxySource struct {
	t      *testing.T
	ps     uint64
	proxy  *PagerProxy
	source *PageSource
}

func newProxySource(t *testing.T, ps uint64) *proxySource {
	proxy := NewPagerProxy(false)
	return &proxySource{t: t, ps: ps, proxy: proxy, source: NewPageSource(proxy)}
}

// read makes a read request for pages [first, first+count).
func (s *proxySource) read(first, count uint64) *PageRequest {
	s.t.Helper()
	request := NewPageRequest()
	expect(s.t, "a read request", s.source.GetPages(first*s.ps, count*s.ps, request), ErrShouldWait)
	return request
}

// outstanding is every outstanding read request, in pages.
func (s *proxySource) outstanding(first, last uint64) []RequestRange {
	var pages []RequestRange
	for _, r := range s.source.AppendOutstanding(nil, ReadRequest, first*s.ps, last*s.ps) {
		pages = append(pages, RequestRange{Offset: r.Offset / s.ps, Len: r.Len / s.ps})
	}
	return pages
}

// waiting waits on request on a goroutine of its own and reports how the
// wait ended.
func waiting(ctx context.Context, request *PageRequest) <-chan error {
	ended := make(chan error, 1)
	go func() { ended <- request.Wait(ctx) }()
	return ended
}

// ended reports how a wait ended, or that it has not.
func ended(wait <-chan error) (error, bool) {
	synctest.Wait()
	select {
	case err := <-wait:
		return err, true
	default:
		return nil, false
	}
}

// Requests that overlap batch. One that starts inside a request already sent
// waits on it, cut to where that one ends. One that starts before it is cut to
// end where that one starts, and is sent, even where it would run past its
// end. The source reports exactly the requests sent, whatever range it is
// asked about.
func TestOverlappingReadRequestsBatch(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent := s.read(4, 4)
		inside := s.read(5, 2)
		across := s.read(6, 4)
		before := s.read(1, 5)
		after := s.read(8, 2)
		spanning := s.read(0, 12)
		expect(t, "the first is sent", s.proxy.Holds(sent), true)
		expect(t, "one inside it waits", s.proxy.Holds(inside), false)
		expect(t, "one across its end waits", s.proxy.Holds(across), false)
		expect(t, "one before it is sent", s.proxy.Holds(before), true)
		expect(t, "one past its end is sent", s.proxy.Holds(after), true)
		expect(t, "one spanning them all is sent", s.proxy.Holds(spanning), true)
		expect(t, "the requests the proxy holds", len(s.proxy.requests), 4)
		want := []RequestRange{{0, 1}, {1, 3}, {4, 4}, {8, 2}}
		if got := s.outstanding(0, 16); !slices.Equal(got, want) {
			t.Errorf("outstanding %v, want %v", got, want)
		}
		if got, want := s.outstanding(5, 9), []RequestRange{{4, 4}, {8, 2}}; !slices.Equal(got, want) {
			t.Errorf("outstanding over [5, 9) %v, want %v", got, want)
		}
		if got, want := s.outstanding(3, 4), []RequestRange{{1, 3}}; !slices.Equal(got, want) {
			t.Errorf("outstanding over [3, 4) %v, want %v", got, want)
		}
		if got := s.outstanding(10, 16); len(got) != 0 {
			t.Errorf("outstanding past them all %v, want none", got)
		}
		if got, want := s.outstanding(0, 1), []RequestRange{{0, 1}}; !slices.Equal(got, want) {
			t.Errorf("outstanding over [0, 1) %v, want %v", got, want)
		}
	})
}

// A supply of a request's whole range wakes it and every request waiting on
// it at once, in whatever order its pages come. A supply of part of it wakes
// none of them, and neither does a supply of another request's range.
func TestASupplyWakesEveryRequestItCovers(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent := s.read(2, 4)
		first, second := s.read(3, 1), s.read(4, 2)
		other := s.read(8, 1)
		ctx := t.Context()
		waits := []<-chan error{waiting(ctx, sent), waiting(ctx, first), waiting(ctx, second)}
		otherWait := waiting(ctx, other)
		// Pages 4, 2 and 3, then 5.
		s.source.OnPagesSupplied(4*ps, ps)
		s.source.OnPagesSupplied(2*ps, 2*ps)
		s.source.OnPagesSupplied(8*ps, ps)
		for at, wait := range waits {
			if _, done := ended(wait); done {
				t.Errorf("request %d woke on a supply of three of its four pages", at)
			}
		}
		err, done := ended(otherWait)
		expect(t, "the other request woke on its own supply", done, true)
		expectNoError(t, "the other request", err)
		s.source.OnPagesSupplied(5*ps, ps)
		for at, wait := range waits {
			err, done := ended(wait)
			expect(t, "woke on the rest of the range", done, true)
			if err != nil {
				t.Errorf("request %d: %v", at, err)
			}
		}
		expect(t, "the proxy holds nothing", len(s.proxy.requests), 0)
		expect(t, "nothing is outstanding", len(s.outstanding(0, 16)), 0)
		expect(t, "the waits", s.proxy.Waits(), PagerWaits{Total: 4, Succeeded: 4})
	})
}

// A failure of a request's range ends it and its waiters with the failure,
// each whose start it covers. A waiter whose start it does not cover still
// wakes, without it, and finds its pages missing when it looks again. A
// failure that ends where another request begins leaves that one alone.
func TestAFailureEndsTheRequestsItCoversWithIt(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent := s.read(0, 4)
		covered, late := s.read(0, 1), s.read(2, 2)
		ctx := t.Context()
		sentWait, coveredWait, lateWait := waiting(ctx, sent), waiting(ctx, covered), waiting(ctx, late)
		s.source.OnPagesFailed(0, 2*ps, ErrIO)
		if _, done := ended(sentWait); done {
			t.Errorf("the request ended on a failure of half its range")
		}
		s.source.OnPagesSupplied(2*ps, 2*ps)
		err, done := ended(sentWait)
		expect(t, "the request ended", done, true)
		expect(t, "the request's status", err, ErrIO)
		err, done = ended(coveredWait)
		expect(t, "the waiter whose start failed ended", done, true)
		expect(t, "its status", err, ErrIO)
		err, done = ended(lateWait)
		expect(t, "the waiter whose start was supplied ended", done, true)
		expectNoError(t, "its status", err)
		expect(t, "the waits", s.proxy.Waits(), PagerWaits{Total: 3, Succeeded: 1, Failed: 2})

		s = newProxySource(t, ps)
		failed, next := s.read(0, 2), s.read(2, 2)
		failedWait, nextWait := waiting(ctx, failed), waiting(ctx, next)
		s.source.OnPagesFailed(0, 2*ps, ErrIO)
		s.source.OnPagesSupplied(2*ps, 2*ps)
		err, done = ended(failedWait)
		expect(t, "the failed request ended", done, true)
		expect(t, "its status", err, ErrIO)
		err, done = ended(nextWait)
		expect(t, "the next request ended", done, true)
		expectNoError(t, "its status", err)
	})
}

// A waiter whose context ends gives its request up: the request it waited on
// no longer wakes it, and still wakes the others.
func TestAWaiterThatGivesUpLeavesTheRequest(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent := s.read(0, 2)
		leaving, staying := s.read(0, 1), s.read(1, 1)
		ctx, cancel := context.WithCancel(t.Context())
		leftWait := waiting(ctx, leaving)
		stayWait := waiting(t.Context(), staying)
		cancel()
		err, done := ended(leftWait)
		expect(t, "the waiter gave up", done, true)
		expect(t, "with its context's error", errors.Is(err, context.Canceled), true)
		expect(t, "it is no longer in use", leaving.isInitialized(), false)
		expect(t, "one waits on the request", len(sent.overlap), 1)
		s.source.OnPagesSupplied(0, 2*ps)
		err, done = ended(stayWait)
		expect(t, "the other woke", done, true)
		expectNoError(t, "the other", err)
		expect(t, "the waits", s.proxy.Waits(), PagerWaits{Total: 2, Succeeded: 1, Failed: 1})
	})
}

// A request others wait on that is cancelled hands its place to the first of
// them, which the proxy holds from then on, and the supply of the range wakes
// it and the rest.
func TestTheProxyHoldsTheWaiterACancelledRequestHandsItsPlaceTo(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent := s.read(0, 2)
		first, second := s.read(0, 1), s.read(1, 1)
		sent.CancelRequest()
		expect(t, "the cancelled request is held", s.proxy.Holds(sent), false)
		expect(t, "the first waiter is held", s.proxy.Holds(first), true)
		expect(t, "the second waiter is held", s.proxy.Holds(second), false)
		if got, want := s.outstanding(0, 4), []RequestRange{{0, 2}}; !slices.Equal(got, want) {
			t.Errorf("outstanding %v, want %v", got, want)
		}
		ctx := t.Context()
		firstWait, secondWait := waiting(ctx, first), waiting(ctx, second)
		s.source.OnPagesSupplied(0, 2*ps)
		for _, wait := range []<-chan error{firstWait, secondWait} {
			err, done := ended(wait)
			expect(t, "woke", done, true)
			expectNoError(t, "the wake", err)
		}
		expect(t, "the proxy holds nothing", len(s.proxy.requests), 0)
	})
}

// A detached source ends every request it sent and every one waiting, and
// sends nothing more; closing it tells the proxy, which takes nothing after.
func TestADetachedSourceEndsItsRequestsAndClosesItsProxy(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		s := newProxySource(t, ps)
		sent, waiter := s.read(0, 2), s.read(1, 1)
		ctx := t.Context()
		sentWait, waiterWait := waiting(ctx, sent), waiting(ctx, waiter)
		s.source.Close()
		for _, wait := range []<-chan error{sentWait, waiterWait} {
			err, done := ended(wait)
			expect(t, "ended", done, true)
			expectNoError(t, "the end", err)
		}
		expect(t, "the proxy was told the source detached", s.proxy.completePending, true)
		expect(t, "the proxy was told the source closed", s.proxy.sourceClosed, true)
		expect(t, "a request after", s.source.GetPages(0, ps, NewPageRequest()), ErrBadState)
		defer func() {
			if recover() == nil {
				t.Errorf("a closed proxy took a request")
			}
		}()
		s.proxy.SendAsyncRequest(&PageRequest{providerOwned: true})
	})
}

// A proxy answers reads, and dirty requests only where it traps them.
func TestAProxyAnswersDirtyRequestsOnlyWhereItTrapsThem(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		reads := NewPageSource(NewPagerProxy(false))
		expect(t, "a user pager", reads.Properties().IsUserPager, true)
		expect(t, "reads", reads.SupportsPageRequestType(ReadRequest), true)
		expect(t, "no dirty requests", reads.ShouldTrapDirtyTransitions(), false)
		expect(t, "no writebacks", reads.SupportsPageRequestType(WritebackRequest), false)
		expect(t, "a dirty request", reads.RequestDirtyTransition(NewPageRequest(), 0, ps), ErrNotSupported)
		proxy := NewPagerProxy(true)
		dirty := NewPageSource(proxy)
		expect(t, "dirty requests", dirty.ShouldTrapDirtyTransitions(), true)
		request := NewPageRequest()
		expect(t, "a dirty request", dirty.RequestDirtyTransition(request, 0, ps), ErrShouldWait)
		expect(t, "it is held", proxy.Holds(request), true)
		expect(t, "it is no read", len(dirty.AppendOutstanding(nil, ReadRequest, 0, ps)), 0)
		expect(t, "it is a dirty request", len(dirty.AppendOutstanding(nil, DirtyRequest, 0, ps)), 1)
		expect(t, "any page is fine", proxy.DebugIsPageOk(nil, 0), true)
		dirty.OnPagesDirtied(0, ps)
		expect(t, "it is answered", proxy.Holds(request), false)
		defer func() {
			if recover() == nil {
				t.Errorf("a proxy that does not trap dirty transitions took a dirty request")
			}
		}()
		NewPagerProxy(false).SendAsyncRequest(&PageRequest{providerOwned: true, typ: DirtyRequest})
	})
}
