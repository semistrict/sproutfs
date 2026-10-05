// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/vmo_unittest.cc and vm/unittests/test_helper.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"bytes"
	"testing"
)

// Cases of the port's own, not Zircon's, written as its VMO cases are: the
// departures D1, D2 and D4, and the identity roots a region's layer falls
// through to (plan: Zircon's objects and ours, Departures).

// recordingProvider is a stub provider that records the requests it is
// sent, as a pager would see them.
type recordingProvider struct {
	stubPageProvider
	requests []CowRange
	types    []PageRequestType
}

func newRecordingProvider(trapDirty bool) *recordingProvider {
	return &recordingProvider{stubPageProvider: stubPageProvider{trapDirty: trapDirty, ignoreRequests: true}}
}

func (p *recordingProvider) SendAsyncRequest(request *PageRequest) {
	p.requests = append(p.requests, CowRange{RequestOffset(request), RequestLen(request)})
	p.types = append(p.types, RequestType(request))
}

// pattern is a page of one byte value.
func pattern(ps uint64, b byte) []byte { return bytes.Repeat([]byte{b}, int(ps)) }

// readPage reads one page of vmo at offset.
func readPage(t *testing.T, env *vmoEnv, vmo *ObjectPaged, offset uint64) []byte {
	t.Helper()
	got := make([]byte, env.ps)
	mustNotFail(t, "read", vmo.Read(env.ctx, got, offset))
	return got
}

// dirtyRanges lists vmo's dirty ranges in pages, a zero range negated.
func dirtyRanges(t *testing.T, env *vmoEnv, vmo *ObjectPaged) [][3]uint64 {
	t.Helper()
	var ranges [][3]uint64
	err := vmo.EnumerateDirtyRanges(0, vmo.Size(), func(offset, length uint64, isZero bool) error {
		zero := uint64(0)
		if isZero {
			zero = 1
		}
		ranges = append(ranges, [3]uint64{offset / env.ps, length / env.ps, zero})
		return nil
	})
	mustNotFail(t, "enumerate", err)
	return ranges
}

func expectRanges(t *testing.T, what string, got, want [][3]uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", what, got, want)
			return
		}
	}
}

// D1, with the source not trapping dirty transitions: a store through the
// lookup. Its guard puts back Zircon's in-place rule, and this case fails.
func TestAStoreIntoAPageACheckpointHoldsGetsADirtyCopy(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		mustNotFail(t, "store the pause's bytes", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		paused := vmo.DebugGetPage(0)
		expect(t, "dirty", paused.dirtyState, Dirty)
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		mapping.fault(0, false)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		expect(t, "awaiting clean", paused.dirtyState, AwaitingClean)
		pagesOut := env.pmm.out
		// A store after the pause.
		mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(ps, 'B'), 0))
		copied := vmo.DebugGetPage(0)
		expect(t, "the store got its own page", copied != paused, true)
		expect(t, "a page was allocated for it", env.pmm.out, pagesOut+1)
		expect(t, "the copy is dirty", copied.dirtyState, Dirty)
		expect(t, "the copy is in the dirty queue", pq.DebugPageIsPagerBackedDirty(copied), true)
		expect(t, "the checkpoint's page is unchanged", paused.dirtyState, AwaitingClean)
		expect(t, "the checkpoint's page holds the pause", bytes.Equal(paused.data, pattern(ps, 'A')), true)
		// The mapping of the checkpoint's page was revoked.
		_, _, mapped := mapping.query(0)
		expect(t, "the old mapping is gone", mapped, false)
		// The upload reads the pause's bytes; the guest reads its store.
		held := make([]byte, ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "the writeback holds the pause", bytes.Equal(held, pattern(ps, 'A')), true)
		expect(t, "the guest reads its store", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'B')), true)
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
		// The checkpoint lands: its page goes, and the store stays dirty.
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "the checkpoint's page was freed", env.pmm.out, pagesOut)
		expect(t, "the store is still dirty", vmo.DebugGetPage(0).dirtyState, Dirty)
		expectRanges(t, "dirty after the end", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
		expect(t, "nothing held", vmo.ReadWriteback(held, 0), ErrBadState)
	})
}

