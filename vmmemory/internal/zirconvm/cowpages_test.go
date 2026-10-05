// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/vmo_unittest.cc and vm/unittests/test_helper.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
)

// Cases of the port's own, not Zircon's, for what its VMO cases do not reach:
// page requests waited on, failed and cancelled; a lookup racing a
// compression; zeroing a child; taking from a child; detaching a source; the
// lookup cursor's skips; the isolate queue. Zircon reaches most of these
// through its pager and page queue cases, which steps 5 and 8 port.

// waitingProvider records its requests and waits on them as a pager's
// client does.
type waitingProvider struct {
	recordingProvider
	swaps int
}

func newWaitingProvider(trapDirty bool) *waitingProvider {
	return &waitingProvider{recordingProvider: *newRecordingProvider(trapDirty)}
}

func (p *waitingProvider) SwapAsyncRequest(_, _ *PageRequest) { p.swaps++ }

func (p *waitingProvider) WaitOnEvent(ctx context.Context, event *Event) error {
	return event.Wait(ctx)
}

// A read and a fault inside it wait on one request, which a supply of its
// pages ends for both.
func TestReadsOfOneRangeWaitOnOneRequestAndASupplyWakesThem(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 4*ps)
		mustNotFail(t, "create", err)
		done := make(chan []byte)
		go func() {
			buf := make([]byte, 4*ps)
			mustNotFail(t, "read", vmo.Read(env.ctx, buf, 0))
			done <- buf
		}()
		synctest.Wait()
		faulted := make(chan *VmPage)
		go func() {
			page, err := vmo.GetPageBlocking(env.ctx, 2*ps, PfFlagSwFault)
			mustNotFail(t, "fault", err)
			faulted <- page
		}()
		synctest.Wait()
		// One request reached the provider: the fault waits on the read's.
		expect(t, "requests", len(provider.requests), 1)
		expect(t, "the request", provider.requests[0], CowRange{0, 4 * ps})
		expect(t, "the request is a read", provider.types[0], ReadRequest)
		aux, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create aux", err)
		mustNotFail(t, "write aux", aux.Write(env.ctx, pattern(4*ps, 'S'), 0))
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, 4*ps, splice))
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, 4*ps, splice, PagerSupply))
		expect(t, "the read", bytes.Equal(<-done, pattern(4*ps, 'S')), true)
		expect(t, "the fault's page", <-faulted, vmo.DebugGetPage(2*ps))
	})
}

// A request the pager fails ends its waiters with the failure, and a
// failure only a pager may use is refused.
func TestAFailedRequestFailsItsWaiters(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 2*ps)
		mustNotFail(t, "create", err)
		failed := make(chan error)
		go func() {
			_, err := vmo.GetPageBlocking(env.ctx, ps, PfFlagSwFault)
			failed <- err
		}()
		synctest.Wait()
		expect(t, "a status no request ends with", vmo.FailPageRequests(0, 2*ps, ErrNotFound), ErrInvalidArgs)
		expect(t, "out of range", vmo.FailPageRequests(0, 3*ps, ErrIO), ErrOutOfRange)
		mustNotFail(t, "fail", vmo.FailPageRequests(0, 2*ps, ErrIO))
		expect(t, "the waiter's error", <-failed, ErrIO)
	})
}

// A request others wait on that is cancelled hands its place to the first
// of them, which the provider then owns.
func TestACancelledRequestHandsItsPlaceToAWaiter(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		source := NewPageSource(provider)
		vmo, err := CreateExternal(env.node, source, 4*ps)
		mustNotFail(t, "create", err)
		first := NewMultiPageRequest()
		_, err = vmo.GetPage(env.ctx, 0, PfFlagSwFault, first)
		expect(t, "first", err, ErrShouldWait)
		// The first request asked for one page; a read of more is made of
		// the second.
		second := NewMultiPageRequest()
		_, err = vmo.GetPage(env.ctx, 0, PfFlagSwFault, second)
		expect(t, "second", err, ErrShouldWait)
		expect(t, "one request sent", len(provider.requests), 1)
		first.CancelRequests()
		expect(t, "the waiter took its place", provider.swaps, 1)
		woken := make(chan error)
		go func() { woken <- second.Wait(env.ctx) }()
		synctest.Wait()
		supplyPagerVmoPages(t, env, vmo, 0, 1)
		expectNoError(t, "the second woke", <-woken)
		second.CancelRequests()
	})
}

// A detached source ends its requests, frees the pages it can supply again,
// and keeps the pages not yet written back.
func TestADetachedSourceKeepsOnlyPagesNotWrittenBack(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 3*ps)
		mustNotFail(t, "create", err)
		supplyPagerVmoPages(t, env, vmo, 0, 2)
		mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, 'D'), 0))
		woken := make(chan error)
		go func() {
			_, err := vmo.GetPageBlocking(env.ctx, 2*ps, PfFlagSwFault)
			woken <- err
		}()
		synctest.Wait()
		vmo.DetachSource()
		expect(t, "the waiter fails on its retry", <-woken, ErrBadState)
		expect(t, "the dirty page is kept", vmo.DebugGetCowPages().DebugIsPage(0), true)
		expect(t, "the clean page is freed", vmo.DebugGetCowPages().DebugIsEmpty(ps), true)
		expectAttribution(t, "after the detach", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		splice := NewPageSpliceList[VmPage](ps, env.node)
		splice.Initialize(ps)
		splice.Finalize()
		expect(t, "no supply after the detach", vmo.SupplyPages(env.ctx, ps, ps, splice, PagerSupply), ErrBadState)
	})
}

// raceStrategy compresses as StoreAsIs does, but first calls race, with no
// lock held, as another thread meeting the compression would.
type raceStrategy struct {
	StoreAsIs
	race func()
}

func (s *raceStrategy) Compress(src, dst []byte, limit uint64) StrategyResult {
	if s.race != nil {
		race := s.race
		s.race = nil
		race()
	}
	return s.StoreAsIs.Compress(src, dst, limit)
}

// A lookup that meets a page while it is being compressed copies it back
// from the compressor, and the compression then reclaims nothing.
func TestALookupDuringACompressionTakesThePageBack(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		strategy := &raceStrategy{}
		storage, _ := env.newSpillStorage(t, testSpillPages)
		compression := NewCompression(env.pmm, ps, storage, strategy, ps)
		node := NewNode(env.pmm, ps, compression)
		vmo, err := CreateObjectPaged(node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, pattern(ps, 'Q'), 0))
		page := vmo.DebugGetPage(0)
		strategy.race = func() {
			expect(t, "the temporary reference", vmo.DebugGetCowPages().DebugIsReference(0), true)
			got := make([]byte, ps)
			mustNotFail(t, "read during the compression", vmo.Read(env.ctx, got, 0))
			expect(t, "the bytes read", bytes.Equal(got, pattern(ps, 'Q')), true)
		}
		guard := compression.AcquireCompressor()
		mustNotFail(t, "arm", guard.Get().Arm())
		result, failure := vmo.DebugGetCowPages().ReclaimPage(env.ctx, page, 0, FollowHint, guard.Get())
		guard.Release()
		expect(t, "no failure", failure, ReclaimSucceeded)
		expect(t, "nothing reclaimed", result.NumPages, uint64(0))
		expect(t, "a page again", vmo.DebugGetCowPages().DebugIsPage(0), true)
		expectAttribution(t, "kept", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expect(t, "the bytes kept", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'Q')), true)
	})
}