// D1, with the source trapping dirty transitions: a store the pager agreed
// to with DirtyPages.
func TestAPagerAgreedStoreIntoAPageACheckpointHoldsGetsADirtyCopy(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, pages := makeCommittedPagerVmo(t, env, 2, true)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, 2*ps))
		mustNotFail(t, "store the pause's bytes", vmo.Write(env.ctx, pattern(2*ps, 'A'), 0))
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 2*ps, false))
		// The pager agrees to a store into the first page only.
		mustNotFail(t, "dirty the first", vmo.DirtyPages(env.ctx, 0, ps))
		expect(t, "the first was split", vmo.DebugGetPage(0) != pages[0], true)
		expect(t, "the first's copy is dirty", vmo.DebugGetPage(0).dirtyState, Dirty)
		expect(t, "the second is still the checkpoint's", vmo.DebugGetPage(ps) == pages[1], true)
		expect(t, "the second is awaiting clean", pages[1].dirtyState, AwaitingClean)
		mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, 'B'), 0))
		held := make([]byte, 2*ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "the writeback holds the pause", bytes.Equal(held, pattern(2*ps, 'A')), true)
		mustNotFail(t, "end", vmo.WritebackEnd(0, 2*ps))
		expect(t, "the first is dirty", vmo.DebugGetPage(0).dirtyState, Dirty)
		expect(t, "the second is clean", pages[1].dirtyState, Clean)
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
	})
}

// D1 over zeros: stores that split an AwaitingClean zero interval, one at
// its middle and one in the part to its right, leave the checkpoint zeros.
func TestStoresIntoAZeroIntervalACheckpointHoldsLeaveItsZeros(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 4, false)
		mustNotFail(t, "zero", vmo.ZeroRange(env.ctx, 0, 4*ps))
		expectRanges(t, "a dirty zero range", dirtyRanges(t, env, vmo), [][3]uint64{{0, 4, 1}})
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 4*ps, true))
		mustNotFail(t, "store into the middle", vmo.Write(env.ctx, pattern(ps, 'B'), ps))
		mustNotFail(t, "store to its right", vmo.Write(env.ctx, pattern(ps, 'C'), 2*ps))
		held := make([]byte, 4*ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "the writeback holds zeros", allZero(held), true)
		expect(t, "the guest reads its first store", bytes.Equal(readPage(t, env, vmo, ps), pattern(ps, 'B')), true)
		expect(t, "the guest reads its second store", bytes.Equal(readPage(t, env, vmo, 2*ps), pattern(ps, 'C')), true)
		mustNotFail(t, "end", vmo.WritebackEnd(0, 4*ps))
		// The zeros landed; the stores are still to be written.
		expectRanges(t, "dirty after the end", dirtyRanges(t, env, vmo), [][3]uint64{{1, 2, 0}})
		expect(t, "nothing held", vmo.ReadWriteback(held[:ps], 0), ErrBadState)
	})
}

// D4. Its guard puts back Zircon's rule, which leaves the pages
// AwaitingClean, and this case fails.
func TestAnAbandonedWritebackMakesEveryPageDirtyAgain(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo := makeUncommittedPagerVmo(t, env, 3, false)
		// A dirty page, another, and dirty zeros.
		mustNotFail(t, "zero", vmo.ZeroRange(env.ctx, 0, 3*ps))
		mustNotFail(t, "store the first", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		mustNotFail(t, "store the second", vmo.Write(env.ctx, pattern(ps, 'B'), ps))
		first, second := vmo.DebugGetPage(0), vmo.DebugGetPage(ps)
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		mapping.fault(0, false)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 3*ps, false))
		// A store into the second after the pause gets a copy (D1).
		mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(ps, 'C'), ps))
		secondCopy := vmo.DebugGetPage(ps)
		pagesOut := env.pmm.out
		mustNotFail(t, "abandon", vmo.WritebackAbandon(env.ctx, 0, 3*ps))
		expect(t, "the first is dirty again", first.dirtyState, Dirty)
		expect(t, "the first is its own page still", vmo.DebugGetPage(0), first)
		expect(t, "the first is in the dirty queue", pq.DebugPageIsPagerBackedDirty(first), true)
		expect(t, "the second's copy is the guest's", vmo.DebugGetPage(ps), secondCopy)
		expect(t, "the second's copy is dirty", secondCopy.dirtyState, Dirty)
		expect(t, "the checkpoint's copy of the second was freed", env.pmm.out, pagesOut-1)
		expect(t, "second's old page was given back", second.dirtyState, Untracked)
		// The mapping of the range was revoked.
		_, _, mapped := mapping.query(0)
		expect(t, "the mapping is gone", mapped, false)
		// Nothing is held, and everything is dirty for the next writeback.
		held := make([]byte, ps)
		for _, off := range []uint64{0, ps, 2 * ps} {
			expect(t, "nothing held", vmo.ReadWriteback(held, off), ErrBadState)
		}
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 2, 0}, {2, 1, 1}})
		// The next writeback takes the zeros again, and ends with them clean.
		mustNotFail(t, "begin again", vmo.WritebackBegin(0, 3*ps, false))
		mustNotFail(t, "end", vmo.WritebackEnd(0, 3*ps))
		expectRanges(t, "clean", dirtyRanges(t, env, vmo), nil)
		// A store into the first now needs no copy.
		pagesOut = env.pmm.out
		mustNotFail(t, "store into the first again", vmo.Write(env.ctx, pattern(ps, 'D'), 0))
		expect(t, "the first stores in place", vmo.DebugGetPage(0), first)
		expect(t, "no page allocated", env.pmm.out, pagesOut)
	})
}

// D2: a dirty page is spilled, keeps its dirty state through a writeback,
// and comes back clean once the writeback ended.
func TestADirtyPageIsSpilledAndKeepsItsDirtyState(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		page := vmo.DebugGetPage(0)
		// Without a compressor a dirty page is not reclaimed, as Zircon's.
		expect(t, "not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		// With one it is spilled.
		expect(t, "spilled", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expect(t, "a reference", vmo.DebugGetCowPages().DebugIsReference(0), true)
		expectAttribution(t, "spilled", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectRanges(t, "still dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		held := make([]byte, ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "the writeback reads the spilled page", bytes.Equal(held, pattern(ps, 'A')), true)
		back := vmo.DebugGetPage(0)
		expect(t, "the writeback read it back awaiting clean", back.dirtyState, AwaitingClean)
		expect(t, "in the dirty queue", pq.DebugPageIsPagerBackedDirty(back), true)
		// Spilled again while awaiting clean, and ended.
		expect(t, "spilled awaiting clean", compressPage(t, env, vmo.DebugGetCowPages(), back, 0), uint64(1))
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expectRanges(t, "clean", dirtyRanges(t, env, vmo), nil)
		// Read back, it is a clean page that can be evicted.
		expect(t, "the bytes", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'A')), true)
		clean := vmo.DebugGetPage(0)
		expect(t, "clean", clean.dirtyState, Clean)
		expectReclaim(t, env, "reclaimable", clean, 0)
		expect(t, "evicted", reclaim(env, vmo, clean, 0, FollowHint), uint64(1))
	})
}

// D2: a dirty page of zeros spills to a dirty zero interval, and an
// AwaitingClean one to an AwaitingClean interval the checkpoint holds as
// zeros.
func TestADirtyPageOfZerosSpillsToADirtyZeroInterval(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 2, false)
		mustNotFail(t, "store zeros", vmo.Write(env.ctx, make([]byte, 2*ps), 0))
		first := vmo.DebugGetPage(0)
		expect(t, "spilled the first", compressPage(t, env, vmo.DebugGetCowPages(), first, 0), uint64(1))
		expectRanges(t, "a zero range and a page", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 1}, {1, 1, 0}})
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 2*ps, false))
		second := vmo.DebugGetPage(ps)
		expect(t, "spilled the second", compressPage(t, env, vmo.DebugGetCowPages(), second, ps), uint64(1))
		held := make([]byte, 2*ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "zeros held", allZero(held), true)
		mustNotFail(t, "end", vmo.WritebackEnd(0, 2*ps))
		expectRanges(t, "clean", dirtyRanges(t, env, vmo), nil)
		// The zeros were written back: the pager supplies them from here on.
		expect(t, "the first is the pager's again", vmo.DebugGetCowPages().DebugIsEmpty(0), true)
		expect(t, "the second is the pager's again", vmo.DebugGetCowPages().DebugIsEmpty(ps), true)
	})
}