// A take that meets a page while it is being compressed takes it from the
// compressor's spare page.
func TestATakeDuringACompressionTakesThePageFromTheCompressor(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		strategy := &raceStrategy{}
		storage, _ := env.newSpillStorage(t, testSpillPages)
		compression := NewCompression(env.pmm, ps, storage, strategy, ps)
		node := NewNode(env.pmm, ps, compression)
		vmo, err := CreateObjectPaged(node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, pattern(ps, 'T'), 0))
		page := vmo.DebugGetPage(0)
		splice := NewPageSpliceList[VmPage](ps, node)
		strategy.race = func() {
			mustNotFail(t, "take during the compression", vmo.TakePages(env.ctx, 0, ps, splice))
		}
		guard := compression.AcquireCompressor()
		mustNotFail(t, "arm", guard.Get().Arm())
		result, failure := vmo.DebugGetCowPages().ReclaimPage(env.ctx, page, 0, FollowHint, guard.Get())
		guard.Release()
		expect(t, "no failure", failure, ReclaimSucceeded)
		expect(t, "nothing reclaimed", result.NumPages, uint64(0))
		expectAttribution(t, "taken", vmo.GetAttributedMemory(), AttributionCounts{})
		taken := splice.Pop()
		expect(t, "a page taken", taken.IsPage(), true)
		expect(t, "the bytes taken", bytes.Equal(taken.Page().data, pattern(ps, 'T')), true)
		node.FreePage(taken.ReleasePage())
	})
}

// A page that cannot be stored compressed stays, and leaves the reclaim
// queues so it is not tried again.
func TestAPageThatCannotBeStoredStaysAndIsNotTriedAgain(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		pq.EnableAnonymousReclaim(true)
		vmo, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, pattern(ps, 'F'), 0))
		page := vmo.DebugGetPage(0)
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "anonymous pages are reclaimable", ok, true)
		env.disk.FailNext(sim.DiskWrite, 1)
		guard := env.compression.AcquireCompressor()
		mustNotFail(t, "arm", guard.Get().Arm())
		_, failure := vmo.DebugGetCowPages().ReclaimPage(env.ctx, page, 0, FollowHint, guard.Get())
		guard.Release()
		expect(t, "the failure", failure, CompressFailedReclaim)
		expect(t, "the page stays", vmo.DebugGetPage(0), page)
		expect(t, "in the failed queue", pq.DebugPageIsFailedReclaim(page), true)
		expectAttribution(t, "kept", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
	})
}

// Zeroing a child puts markers over its own pages and over the gaps where it
// sees its parent, and leaves the parent as it was.
func TestZeroingAChildHidesItsParentWithMarkers(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		parent, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the parent", parent.Write(env.ctx, pattern(4*ps, 'P'), 0))
		child, err := parent.CreateClone(SnapshotOnWrite, 0, 4*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child", child.Write(env.ctx, pattern(ps, 'C'), ps))
		mapping := newTestMapping(t, env, child)
		defer mapping.unmap()
		mapping.fault(0, false)
		mustNotFail(t, "zero the child", child.ZeroRange(env.ctx, 0, 4*ps))
		_, _, mapped := mapping.query(0)
		expect(t, "the mapping of the parent's page is gone", mapped, false)
		got := make([]byte, 4*ps)
		mustNotFail(t, "read the child", child.Read(env.ctx, got, 0))
		expect(t, "the child reads zeros", allZero(got), true)
		for i := range uint64(4) {
			expect(t, "a marker", child.DebugGetCowPages().DebugIsMarker(i*ps), true)
		}
		expectAttribution(t, "the child holds nothing", child.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, child, 0)
		mustNotFail(t, "read the parent", parent.Read(env.ctx, got, 0))
		expect(t, "the parent is unchanged", bytes.Equal(got, pattern(4*ps, 'P')), true)
	})
}

// Zeroing partial pages zeroes only their bytes, and zeroing a root's whole
// pages frees them.
func TestZeroingPartsOfPagesZeroesOnlyThoseBytes(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 3*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, pattern(3*ps, 'Z'), 0))
		mustNotFail(t, "zero across a boundary", vmo.ZeroRange(env.ctx, ps-10, 20))
		got := make([]byte, 3*ps)
		mustNotFail(t, "read", vmo.Read(env.ctx, got, 0))
		want := pattern(3*ps, 'Z')
		clear(want[ps-10 : ps+10])
		expect(t, "only those bytes", bytes.Equal(got, want), true)
		mustNotFail(t, "zero the last page whole", vmo.ZeroRange(env.ctx, 2*ps, ps))
		expect(t, "the last page is freed", vmo.DebugGetCowPages().DebugIsEmpty(2*ps), true)
		expectAttribution(t, "two left", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expect(t, "zeroing past the end", vmo.ZeroRange(env.ctx, 3*ps, ps), ErrOutOfRange)
		expect(t, "untracked zeroing of part of a page", vmo.ZeroRangeUntracked(env.ctx, 1, ps), ErrInvalidArgs)
	})
}

// A decommit is refused where an empty slot does not read zero.
func TestADecommitIsRefusedWhereAnEmptySlotIsNotZero(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pager, _ := makeCommittedPagerVmo(t, env, 1, false)
		expect(t, "a pager's object", pager.DecommitRange(0, ps), ErrNotSupported)
		root, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", root.CommitRange(env.ctx, 0, 2*ps))
		child, err := root.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone", err)
		expect(t, "a child", child.DecommitRange(0, ps), ErrNotSupported)
		expect(t, "unaligned", root.DecommitRange(1, ps), ErrInvalidArgs)
		expect(t, "out of range", root.DecommitRange(0, 3*ps), ErrOutOfRange)
		expectNoError(t, "nothing", root.DecommitRange(ps, 0))
		mustNotFail(t, "a root", root.DecommitRange(0, ps))
		expectAttribution(t, "one left", root.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		// The child still reads the parent's page it had not copied.
		expect(t, "the child reads zeros now", allZero(readPage(t, env, child, 0)), true)
	})
}

// Taking pages from a child copies in what it sees of its parent first, and
// leaves markers so it then reads zeros.
func TestTakingFromAChildTakesWhatItSeesOfItsParent(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		parent, err := CreateObjectPaged(env.node, 3*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the parent", parent.Write(env.ctx, pattern(3*ps, 'P'), 0))
		child, err := parent.CreateClone(SnapshotOnWrite, 0, 3*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child's middle", child.Write(env.ctx, pattern(ps, 'C'), ps))
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", child.TakePages(env.ctx, 0, 3*ps, splice))
		for _, want := range []byte("PCP") {
			p := splice.Pop()
			expect(t, "a page", p.IsPage(), true)
			expect(t, "its bytes", bytes.Equal(p.Page().data, pattern(ps, want)), true)
			env.node.FreePage(p.ReleasePage())
		}
		got := make([]byte, 3*ps)
		mustNotFail(t, "read the child", child.Read(env.ctx, got, 0))
		expect(t, "zeros", allZero(got), true)
		mustNotFail(t, "read the parent", parent.Read(env.ctx, got, 0))
		expect(t, "the parent is unchanged", bytes.Equal(got, pattern(3*ps, 'P')), true)
	})
}