// D2 over D1: the page a checkpoint holds beside the page list is spilled
// too, and its writeback reads it back.
func TestAPageACheckpointHoldsIsSpilledAndReadBack(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		paused := vmo.DebugGetPage(0)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(ps, 'B'), 0))
		expect(t, "the checkpoint's page spilled", compressPage(t, env, vmo.DebugGetCowPages(), paused, 0), uint64(1))
		expect(t, "the guest's page is untouched", vmo.DebugGetCowPages().DebugIsPage(0), true)
		held := make([]byte, ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(held, 0))
		expect(t, "the pause's bytes", bytes.Equal(held, pattern(ps, 'A')), true)
		pagesOut := env.pmm.out
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "the checkpoint's page was freed", env.pmm.out, pagesOut-1)
		expect(t, "the guest's store", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'B')), true)
	})
}

// staticResolver resolves offsets of a region to an identity root by a map
// of page indexes.
type staticResolver struct {
	ps    uint64
	root  *CowPages
	pages map[uint64]uint64
}

func (r *staticResolver) Locate(offset uint64) (*CowPages, uint64, bool) {
	rootPage, ok := r.pages[offset/r.ps]
	if !ok {
		return nil, 0, false
	}
	return r.root, rootPage * r.ps, true
}

// A region's layer reads an identity root's page in place, copies it to
// store into it, and asks its own source for what no root holds.
func TestARegionReadsItsIdentityRootAndCopiesItToStore(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		rootProvider := newRecordingProvider(false)
		root, err := CreateIdentityRoot(env.node, NewPageSource(rootProvider), 4*ps)
		mustNotFail(t, "create the root", err)
		rootPages := supplyPagerVmoPages(t, env, root, 0, 2)
		// The root's first pages hold a pattern.
		copy(rootPages[0].data, pattern(ps, 'R'))
		copy(rootPages[1].data, pattern(ps, 'S'))
		regionProvider := newRecordingProvider(false)
		resolver := &staticResolver{ps: ps, root: root.DebugGetCowPages(), pages: map[uint64]uint64{1: 0, 2: 1, 3: 2}}
		region, err := CreateRegionLayer(env.node, NewPageSource(regionProvider), 4*ps, resolver)
		mustNotFail(t, "create the region", err)
		// Page 1 of the region is the root's page 0, read in place.
		page, err := region.GetPageBlocking(env.ctx, ps, PfFlagSwFault)
		mustNotFail(t, "read fault", err)
		expect(t, "the root's page", page, rootPages[0])
		expect(t, "the root's bytes", bytes.Equal(readPage(t, env, region, ps), pattern(ps, 'R')), true)
		expectAttribution(t, "the region holds nothing", region.GetAttributedMemory(), AttributionCounts{})
		// A read of the region's page 0, which no root holds, asks the
		// region's source for that page alone: page 1 is the root's.
		pageRequest := NewMultiPageRequest()
		cursorReq := func() error {
			_, err := region.GetPage(env.ctx, 0, PfFlagSwFault, pageRequest)
			return err
		}
		expect(t, "a read request", cursorReq(), ErrShouldWait)
		expect(t, "the region's request", len(regionProvider.requests), 1)
		expect(t, "the request", regionProvider.requests[0], CowRange{0, ps})
		pageRequest.CancelRequests()
		// The region's page 3 is the root's page 2, which the root does not
		// hold: the root's source is asked.
		pageRequest = NewMultiPageRequest()
		_, err = region.GetPage(env.ctx, 3*ps, PfFlagSwFault, pageRequest)
		expect(t, "a read request of the root", err, ErrShouldWait)
		expect(t, "the root's requests", len(rootProvider.requests), 1)
		expect(t, "the root's request", rootProvider.requests[0], CowRange{2 * ps, ps})
		pageRequest.CancelRequests()
		// A store copies the root's page into the region, dirty.
		mustNotFail(t, "store", region.Write(env.ctx, []byte{'X'}, 2*ps))
		copied := region.DebugGetPage(2 * ps)
		expect(t, "a page of the region's own", copied != nil && copied != rootPages[1], true)
		expect(t, "dirty", copied.dirtyState, Dirty)
		want := pattern(ps, 'S')
		want[0] = 'X'
		expect(t, "the copy", bytes.Equal(readPage(t, env, region, 2*ps), want), true)
		expect(t, "the root is unchanged", bytes.Equal(rootPages[1].data, pattern(ps, 'S')), true)
		expect(t, "the root's page is clean", rootPages[1].dirtyState, Clean)
		expectAttribution(t, "the region holds its copy", region.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectRanges(t, "the region's dirty page", dirtyRanges(t, env, region), [][3]uint64{{2, 1, 0}})
		// Readable lookup sees the root's page and the copy where they read.
		offsets := []uint64{}
		pages := []*VmPage{}
		err = region.DebugGetCowPages().DebugLookupReadable(CowRange{ps, 2 * ps}, func(offset uint64, page *VmPage) error {
			offsets = append(offsets, offset)
			pages = append(pages, page)
			return nil
		})
		mustNotFail(t, "lookup readable", err)
		expectOffsets(t, "readable", offsets, []uint64{1, 2}, ps)
		expect(t, "the root's page readable", pages[0], rootPages[0])
		expect(t, "the copy readable", pages[1], copied)
	})
}

// A region's child of a fork reads through the region to its identity root,
// and a root's page evicted is read again from the root's source.
func TestARegionsChildReadsTheRootThroughTheRegion(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		rootProvider := newRecordingProvider(false)
		root, err := CreateIdentityRoot(env.node, NewPageSource(rootProvider), ps)
		mustNotFail(t, "create the root", err)
		rootPage := supplyPagerVmoPages(t, env, root, 0, 1)[0]
		copy(rootPage.data, pattern(ps, 'R'))
		resolver := &staticResolver{ps: ps, root: root.DebugGetCowPages(), pages: map[uint64]uint64{0: 0}}
		region, err := CreateRegionLayer(env.node, NewPageSource(newRecordingProvider(false)), ps, resolver)
		mustNotFail(t, "create the region", err)
		child, err := region.CreateClone(SnapshotOnWrite, 0, ps)
		mustNotFail(t, "clone", err)
		page, err := child.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		mustNotFail(t, "read through", err)
		expect(t, "the root's page", page, rootPage)
		// The root's page is evicted and asked for again.
		expect(t, "evicted", reclaim(env, root, rootPage, 0, FollowHint), uint64(1))
		pageRequest := NewMultiPageRequest()
		_, err = child.GetPage(env.ctx, 0, PfFlagSwFault, pageRequest)
		expect(t, "a read request of the root", err, ErrShouldWait)
		expect(t, "the root's requests", len(rootProvider.requests), 1)
		pageRequest.CancelRequests()
	})
}

// D1: ending part of a writeback frees only what the checkpoint held there,
// pages and zeros alike, and keeps the rest.
func TestEndingPartOfAWritebackFreesOnlyWhatItHeldThere(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 7, false)
		mustNotFail(t, "zero", vmo.ZeroRange(env.ctx, 0, 7*ps))
		for i, b := range []byte("ABC") {
			mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, b), uint64(i)*ps))
		}
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 7*ps, false))
		for i := range uint64(3) {
			mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(ps, 'x'), i*ps))
		}
		pagesOut := env.pmm.out
		// End the middle page and the middle of the zeros.
		mustNotFail(t, "end the middle page", vmo.WritebackEnd(ps, ps))
		mustNotFail(t, "end the middle zeros", vmo.WritebackEnd(4*ps, 2*ps))
		expect(t, "the middle page's copy was freed", env.pmm.out, pagesOut-1)
		held := make([]byte, ps)
		mustNotFail(t, "read the first", vmo.ReadWriteback(held, 0))
		expect(t, "the first held", bytes.Equal(held, pattern(ps, 'A')), true)
		mustNotFail(t, "read the third", vmo.ReadWriteback(held, 2*ps))
		expect(t, "the third held", bytes.Equal(held, pattern(ps, 'C')), true)
		expect(t, "the middle landed", vmo.ReadWriteback(held, ps), ErrBadState)
		mustNotFail(t, "read the zeros before", vmo.ReadWriteback(held, 3*ps))
		expect(t, "zeros held before", allZero(held), true)
		mustNotFail(t, "read the zeros after", vmo.ReadWriteback(held, 6*ps))
		expect(t, "zeros held after", allZero(held), true)
		expect(t, "the middle zeros landed", vmo.DebugGetCowPages().held.IsOffsetInZeroInterval(5*ps), false)
		// The rest ends.
		mustNotFail(t, "end the rest", vmo.WritebackEnd(0, 7*ps))
		expect(t, "both other copies were freed", env.pmm.out, pagesOut-3)
		expect(t, "nothing held", vmo.DebugGetCowPages().held.IsEmpty(), true)
	})
}