// A gap in what is supplied becomes a marker: zeros the pager supplied.
func TestAGapInASupplyIsAMarker(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 2, false)
		aux, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create aux", err)
		mustNotFail(t, "write aux", aux.Write(env.ctx, pattern(ps, 'A'), 0))
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, 2*ps, splice))
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, 2*ps, splice, PagerSupply))
		expect(t, "a page", vmo.DebugGetCowPages().DebugIsPage(0), true)
		expect(t, "a marker", vmo.DebugGetCowPages().DebugIsMarker(ps), true)
		expect(t, "zeros", allZero(readPage(t, env, vmo, ps)), true)
		// A pager may not transfer.
		splice2 := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take again", aux.TakePages(env.ctx, 0, ps, splice2))
		expect(t, "no transfer to a pager's object", vmo.SupplyPages(env.ctx, 0, ps, splice2, TransferData), ErrNotSupported)
		splice2.Free()
	})
}

// The lookup cursor's cheap walks: pages present as they are, and runs of
// absent ones.
func TestTheLookupCursorSkipsWhatIsMissingAndTakesWhatIsThere(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 6, false)
		pages := supplyPagerVmoPages(t, env, vmo, 0, 2)
		supplyPagerVmoPages(t, env, vmo, 4, 1)
		cow := vmo.DebugGetCowPages()
		cow.lock.Lock()
		defer cow.lock.Unlock()
		cursor, err := cow.GetLookupCursorLocked(CowRange{0, 6 * ps})
		mustNotFail(t, "cursor", err)
		defer cursor.Release()
		got := make([]*VmPage, 6)
		// Pages to be marked accessed are not taken this way.
		expect(t, "marked accessed", cursor.IfExistPages(false, 6, got), uint64(0))
		cursor.DisableMarkAccessed()
		expect(t, "present pages", cursor.IfExistPages(false, 6, got), uint64(2))
		expect(t, "the first", got[0], pages[0])
		expect(t, "the second", got[1], pages[1])
		expect(t, "missing pages", cursor.SkipMissingPages(), uint64(2))
		expect(t, "the page after them", cursor.MaybePage(false), cow.DebugGetPageLocked(4*ps))
		expect(t, "nothing present", cursor.IfExistPages(false, 1, got), uint64(0))
		expect(t, "the last missing", cursor.SkipMissingPages(), uint64(1))
		_, err = cow.GetLookupCursorLocked(CowRange{6 * ps, ps})
		expect(t, "a cursor past the end", err, ErrOutOfRange)
	})
}

// A region's cursor skips only the run its own source supplies, up to the
// next page an identity root holds.
func TestARegionsMissingRunEndsAtTheNextRootPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateIdentityRoot(env.node, NewPageSource(newRecordingProvider(false)), 4*ps)
		mustNotFail(t, "create the root", err)
		resolver := &staticResolver{ps: ps, root: root.DebugGetCowPages(), pages: map[uint64]uint64{3: 0}}
		regionProvider := newRecordingProvider(false)
		region, err := CreateRegionLayer(env.node, NewPageSource(regionProvider), 4*ps, resolver)
		mustNotFail(t, "create the region", err)
		cow := region.DebugGetCowPages()
		cow.lock.Lock()
		cursor, err := cow.GetLookupCursorLocked(CowRange{0, 4 * ps})
		mustNotFail(t, "cursor", err)
		expect(t, "the region's own run", cursor.SkipMissingPages(), uint64(3))
		// The root holds nothing at its page 0 either.
		expect(t, "the root's missing page", cursor.SkipMissingPages(), uint64(1))
		cursor.Release()
		cow.lock.Unlock()
		// A read of the region's start asks its source for the three pages
		// no root holds.
		deferred := NewDeferredOps(cow)
		cow.lock.Lock()
		cursor, err = cow.GetLookupCursorLocked(CowRange{0, 4 * ps})
		mustNotFail(t, "cursor again", err)
		request := NewMultiPageRequest()
		_, err = cursor.RequireReadPage(env.ctx, 4, deferred, request)
		cursor.Release()
		cow.lock.Unlock()
		deferred.Finish()
		expect(t, "a read waits", err, ErrShouldWait)
		expect(t, "the region's requests", len(regionProvider.requests), 1)
		expect(t, "the region's request", regionProvider.requests[0], CowRange{0, 3 * ps})
		request.CancelRequests()
	})
}

// The isolate queue is filled from the oldest reclaim queues when a peek
// finds it empty, never from the active ones.
func TestAPeekIsolatesThePagesOfTheOldestQueues(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 2, false)
		expect(t, "both active", pq.GetActiveInactiveCounts(), ActiveInactiveCounts{Active: 2})
		_, ok := pq.PeekIsolate(NumActiveQueues)
		expect(t, "nothing to peek while active", ok, false)
		for range 3 {
			pq.RotateReclaimQueues()
		}
		expect(t, "both inactive", pq.GetActiveInactiveCounts(), ActiveInactiveCounts{Inactive: 2})
		backlink, ok := pq.PeekIsolate(NumActiveQueues)
		expect(t, "a page to peek", ok, true)
		expect(t, "the object", backlink.Cow, vmo.DebugGetCowPages())
		// The oldest goes first: the first page supplied.
		expect(t, "its first page", backlink.Page, pages[0])
		expect(t, "its offset", backlink.Offset, uint64(0))
		expect(t, "both isolated", pq.QueueCounts().ReclaimIsolate, 2)
	})
}

// An object that is destroyed while a child reads it stays for the child,
// and every page goes back once both are gone.
func TestAParentLetGoStaysForItsChildAndAllPagesGoBackAfter(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		before := env.pmm.out
		parent, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", parent.Write(env.ctx, pattern(2*ps, 'P'), 0))
		child, err := parent.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child", child.Write(env.ctx, pattern(ps, 'C'), 0))
		parent.Destroy()
		expect(t, "the child still reads its parent", bytes.Equal(readPage(t, env, child, ps), pattern(ps, 'P')), true)
		expect(t, "the parent's pages stay", env.pmm.out, before+3)
		child.Destroy()
		expect(t, "every page is back", env.pmm.out, before)
	})
}

// A pager's stats say whether an object was written, until reset.
func TestThePagerStatsSayWhetherAnObjectWasWritten(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		modified, err := vmo.QueryPagerVmoStats(false)
		mustNotFail(t, "query", err)
		expect(t, "not modified", modified, false)
		mustNotFail(t, "write", vmo.Write(env.ctx, []byte{1}, 0))
		modified, err = vmo.QueryPagerVmoStats(true)
		mustNotFail(t, "query and reset", err)
		expect(t, "modified", modified, true)
		modified, err = vmo.QueryPagerVmoStats(false)
		mustNotFail(t, "query again", err)
		expect(t, "reset", modified, false)
		anon, err := CreateObjectPaged(env.node, env.ps)
		mustNotFail(t, "create", err)
		_, err = anon.QueryPagerVmoStats(false)
		expect(t, "an anonymous object", err, ErrNotSupported)
		expect(t, "no dirty ranges of an anonymous object", anon.EnumerateDirtyRanges(0, env.ps, nil), ErrNotSupported)
		expect(t, "no writeback of an anonymous object", anon.WritebackProtect(0, env.ps), ErrNotSupported)
	})
}

// The pause's range protection makes a mapping read-only, so the next store
// faults.
func TestThePausesProtectionMakesMappingsReadOnly(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		mapping.fault(0, true)
		_, writable, _ := mapping.query(0)
		expect(t, "writable", writable, true)
		mustNotFail(t, "protect", vmo.WritebackProtect(0, env.ps))
		_, writable, mapped := mapping.query(0)
		expect(t, "still mapped", mapped, true)
		expect(t, "read-only", writable, false)
		expect(t, "out of range", vmo.WritebackProtect(0, 2*env.ps), ErrOutOfRange)
	})
}

// supplyZeros supplies numPages of zeros from pageOffset, which a pager's
// object holds as markers.
func supplyZeros(t *testing.T, env *vmoEnv, vmo *ObjectPaged, pageOffset, numPages uint64) {
	t.Helper()
	aux, err := CreateObjectPaged(env.node, numPages*env.ps)
	mustNotFail(t, "create aux", err)
	splice := NewPageSpliceList[VmPage](env.ps, env.node)
	mustNotFail(t, "take zeros", aux.TakePages(env.ctx, 0, numPages*env.ps, splice))
	mustNotFail(t, "supply zeros", vmo.SupplyPages(env.ctx, pageOffset*env.ps, numPages*env.ps, splice, PagerSupply))
}

// A store into an object whose pager traps dirty transitions waits for the
// pager to agree, for pages, zero markers and zero intervals alike.
func TestAStoreWaitsForAPagerThatTrapsToAgree(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(true)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 4*ps)
		mustNotFail(t, "create", err)
		supplyPagerVmoPages(t, env, vmo, 0, 2)
		supplyZeros(t, env, vmo, 2, 2)
		expect(t, "a marker", vmo.DebugGetCowPages().DebugIsMarker(2*ps), true)
		stored := make(chan error)
		go func() { stored <- vmo.Write(env.ctx, pattern(4*ps, 'W'), 0) }()
		synctest.Wait()
		expect(t, "one request", len(provider.requests), 1)
		expect(t, "a dirty request", provider.types[0], DirtyRequest)
		expect(t, "of the pages and the markers", provider.requests[0], CowRange{0, 4 * ps})
		mustNotFail(t, "agree", vmo.DirtyPages(env.ctx, 0, 4*ps))
		expectNoError(t, "the store", <-stored)
		expect(t, "stored", bytes.Equal(readPage(t, env, vmo, 3*ps), pattern(ps, 'W')), true)
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 4, 0}})
		expectAttribution(t, "four pages", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		// Zeroed, the range is a dirty zero interval.
		mustNotFail(t, "zero", vmo.ZeroRange(env.ctx, 0, 4*ps))
		expectRanges(t, "zeros", dirtyRanges(t, env, vmo), [][3]uint64{{0, 4, 1}})
		expectAttribution(t, "no pages", vmo.GetAttributedMemory(), AttributionCounts{})
		// A store into the interval's middle asks for that page alone.
		go func() { stored <- vmo.Write(env.ctx, []byte{'M'}, 2*ps) }()
		synctest.Wait()
		expect(t, "a second request", len(provider.requests), 2)
		expect(t, "of the page in the interval", provider.requests[1], CowRange{2 * ps, ps})
		mustNotFail(t, "agree to the page", vmo.DirtyPages(env.ctx, 2*ps, ps))
		expectNoError(t, "the store into the interval", <-stored)
		expectRanges(t, "split", dirtyRanges(t, env, vmo), [][3]uint64{{0, 2, 1}, {2, 1, 0}, {3, 1, 1}})
		got := readPage(t, env, vmo, 2*ps)
		want := make([]byte, ps)
		want[0] = 'M'
		expect(t, "the page stored", bytes.Equal(got, want), true)
		// What is not the pager's to agree to is refused.
		expect(t, "out of range", vmo.DirtyPages(env.ctx, 0, 5*ps), ErrOutOfRange)
		other := makeUncommittedPagerVmo(t, env, 1, true)
		expect(t, "a gap", other.DirtyPages(env.ctx, 0, ps), ErrNotFound)
		plain, _ := makeCommittedPagerVmo(t, env, 1, false)
		expect(t, "a pager that does not trap", plain.DirtyPages(env.ctx, 0, ps), ErrNotSupported)
	})
}

// Zeroing a pager's object makes zero intervals in the state asked for,
// changing the state of intervals it covers from their ends.
func TestZeroingAPagersObjectMakesIntervalsInTheStateAskedFor(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 6, false)
		mustNotFail(t, "zero untracked", vmo.ZeroRangeUntracked(env.ctx, 0, 6*ps))
		expectRanges(t, "untracked zeros are not dirty", dirtyRanges(t, env, vmo), nil)
		// Zircon does not split an interval just to change the state of its
		// middle.
		mustNotFail(t, "zero the middle", vmo.ZeroRange(env.ctx, 2*ps, 2*ps))
		expectRanges(t, "the middle unchanged", dirtyRanges(t, env, vmo), nil)
		mustNotFail(t, "zero all", vmo.ZeroRange(env.ctx, 0, 6*ps))
		expectRanges(t, "all dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 6, 1}})
		mustNotFail(t, "zero the end untracked", vmo.ZeroRangeUntracked(env.ctx, 4*ps, 2*ps))
		expectRanges(t, "the start dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 4, 1}})
		mustNotFail(t, "zero the start untracked", vmo.ZeroRangeUntracked(env.ctx, 0, 2*ps))
		expectRanges(t, "the middle dirty", dirtyRanges(t, env, vmo), [][3]uint64{{2, 2, 1}})
		// Pages become an interval. Markers, zeros the pager supplied, stay
		// clean zeros, and become an interval only when not dirty tracking.
		pages := makeUncommittedPagerVmo(t, env, 4, false)
		supplyPagerVmoPages(t, env, pages, 0, 2)
		supplyZeros(t, env, pages, 2, 2)
		mustNotFail(t, "zero pages and markers", pages.ZeroRange(env.ctx, 0, 4*ps))
		expectRanges(t, "one interval", dirtyRanges(t, env, pages), [][3]uint64{{0, 2, 1}})
		expect(t, "a marker stays", pages.DebugGetCowPages().DebugIsMarker(3*ps), true)
		mustNotFail(t, "zero the markers untracked", pages.ZeroRangeUntracked(env.ctx, 2*ps, 2*ps))
		expect(t, "the marker went", pages.DebugGetCowPages().DebugIsMarker(3*ps), false)
		end := pages.DebugGetCowPages().pageList.Lookup(3 * ps)
		expect(t, "an interval ends there", end.IsIntervalEnd(), true)
		expect(t, "an untracked interval", end.IsZeroIntervalUntracked(), true)
		expectAttribution(t, "no pages", pages.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, pages, 0)
		expect(t, "zeros read", allZero(readPage(t, env, pages, ps)), true)
	})
}