// A writeback's protection, end and abandon act on their range alone: the
// pages and mappings past it, and what the checkpoint holds there, stay.
func TestAWritebacksCallsActOnTheirRangeAlone(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 4, false)
		mustNotFail(t, "store the pause's bytes", vmo.Write(env.ctx, pattern(4*ps, 'A'), 0))
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		mapping.commitAndMap(true)
		mustNotFail(t, "protect the second", vmo.WritebackProtect(ps, ps))
		for i, want := range []bool{true, false, true, true} {
			_, writable, _ := mapping.query(uint64(i) * ps)
			expect(t, "writable outside the protection", writable, want)
		}
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 4*ps, false))
		// Stores after the pause leave the checkpoint the pause's bytes (D1).
		mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(3*ps, 'B'), ps))
		mustNotFail(t, "end the second", vmo.WritebackEnd(ps, ps))
		held := make([]byte, ps)
		expect(t, "the second's copy was freed", vmo.ReadWriteback(held, ps), ErrBadState)
		mustNotFail(t, "the third is held", vmo.ReadWriteback(held, 2*ps))
		mustNotFail(t, "the fourth is held", vmo.ReadWriteback(held, 3*ps))
		mapping.commitAndMap(false)
		mustNotFail(t, "abandon the third", vmo.WritebackAbandon(env.ctx, 2*ps, ps))
		expect(t, "the third's copy was freed", vmo.ReadWriteback(held, 2*ps), ErrBadState)
		mustNotFail(t, "the fourth is still held", vmo.ReadWriteback(held, 3*ps))
		expect(t, "the fourth holds the pause", bytes.Equal(held, pattern(ps, 'A')), true)
		for i, want := range []bool{true, true, false, true} {
			_, _, mapped := mapping.query(uint64(i) * ps)
			expect(t, "mapped outside the abandon", mapped, want)
		}
	})
}

// A store into a page a checkpoint holds fails when no page can be had for
// its copy, and leaves the page to the checkpoint, through the lookup and
// through the pager's agreement alike.
func TestAStoreThatGetsNoPageForItsCopyFails(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		mustNotFail(t, "store the pause's bytes", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		paused := vmo.DebugGetPage(0)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		env.pmm.failNext = true
		expect(t, "the store", vmo.Write(env.ctx, pattern(ps, 'B'), 0), ErrNoMemory)
		expect(t, "the checkpoint's page stays", vmo.DebugGetPage(0), paused)
		expect(t, "awaiting clean", paused.dirtyState, AwaitingClean)
		expect(t, "the guest reads the pause", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'A')), true)

		trapped, _ := makeCommittedPagerVmo(t, env, 1, true)
		mustNotFail(t, "agree", trapped.DirtyPages(env.ctx, 0, ps))
		mustNotFail(t, "begin the trapped", trapped.WritebackBegin(0, ps, false))
		env.pmm.failNext = true
		expect(t, "the agreement", trapped.DirtyPages(env.ctx, 0, ps), ErrNoMemory)
		expect(t, "still awaiting clean", trapped.DebugGetPage(0).dirtyState, AwaitingClean)
	})
}