// A range's helpers keep their ends: trimming, covering, bounding and
// rounding out to pages.
func TestARangesHelpersKeepItsEnds(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		expect(t, "trimmed", CowRange{ps, 3 * ps}.TrimmedFromStart(ps), CowRange{2 * ps, 2 * ps})
		expect(t, "covered", CowRange{3 * ps, ps}.Cover(CowRange{ps, ps}), CowRange{ps, 3 * ps})
		expect(t, "covered the other way", CowRange{ps, ps}.Cover(CowRange{3 * ps, ps}), CowRange{ps, 3 * ps})
		expect(t, "an empty range covers nothing", CowRange{}.Cover(CowRange{ps, ps}), CowRange{ps, ps})
		expect(t, "nothing to cover", CowRange{ps, ps}.Cover(CowRange{5 * ps, 0}), CowRange{ps, ps})
		expect(t, "ends at the limit", CowRange{ps, ps}.IsBoundedBy(2*ps), true)
		expect(t, "an empty range at the limit", CowRange{2 * ps, 0}.IsBoundedBy(2*ps), true)
		expect(t, "ends past the limit", CowRange{ps, ps + 1}.IsBoundedBy(2*ps), false)
		expect(t, "starts past the limit", CowRange{2*ps + 1, 0}.IsBoundedBy(2*ps), false)
		expect(t, "overflows", CowRange{ps, ^uint64(0)}.IsBoundedBy(^uint64(0)), false)
		pages, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		expect(t, "rounded out", pages.DebugGetCowPages().expandTillPageAligned(CowRange{ps + 1, ps}), CowRange{ps, 2 * ps})
		expect(t, "aligned stays", pages.DebugGetCowPages().expandTillPageAligned(CowRange{ps, ps}), CowRange{ps, ps})
	})
}

// A clone hangs from the nearest object above that holds content in its
// range, so it sees what it would hanging from the object it was made from.
func TestACloneHangsFromTheNearestObjectWithContentInItsRange(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateObjectPaged(env.node, 3*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the root", root.Write(env.ctx, pattern(3*ps, 'R'), 0))
		child, err := root.CreateClone(SnapshotOnWrite, 0, 3*ps)
		mustNotFail(t, "clone", err)
		empty, err := child.CreateClone(SnapshotOnWrite, ps, ps)
		mustNotFail(t, "clone the empty child", err)
		expect(t, "an empty child is skipped", empty.DebugGetCowPages().DebugGetParent(), root.DebugGetCowPages())
		mustNotFail(t, "write the child", child.Write(env.ctx, pattern(ps, 'C'), ps))
		owner, err := child.CreateClone(SnapshotOnWrite, ps, ps)
		mustNotFail(t, "clone where the child has content", err)
		expect(t, "hangs from the child", owner.DebugGetCowPages().DebugGetParent(), child.DebugGetCowPages())
		expect(t, "reads the child", bytes.Equal(readPage(t, env, owner, 0), pattern(ps, 'C')), true)
		past, err := child.CreateClone(SnapshotOnWrite, 2*ps, ps)
		mustNotFail(t, "clone past the child's content", err)
		expect(t, "hangs from the root", past.DebugGetCowPages().DebugGetParent(), root.DebugGetCowPages())
		expect(t, "reads the root", bytes.Equal(readPage(t, env, past, 0), pattern(ps, 'R')), true)
		none, err := child.CreateClone(SnapshotOnWrite, 3*ps, 0)
		mustNotFail(t, "an empty clone", err)
		expect(t, "an empty clone has no size", none.Size(), uint64(0))
	})
}

// Taking from a child at an offset into its parent leaves a marker where the
// parent may have content to hide, and an empty slot elsewhere. As Zircon's,
// the parent "may have content" wherever its page list has a node, so the
// test puts the parent's content in a node of its own.
func TestTakingFromAChildLeavesMarkersOnlyOverItsParentsContent(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		parent, err := CreateObjectPaged(env.node, 18*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the parent's second node", parent.Write(env.ctx, pattern(2*ps, 'P'), 16*ps))
		child, err := parent.CreateClone(SnapshotOnWrite, ps, 17*ps)
		mustNotFail(t, "clone", err)
		cow := child.DebugGetCowPages()
		for _, off := range []uint64{0, 15 * ps} {
			mustNotFail(t, "write the child", child.Write(env.ctx, pattern(ps, 'C'), off))
			splice := NewPageSpliceList[VmPage](ps, env.node)
			mustNotFail(t, "take", child.TakePages(env.ctx, off, ps, splice))
			p := splice.Pop()
			expect(t, "a page", p.IsPage(), true)
			expect(t, "its bytes", bytes.Equal(p.Page().data, pattern(ps, 'C')), true)
			env.node.FreePage(p.ReleasePage())
			got := make([]byte, ps)
			mustNotFail(t, "read the child", child.Read(env.ctx, got, off))
			expect(t, "zeros", allZero(got), true)
		}
		expect(t, "nothing above the first", cow.DebugIsEmpty(0), true)
		expect(t, "the sixteenth hides the parent", cow.DebugIsMarker(15*ps), true)
	})
}

// lookupReadable lists the offsets and pages a readable lookup of r reports.
func lookupReadable(t *testing.T, vmo *ObjectPaged, r CowRange) ([]uint64, []*VmPage) {
	t.Helper()
	var offsets []uint64
	var pages []*VmPage
	err := vmo.DebugGetCowPages().DebugLookupReadable(r, func(offset uint64, page *VmPage) error {
		offsets = append(offsets, offset)
		pages = append(pages, page)
		return nil
	})
	mustNotFail(t, "lookup", err)
	return offsets, pages
}

// A readable lookup of a clone reports the pages it reads from each object
// above it, at each one's offset, and no further than its range. An error
// from the callback ends the lookup.
func TestAReadableLookupReportsThePagesOfEachOwnerAbove(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateObjectPaged(env.node, 5*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the root", root.Write(env.ctx, pattern(5*ps, 'R'), 0))
		child, err := root.CreateClone(SnapshotOnWrite, ps, 4*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child's last", child.Write(env.ctx, pattern(ps, 'C'), 3*ps))
		grandchild, err := child.CreateClone(SnapshotOnWrite, ps, 3*ps)
		mustNotFail(t, "clone the child", err)
		expect(t, "hangs from the child", grandchild.DebugGetCowPages().DebugGetParent(), child.DebugGetCowPages())
		offsets, pages := lookupReadable(t, grandchild, CowRange{0, 3 * ps})
		expect(t, "three pages", len(offsets), 3)
		expect(t, "the root's third", offsets[0], uint64(0))
		expect(t, "is the root's", pages[0], root.DebugGetPage(2*ps))
		expect(t, "the root's fourth", offsets[1], ps)
		expect(t, "is the root's", pages[1], root.DebugGetPage(3*ps))
		expect(t, "the child's last", offsets[2], 2*ps)
		expect(t, "is the child's", pages[2], child.DebugGetPage(3*ps))
		// No further than the range, past a page of the object's own.
		mustNotFail(t, "write the grandchild's first", grandchild.Write(env.ctx, pattern(ps, 'G'), 0))
		offsets, pages = lookupReadable(t, grandchild, CowRange{0, 2 * ps})
		expect(t, "two pages", len(offsets), 2)
		expect(t, "its own", pages[0], grandchild.DebugGetPage(0))
		expect(t, "the root's fourth again", offsets[1], ps)
		expect(t, "is the root's again", pages[1], root.DebugGetPage(3*ps))
		// An error from the callback ends the lookup, at the object's own
		// page and at its parent's alike.
		for _, off := range []uint64{0, ps} {
			calls := 0
			err = grandchild.DebugGetCowPages().DebugLookupReadable(CowRange{off, 2 * ps}, func(uint64, *VmPage) error {
				calls++
				return ErrIO
			})
			expect(t, "the callback's error", err, ErrIO)
			expect(t, "one call", calls, 1)
		}
	})
}

// As Zircon's, a readable lookup reads the rest of the range from the first
// owner above that has content, so a page that owner does not hold, but an
// object above it does, is not reported.
func TestAReadableLookupReadsTheRestFromTheFirstOwnerWithContent(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateObjectPaged(env.node, 5*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the root", root.Write(env.ctx, pattern(5*ps, 'R'), 0))
		child, err := root.CreateClone(SnapshotOnWrite, ps, 4*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child's third", child.Write(env.ctx, pattern(ps, 'C'), 2*ps))
		grandchild, err := child.CreateClone(SnapshotOnWrite, 0, 4*ps)
		mustNotFail(t, "clone the child", err)
		offsets, pages := lookupReadable(t, grandchild, CowRange{0, 4 * ps})
		expect(t, "three pages, not four", len(offsets), 3)
		expect(t, "the root's second", offsets[0], uint64(0))
		expect(t, "is the root's", pages[0], root.DebugGetPage(ps))
		expect(t, "the root's third", offsets[1], ps)
		expect(t, "is the root's", pages[1], root.DebugGetPage(2*ps))
		expect(t, "the child's third", offsets[2], 2*ps)
		expect(t, "is the child's", pages[2], child.DebugGetPage(2*ps))
	})
}

// The pager's agreement to dirty a range wakes only the stores waiting in
// that range, for pages and zero markers alike.
func TestAnAgreementWakesOnlyTheStoresInItsRange(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(true)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 6*ps)
		mustNotFail(t, "create", err)
		supplyPagerVmoPages(t, env, vmo, 0, 3)
		supplyZeros(t, env, vmo, 3, 3)
		for _, store := range []struct {
			what       string
			offset     uint64
			neighbours [2]uint64
		}{
			{"a page", 2 * ps, [2]uint64{ps, 3 * ps}},
			{"a marker", 5 * ps, [2]uint64{4 * ps, 4 * ps}},
		} {
			sent := len(provider.requests)
			stored := make(chan error)
			go func() { stored <- vmo.Write(env.ctx, []byte{'W'}, store.offset) }()
			synctest.Wait()
			expect(t, "a request", len(provider.requests), sent+1)
			expect(t, "of the store's page", provider.requests[sent], CowRange{store.offset, ps})
			for _, off := range store.neighbours {
				mustNotFail(t, "agree to a neighbour", vmo.DirtyPages(env.ctx, off, ps))
			}
			synctest.Wait()
			expect(t, "no request again", len(provider.requests), sent+1)
			select {
			case err := <-stored:
				t.Fatalf("%s: the store did not wait: %v", store.what, err)
			default:
			}
			mustNotFail(t, "agree to the store's page", vmo.DirtyPages(env.ctx, store.offset, ps))
			expectNoError(t, "the store", <-stored)
		}
	})
}

// An agreement that fails on a gap wakes the stores waiting in its range to
// look again, and only those.
func TestAFailedAgreementWakesOnlyTheStoresInItsRange(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(true)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 4*ps)
		mustNotFail(t, "create", err)
		supplyPagerVmoPages(t, env, vmo, 0, 2)
		supplyPagerVmoPages(t, env, vmo, 3, 1)
		inRange := make(chan error)
		go func() { inRange <- vmo.Write(env.ctx, []byte{'A'}, ps) }()
		synctest.Wait()
		past := make(chan error)
		go func() { past <- vmo.Write(env.ctx, []byte{'B'}, 3*ps) }()
		synctest.Wait()
		expect(t, "two requests", len(provider.requests), 2)
		expect(t, "the second page's", provider.requests[0], CowRange{ps, ps})
		expect(t, "the fourth page's", provider.requests[1], CowRange{3 * ps, ps})
		expect(t, "a gap", vmo.DirtyPages(env.ctx, ps, 2*ps), ErrNotFound)
		synctest.Wait()
		// The store in range looked again and asked again; the other did not.
		expect(t, "three requests", len(provider.requests), 3)
		expect(t, "the second page's again", provider.requests[2], CowRange{ps, ps})
		mustNotFail(t, "agree to the second", vmo.DirtyPages(env.ctx, ps, ps))
		expectNoError(t, "the store in range", <-inRange)
		mustNotFail(t, "agree to the fourth", vmo.DirtyPages(env.ctx, 3*ps, ps))
		expectNoError(t, "the store past it", <-past)
		expect(t, "no more requests", len(provider.requests), 3)
	})
}

// An object's calls round ranges out to pages, trim them to the object, or
// refuse them, as each asks.
func TestAnObjectsCallsRoundTrimOrRefuseTheirRanges(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, pattern(4*ps, 'A'), 0))
		expectAttribution(t, "trimmed to the object", vmo.GetAttributedMemoryInRange(ps, 10*ps), PrivateAttributionCounts(3*ps, 0))
		expectAttribution(t, "rounded out to pages", vmo.GetAttributedMemoryInRange(ps+1, ps), PrivateAttributionCounts(2*ps, 0))
		expectAttribution(t, "past the end", vmo.GetAttributedMemoryInRange(4*ps, ps), AttributionCounts{})
		counts := PrivateAttributionCounts(2*ps, ps)
		expect(t, "all bytes", counts.TotalBytes(), 3*ps)
		expect(t, "private bytes", counts.TotalPrivateBytes(), 3*ps)
		// A partial zero of a page past the first.
		mustNotFail(t, "zero two bytes", vmo.ZeroRange(env.ctx, ps+1, 2))
		want := pattern(ps, 'A')
		want[1], want[2] = 0, 0
		expect(t, "two bytes zeroed", bytes.Equal(readPage(t, env, vmo, ps), want), true)
		expect(t, "a partial zero past the end", vmo.ZeroRange(env.ctx, 4*ps+1, 1), ErrOutOfRange)
		expect(t, "untracked zeros of a part page", vmo.ZeroRangeUntracked(env.ctx, 1, ps), ErrInvalidArgs)
		expect(t, "untracked zeros of a part length", vmo.ZeroRangeUntracked(env.ctx, 0, 1), ErrInvalidArgs)
		// A prefetch decompresses the pages its range touches.
		cow := vmo.DebugGetCowPages()
		for i := range uint64(4) {
			expect(t, "compressed", compressPage(t, env, cow, vmo.DebugGetPage(i*ps), i*ps), uint64(1))
		}
		mustNotFail(t, "prefetch", vmo.PrefetchRange(env.ctx, ps+1, ps))
		for i, want := range []bool{true, false, false, true} {
			expect(t, "still compressed outside the prefetch", cow.DebugIsReference(uint64(i)*ps), want)
		}
		// Zeroing a pager's page modifies the object.
		pager, _ := makeCommittedPagerVmo(t, env, 1, false)
		modified, err := pager.QueryPagerVmoStats(true)
		mustNotFail(t, "query", err)
		expect(t, "not modified", modified, false)
		mustNotFail(t, "zero the pager's page", pager.ZeroRange(env.ctx, 0, ps))
		modified, err = pager.QueryPagerVmoStats(false)
		mustNotFail(t, "query again", err)
		expect(t, "modified", modified, true)
	})
}

// Zeroing part of a pager's page that reads as zeros already stores
// nothing, so dirties nothing: a marker, or a page in a zero interval.
func TestZeroingPartOfAZeroPageDirtiesNothing(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 2, false)
		supplyZeros(t, env, vmo, 0, 1)
		mustNotFail(t, "zero untracked", vmo.ZeroRangeUntracked(env.ctx, ps, ps))
		for _, off := range []uint64{1, ps + 1} {
			mustNotFail(t, "zero two bytes", vmo.ZeroRange(env.ctx, off, 2))
		}
		expectRanges(t, "nothing dirty", dirtyRanges(t, env, vmo), nil)
		expect(t, "the marker stays", vmo.DebugGetCowPages().DebugIsMarker(0), true)
		expectAttribution(t, "no pages", vmo.GetAttributedMemory(), AttributionCounts{})
	})
}

// Zeroing a gap ends the reads waiting in it, and only those.
func TestZeroingAGapEndsOnlyTheReadsWaitingInIt(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 4*ps)
		mustNotFail(t, "create", err)
		supplyPagerVmoPages(t, env, vmo, 0, 1)
		supplyPagerVmoPages(t, env, vmo, 2, 1)
		inGap := make(chan []byte)
		go func() { inGap <- readPage(t, env, vmo, ps) }()
		synctest.Wait()
		past := make(chan []byte)
		go func() { past <- readPage(t, env, vmo, 3*ps) }()
		synctest.Wait()
		expect(t, "two requests", len(provider.requests), 2)
		expect(t, "the gap's", provider.requests[0], CowRange{ps, ps})
		expect(t, "the last page's", provider.requests[1], CowRange{3 * ps, ps})
		mustNotFail(t, "zero the gap", vmo.ZeroRange(env.ctx, ps, ps))
		expect(t, "the read in the gap reads zeros", allZero(<-inGap), true)
		synctest.Wait()
		expect(t, "the other read did not look again", len(provider.requests), 2)
		supplyPagerVmoPages(t, env, vmo, 3, 1)
		expect(t, "the other read", bytes.Equal(<-past, vmo.DebugGetPage(3*ps).data), true)
	})
}

// A supply around a page already there keeps that page and ends the reads
// waiting on either side of it.
func TestASupplyAroundAPageAlreadyThereEndsTheReadsOnBothSides(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 3*ps)
		mustNotFail(t, "create", err)
		there := supplyPagerVmoPages(t, env, vmo, 1, 1)[0]
		reads := make([]chan []byte, 2)
		for i, off := range []uint64{0, 2 * ps} {
			reads[i] = make(chan []byte)
			go func() { reads[i] <- readPage(t, env, vmo, off) }()
			synctest.Wait()
		}
		expect(t, "two requests", len(provider.requests), 2)
		aux, err := CreateObjectPaged(env.node, 3*ps)
		mustNotFail(t, "create aux", err)
		mustNotFail(t, "write aux", aux.Write(env.ctx, pattern(3*ps, 'S'), 0))
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, 3*ps, splice))
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, 3*ps, splice, PagerSupply))
		for i := range reads {
			expect(t, "a read on either side", bytes.Equal(<-reads[i], pattern(ps, 'S')), true)
		}
		expect(t, "the page there stays", vmo.DebugGetPage(ps), there)
		synctest.Wait()
		expect(t, "no request again", len(provider.requests), 2)
	})
}

// A commit and a prefetch ask the pager for the pages of their range.
func TestACommitAndAPrefetchAskForTheirRange(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		provider := newWaitingProvider(false)
		vmo, err := CreateExternal(env.node, NewPageSource(provider), 4*ps)
		mustNotFail(t, "create", err)
		committed := make(chan error)
		go func() { committed <- vmo.CommitRange(env.ctx, ps, 2*ps) }()
		synctest.Wait()
		expect(t, "a request", len(provider.requests), 1)
		expect(t, "of the commit's range", provider.requests[0], CowRange{ps, 2 * ps})
		supplyPagerVmoPages(t, env, vmo, 1, 2)
		expectNoError(t, "the commit", <-committed)
		prefetched := make(chan error)
		go func() { prefetched <- vmo.PrefetchRange(env.ctx, 3*ps+1, ps-1) }()
		synctest.Wait()
		expect(t, "a second request", len(provider.requests), 2)
		expect(t, "of the prefetch's page", provider.requests[1], CowRange{3 * ps, ps})
		supplyPagerVmoPages(t, env, vmo, 3, 1)
		expectNoError(t, "the prefetch", <-prefetched)
	})
}

// Taking from the middle of an object takes its pages in order and leaves
// the rest.
func TestTakingFromTheMiddleTakesItsPagesInOrder(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		for i, b := range []byte("ABCD") {
			mustNotFail(t, "write", vmo.Write(env.ctx, pattern(ps, b), uint64(i)*ps))
		}
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", vmo.TakePages(env.ctx, ps, 2*ps, splice))
		for _, want := range []byte("BC") {
			p := splice.Pop()
			expect(t, "a page", p.IsPage(), true)
			expect(t, "in order", bytes.Equal(p.Page().data, pattern(ps, want)), true)
			env.node.FreePage(p.ReleasePage())
		}
		expect(t, "the first stays", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'A')), true)
		expect(t, "the second is zero", allZero(readPage(t, env, vmo, ps)), true)
		expect(t, "the last stays", bytes.Equal(readPage(t, env, vmo, 3*ps), pattern(ps, 'D')), true)
	})
}

// A page the cursor finds is writable only when a store into it needs
// nothing more: in the target, and Dirty if the target is dirty tracked.
func TestACursorsPageIsWritableOnlyWhenAStoreNeedsNothingMore(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		require := func(vmo *ObjectPaged) bool {
			t.Helper()
			cow := vmo.DebugGetCowPages()
			deferred := NewDeferredOps(cow)
			defer deferred.Finish()
			cow.lock.Lock()
			defer cow.lock.Unlock()
			cursor, err := cow.GetLookupCursorLocked(CowRange{0, ps})
			mustNotFail(t, "cursor", err)
			defer cursor.Release()
			result, err := cursor.RequirePage(env.ctx, false, 1, deferred, NewMultiPageRequest())
			mustNotFail(t, "require", err)
			return result.Writable
		}
		pager, _ := makeCommittedPagerVmo(t, env, 1, false)
		expect(t, "a clean page", require(pager), false)
		mustNotFail(t, "store", pager.Write(env.ctx, []byte{1}, 0))
		expect(t, "a dirty page", require(pager), true)
		anon, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "store into the anonymous object", anon.Write(env.ctx, []byte{1}, 0))
		expect(t, "an anonymous page", require(anon), true)
		clone, err := anon.CreateClone(SnapshotOnWrite, 0, ps)
		mustNotFail(t, "clone", err)
		expect(t, "a parent's page", require(clone), false)
	})
}

// Eviction takes the node aligned run around the page, and a Clean page is
// evicted, not spilled, even with a compressor at hand. A zero page found
// is replaced, which counts as a reclamation.
func TestEvictionTakesTheNodeAroundAPageAndSpillsNoCleanPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, pages := makeCommittedPagerVmo(t, env, 2*NodePages, false)
		cow := vmo.DebugGetCowPages()
		// Every page is isolated, as the reclamation thread would.
		for _, page := range pages {
			env.node.PageQueues().MoveToReclaimDontNeed(page)
		}
		guard := env.compression.AcquireCompressor()
		compressor := guard.Get()
		mustNotFail(t, "arm", compressor.Arm())
		evicted := reclaimCow(env.ctx, cow, pages[NodePages+1], (NodePages+1)*ps, IgnoreHint, compressor)
		guard.Release()
		expect(t, "the second node evicted", evicted, uint64(NodePages))
		for i := range uint64(2 * NodePages) {
			expect(t, "only the second node is empty", cow.DebugIsEmpty(i*ps), i >= NodePages)
		}
		anon, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", anon.CommitRange(env.ctx, 0, ps))
		events := anon.ReclamationEventCount()
		expect(t, "deduplicated", anon.DebugGetCowPages().DedupZeroPage(anon.DebugGetPage(0), 0), true)
		expect(t, "a reclamation counted", anon.ReclamationEventCount(), events+1)
	})
}

// A page a root lets go is unmapped from every clone that sees it, at each
// clone's own offset, and only there.
func TestAPageLetGoIsUnmappedFromTheClonesThatSeeIt(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the root", root.Write(env.ctx, pattern(4*ps, 'R'), 0))
		child, err := root.CreateClone(SnapshotOnWrite, ps, 3*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child's second", child.Write(env.ctx, pattern(ps, 'C'), ps))
		grandchild, err := child.CreateClone(SnapshotOnWrite, ps, 2*ps)
		mustNotFail(t, "clone the child", err)
		expect(t, "hangs from the child", grandchild.DebugGetCowPages().DebugGetParent(), child.DebugGetCowPages())
		childMap := newTestMapping(t, env, child)
		defer childMap.unmap()
		childMap.commitAndMap(false)
		grandMap := newTestMapping(t, env, grandchild)
		defer grandMap.unmap()
		grandMap.commitAndMap(false)
		// The root's last page goes: the child sees it at its third page, the
		// grandchild at its second.
		mustNotFail(t, "decommit the root's last", root.DecommitRange(3*ps, ps))
		for i, want := range []bool{true, true, false} {
			_, _, mapped := childMap.query(uint64(i) * ps)
			expect(t, "the child's mappings", mapped, want)
		}
		for i, want := range []bool{true, false} {
			_, _, mapped := grandMap.query(uint64(i) * ps)
			expect(t, "the grandchild's mappings", mapped, want)
		}
		expect(t, "the child reads zeros", allZero(readPage(t, env, child, 2*ps)), true)
		expect(t, "the grandchild reads zeros", allZero(readPage(t, env, grandchild, ps)), true)
		expect(t, "the grandchild reads the child", bytes.Equal(readPage(t, env, grandchild, 0), pattern(ps, 'C')), true)
	})
}

// Zeroing a child over the end of what it sees of its parent zeroes the
// part that sees the parent too. Zircon takes such a gap as zero already and
// leaves the parent showing; the port departs there (dirty.go).
func TestZeroingAChildPastItsParentsEndZeroesWhatItSaw(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		parent, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the parent", parent.Write(env.ctx, pattern(2*ps, 'P'), 0))
		child, err := parent.CreateClone(SnapshotOnWrite, 0, 4*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "zero across the parent's end", child.ZeroRange(env.ctx, ps, 2*ps))
		expect(t, "the first still the parent's", bytes.Equal(readPage(t, env, child, 0), pattern(ps, 'P')), true)
		expect(t, "the second zero", allZero(readPage(t, env, child, ps)), true)
		expect(t, "the parent unchanged", bytes.Equal(readPage(t, env, parent, ps), pattern(ps, 'P')), true)
	})
}

// A store that forks the zero page unmaps the zero page from the object's
// mappings, and a store that copies a parent's page unmaps that page from
// the clones that saw it through the object.
func TestAStoreUnmapsThePageItReplaces(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		anon, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		anonMap := newTestMapping(t, env, anon)
		defer anonMap.unmap()
		anonMap.fault(0, false)
		zero, _, _ := anonMap.query(0)
		expect(t, "the zero page mapped", zero, env.node.pmm.ZeroPage())
		mustNotFail(t, "store", anon.Write(env.ctx, []byte{1}, 0))
		_, _, mapped := anonMap.query(0)
		expect(t, "the zero page unmapped", mapped, false)

		root, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create the root", err)
		mustNotFail(t, "write the root", root.Write(env.ctx, pattern(2*ps, 'R'), 0))
		child, err := root.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write the child's second", child.Write(env.ctx, pattern(ps, 'C'), ps))
		grandchild, err := child.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone the child", err)
		expect(t, "hangs from the child", grandchild.DebugGetCowPages().DebugGetParent(), child.DebugGetCowPages())
		grandMap := newTestMapping(t, env, grandchild)
		defer grandMap.unmap()
		grandMap.fault(0, false)
		seen, _, _ := grandMap.query(0)
		expect(t, "the root's page mapped", seen, root.DebugGetPage(0))
		mustNotFail(t, "store into the child's first", child.Write(env.ctx, []byte{'X'}, 0))
		_, _, mapped = grandMap.query(0)
		expect(t, "the root's page unmapped from the grandchild", mapped, false)
		got := readPage(t, env, grandchild, 0)
		expect(t, "the grandchild reads the child's copy", got[0], byte('X'))
	})
}
