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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
		expect(t, "the writeback holds the pause", bytes.Equal(held, pattern(ps, 'A')), true)
		expect(t, "the guest reads its store", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'B')), true)
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
		// The checkpoint lands: its page goes, and the store stays dirty.
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "the checkpoint's page was freed", env.pmm.out, pagesOut)
		expect(t, "the store is still dirty", vmo.DebugGetPage(0).dirtyState, Dirty)
		expectRanges(t, "dirty after the end", dirtyRanges(t, env, vmo), [][3]uint64{{0, 1, 0}})
		expect(t, "nothing held", vmo.ReadWriteback(env.ctx, held, 0), ErrBadState)
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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
		expect(t, "the writeback holds zeros", allZero(held), true)
		expect(t, "the guest reads its first store", bytes.Equal(readPage(t, env, vmo, ps), pattern(ps, 'B')), true)
		expect(t, "the guest reads its second store", bytes.Equal(readPage(t, env, vmo, 2*ps), pattern(ps, 'C')), true)
		mustNotFail(t, "end", vmo.WritebackEnd(0, 4*ps))
		// The zeros landed; the stores are still to be written.
		expectRanges(t, "dirty after the end", dirtyRanges(t, env, vmo), [][3]uint64{{1, 2, 0}})
		expect(t, "nothing held", vmo.ReadWriteback(env.ctx, held[:ps], 0), ErrBadState)
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
			expect(t, "nothing held", vmo.ReadWriteback(env.ctx, held, off), ErrBadState)
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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
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
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
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

// A store into a region's page an identity root holds unmaps the root's page
// from the region's mappings, so the next access finds the copy. Zircon
// skips the unmap when a page fills an empty slot of a pager's object, as no
// mapping can see one there; a region's mapping can.
func TestAStoreIntoARegionUnmapsTheRootsPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		root, err := CreateIdentityRoot(env.node, NewPageSource(newRecordingProvider(false)), ps)
		mustNotFail(t, "create the root", err)
		rootPage := supplyPagerVmoPages(t, env, root, 0, 1)[0]
		resolver := &staticResolver{ps: ps, root: root.DebugGetCowPages(), pages: map[uint64]uint64{0: 0}}
		region, err := CreateRegionLayer(env.node, NewPageSource(newRecordingProvider(false)), ps, resolver)
		mustNotFail(t, "create the region", err)
		mapping := newTestMapping(t, env, region)
		defer mapping.unmap()
		mapping.fault(0, false)
		mapped, _, _ := mapping.query(0)
		expect(t, "the root's page mapped", mapped, rootPage)
		mustNotFail(t, "store", region.Write(env.ctx, []byte{'X'}, 0))
		_, _, isMapped := mapping.query(0)
		expect(t, "the root's page unmapped", isMapped, false)
		mapping.fault(0, false)
		mapped, _, _ = mapping.query(0)
		expect(t, "the copy mapped", mapped, region.DebugGetPage(0))
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
		mustNotFail(t, "read the first", vmo.ReadWriteback(env.ctx, held, 0))
		expect(t, "the first held", bytes.Equal(held, pattern(ps, 'A')), true)
		mustNotFail(t, "read the third", vmo.ReadWriteback(env.ctx, held, 2*ps))
		expect(t, "the third held", bytes.Equal(held, pattern(ps, 'C')), true)
		expect(t, "the middle landed", vmo.ReadWriteback(env.ctx, held, ps), ErrBadState)
		mustNotFail(t, "read the zeros before", vmo.ReadWriteback(env.ctx, held, 3*ps))
		expect(t, "zeros held before", allZero(held), true)
		mustNotFail(t, "read the zeros after", vmo.ReadWriteback(env.ctx, held, 6*ps))
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
		expect(t, "the second's copy was freed", vmo.ReadWriteback(env.ctx, held, ps), ErrBadState)
		mustNotFail(t, "the third is held", vmo.ReadWriteback(env.ctx, held, 2*ps))
		mustNotFail(t, "the fourth is held", vmo.ReadWriteback(env.ctx, held, 3*ps))
		mapping.commitAndMap(false)
		mustNotFail(t, "abandon the third", vmo.WritebackAbandon(env.ctx, 2*ps, ps))
		expect(t, "the third's copy was freed", vmo.ReadWriteback(env.ctx, held, 2*ps), ErrBadState)
		mustNotFail(t, "the fourth is still held", vmo.ReadWriteback(env.ctx, held, 3*ps))
		expect(t, "the fourth holds the pause", bytes.Equal(held, pattern(ps, 'A')), true)
		for i, want := range []bool{true, true, false, true} {
			_, _, mapped := mapping.query(uint64(i) * ps)
			expect(t, "mapped outside the abandon", mapped, want)
		}
		// Every page writable again: the first, still AwaitingClean, gets a
		// copy and is held too.
		mapping.commitAndMap(true)
		mustNotFail(t, "the first is held", vmo.ReadWriteback(env.ctx, held, 0))
		mustNotFail(t, "begin the third again", vmo.WritebackBegin(2*ps, ps, false))
		for i, want := range []bool{true, true, false, true} {
			_, writable, _ := mapping.query(uint64(i) * ps)
			expect(t, "writable outside the begin", writable, want)
		}
		mustNotFail(t, "the first is held still", vmo.ReadWriteback(env.ctx, held, 0))
		mustNotFail(t, "the fourth is held still", vmo.ReadWriteback(env.ctx, held, 3*ps))
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

// Ending a writeback cleans only the zeros it took: zeros added to the
// interval after the pause stay dirty for the next one.
func TestEndingAWritebackCleansOnlyTheZerosItTook(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 4, false)
		mustNotFail(t, "zero the second", vmo.ZeroRange(env.ctx, ps, ps))
		expectRanges(t, "one dirty zero page", dirtyRanges(t, env, vmo), [][3]uint64{{1, 1, 1}})
		mustNotFail(t, "begin", vmo.WritebackBegin(ps, ps, false))
		mustNotFail(t, "zero the third", vmo.ZeroRange(env.ctx, 2*ps, ps))
		expectRanges(t, "both dirty", dirtyRanges(t, env, vmo), [][3]uint64{{1, 2, 1}})
		mustNotFail(t, "end", vmo.WritebackEnd(ps, 2*ps))
		expectRanges(t, "the third still dirty", dirtyRanges(t, env, vmo), [][3]uint64{{2, 1, 1}})
		// A single page of dirty zeros becomes untracked zeros when asked.
		mustNotFail(t, "zero the third untracked", vmo.ZeroRangeUntracked(env.ctx, 2*ps, ps))
		expectRanges(t, "nothing dirty", dirtyRanges(t, env, vmo), nil)
		expect(t, "zeros read", allZero(readPage(t, env, vmo, 2*ps)), true)
	})
}

// reservationsTaken is how many reservations of the case's spill are held.
func reservationsTaken(env *vmoEnv) int { return testSpillPages - env.storage.Available() }

// reservationOf is the reservation a page holds, failing the case where it
// holds none.
func reservationOf(t *testing.T, what string, page *VmPage) ReferenceValue {
	t.Helper()
	ref, reserved := page.DebugReservation()
	expect(t, what+" holds a reservation", reserved, true)
	return ref
}

// D5 with D4: an abandoned writeback gives each page made Dirty again a
// reservation of its own: the reservation the checkpoint held its bytes in,
// whether the page is resident or spilled. A page the guest copied away from
// keeps the copy's, and the checkpoint's goes back.
func TestAnAbandonedWritebackGivesEachPageItsOwnReservation(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 3, false)
		for i, b := range []byte{'A', 'B', 'C'} {
			mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, b), uint64(i)*ps))
		}
		first, second, third := vmo.DebugGetPage(0), vmo.DebugGetPage(ps), vmo.DebugGetPage(2*ps)
		firstRef := reservationOf(t, "the first", first)
		secondRef := reservationOf(t, "the second", second)
		thirdRef := reservationOf(t, "the third", third)
		// The third is spilled into its reservation.
		expect(t, "spilled", compressPage(t, env, vmo.DebugGetCowPages(), third, 2*ps), uint64(1))
		expect(t, "three taken", reservationsTaken(env), 3)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 3*ps, false))
		// A store into the second after the pause gets a copy, with a
		// reservation of its own (D1).
		mustNotFail(t, "store after the pause", vmo.Write(env.ctx, pattern(ps, 'D'), ps))
		secondCopy := vmo.DebugGetPage(ps)
		copyRef := reservationOf(t, "the second's copy", secondCopy)
		expect(t, "the copy's is not the checkpoint's", copyRef != secondRef, true)
		expect(t, "four taken", reservationsTaken(env), 4)
		mustNotFail(t, "abandon", vmo.WritebackAbandon(env.ctx, 0, 3*ps))
		expect(t, "the first is dirty", first.dirtyState, Dirty)
		expect(t, "with the reservation it had", reservationOf(t, "the first", first), firstRef)
		expect(t, "the second's copy is dirty", secondCopy.dirtyState, Dirty)
		expect(t, "with its own", reservationOf(t, "the second's copy", secondCopy), copyRef)
		expect(t, "the checkpoint's copy of the second went back", reservationsTaken(env), 3)
		// The third is read back dirty, holding the reservation it was
		// spilled into.
		expect(t, "the third's bytes", bytes.Equal(readPage(t, env, vmo, 2*ps), pattern(ps, 'C')), true)
		back := vmo.DebugGetPage(2 * ps)
		expect(t, "the third is dirty", back.dirtyState, Dirty)
		expect(t, "with the reservation it was spilled into", reservationOf(t, "the third", back), thirdRef)
		expect(t, "still three taken", reservationsTaken(env), 3)
		expectRanges(t, "dirty", dirtyRanges(t, env, vmo), [][3]uint64{{0, 3, 0}})
	})
}

// D5: a page spilled and read back goes on holding the one reservation, and
// a writeback that ends gives it back.
func TestADirtyPageHoldsOneReservationUntilItIsClean(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		page := vmo.DebugGetPage(0)
		_, reserved := page.DebugReservation()
		expect(t, "a clean page holds none", reserved, false)
		mustNotFail(t, "store", vmo.Write(env.ctx, pattern(ps, 'A'), 0))
		ref := reservationOf(t, "the dirty page", page)
		expect(t, "one taken", reservationsTaken(env), 1)
		expect(t, "spilled", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expect(t, "into its reservation", env.storage.Holds(ref), true)
		expect(t, "still one", reservationsTaken(env), 1)
		expect(t, "read back", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'A')), true)
		back := vmo.DebugGetPage(0)
		expect(t, "the same reservation", reservationOf(t, "the page read back", back), ref)
		expect(t, "holding no bytes", env.storage.Holds(ref), false)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		expect(t, "the checkpoint holds it", reservationOf(t, "the paused page", back), ref)
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		_, reserved = back.DebugReservation()
		expect(t, "a clean page holds none", reserved, false)
		expect(t, "none taken", reservationsTaken(env), 0)
	})
}

// D5: a store with no reservation left is refused, and the page stays clean.
func TestAStoreWithNoReservationLeftIsRefused(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 1, false)
		for env.storage.Available() > 0 {
			env.storage.Reserve()
		}
		expect(t, "the store", vmo.Write(env.ctx, pattern(ps, 'A'), 0), ErrNoSpace)
		expect(t, "the page stays clean", vmo.DebugGetPage(0).dirtyState, Clean)
		// A write into zeros, which copies the zero page, is refused too.
		zeros := makeUncommittedPagerVmo(t, env, 1, false)
		mustNotFail(t, "zero", zeros.ZeroRange(env.ctx, 0, ps))
		expect(t, "the store into zeros", zeros.Write(env.ctx, pattern(ps, 'Z'), 0), ErrNoSpace)
		expect(t, "zeros still", allZero(readPage(t, env, zeros, 0)), true)
	})
}

// D5: the pager's agreement to dirty a range takes every reservation it needs
// up front, so a range there are too few for is not dirtied at all.
func TestAnAgreementThereAreTooFewReservationsForDirtiesNothing(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 2, true)
		for env.storage.Available() > 1 {
			env.storage.Reserve()
		}
		expect(t, "the agreement", vmo.DirtyPages(env.ctx, 0, 2*ps), ErrNoSpace)
		expect(t, "the first clean", vmo.DebugGetPage(0).dirtyState, Clean)
		expect(t, "the second clean", vmo.DebugGetPage(ps).dirtyState, Clean)
		expect(t, "the one left is left", env.storage.Available(), 1)
		mustNotFail(t, "agree to one", vmo.DirtyPages(env.ctx, ps, ps))
		expect(t, "the second dirty", vmo.DebugGetPage(ps).dirtyState, Dirty)
		expect(t, "none left", env.storage.Available(), 0)
	})
}

// D5 with D2: the agreement counts what it reserves for spilled pages by
// their dirty state. A spilled Dirty page holds its reservation and stays
// spilled; a spilled page the checkpoint holds comes back to the checkpoint
// with its own, and the guest's copy takes a new one.
func TestAnAgreementReservesOnlyForSpilledPagesNotDirty(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, _ := makeCommittedPagerVmo(t, env, 3, true)
		mustNotFail(t, "agree", vmo.DirtyPages(env.ctx, 0, 3*ps))
		cow := vmo.DebugGetCowPages()
		paused := reservationOf(t, "the first", vmo.DebugGetPage(0))
		// The guest stores through its mapping, so the pages do not spill to zeros.
		for i, b := range []byte{'A', 'B', 'C'} {
			copy(vmo.DebugGetPage(uint64(i)*ps).data, pattern(ps, b))
		}
		for i := range uint64(3) {
			expect(t, "spilled", compressPage(t, env, cow, vmo.DebugGetPage(i*ps), i*ps), uint64(1))
		}
		mustNotFail(t, "begin the first", vmo.WritebackBegin(0, ps, false))
		for env.storage.Available() > 1 {
			env.storage.Reserve()
		}
		mustNotFail(t, "agree again", vmo.DirtyPages(env.ctx, 0, 3*ps))
		expect(t, "none left", env.storage.Available(), 0)
		copied := vmo.DebugGetPage(0)
		expect(t, "the guest's copy is dirty", copied.dirtyState, Dirty)
		expect(t, "with a reservation of its own", reservationOf(t, "the copy", copied) != paused, true)
		expect(t, "the second stays spilled", cow.DebugIsReference(ps), true)
		expect(t, "the third stays spilled", cow.DebugIsReference(2*ps), true)
		held := make([]byte, ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
		expect(t, "the checkpoint keeps the pause", bytes.Equal(held, copied.data), true)
	})
}

// A page at a Frame has no bytes in this process, so a copy, a zeroing or a
// compression of it here asserts rather than copying nothing: its pager moves
// its bytes itself.
func TestAPageAtAFrameHasNoBytesHere(t *testing.T) {
	page := NewFramePage("slot 3 of file 1")
	expect(t, "its frame", page.Frame, any("slot 3 of file 1"))
	defer func() {
		expect(t, "the assertion", recover() != nil, true)
	}()
	_ = page.bytesHere()
	t.Fatal("a page at a frame gave bytes")
}

// A splice list a supply has processed is reused for the next supply, and a
// supplied page's backlink names the object and offset it went to.
func TestASpliceListIsReusedAndASuppliedPagesBacklinkNamesItsObject(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo := makeUncommittedPagerVmo(t, env, 4, false)
		aux, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create aux", err)
		mustNotFail(t, "commit aux", aux.CommitRange(env.ctx, 0, 2*ps))
		splice := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, ps, splice))
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, ps, splice, PagerSupply))
		splice.Reuse()
		mustNotFail(t, "take again", aux.TakePages(env.ctx, ps, ps, splice))
		mustNotFail(t, "supply again", vmo.SupplyPages(env.ctx, 2*ps, ps, splice, PagerSupply))
		expect(t, "processed", splice.IsProcessed(), true)
		aux.Destroy()
		for _, offset := range []uint64{0, 2 * ps} {
			page, err := vmo.GetPage(env.ctx, offset, 0, nil)
			mustNotFail(t, "get page", err)
			link, ok := env.node.PageQueues().Backlink(page)
			expect(t, "queued", ok, true)
			expect(t, "the backlink's object", link.Cow, vmo.CowPages())
			expect(t, "the backlink's offset", link.Offset, offset)
			expect(t, "the page at the object's offset", vmo.CowPages().PageLocked(offset), page)
		}
		_, ok := env.node.PageQueues().Backlink(NewFramePage(nil))
		expect(t, "a page in no queue has no backlink", ok, false)
	})
}

// D1 for a pager that copies its own pages: the copy it made becomes the
// Dirty page, and the checkpoint keeps the page of its pause beside the page
// list until its writeback ends. Only an AwaitingClean page is split.
func TestAPagersOwnCopySplitsAPageACheckpointHolds(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, pages := makeCommittedPagerVmo(t, env, 2, true)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, 2*ps))
		mustNotFail(t, "store the pause's bytes", vmo.Write(env.ctx, pattern(2*ps, 'A'), 0))
		copied, err := env.pmm.AllocPage()
		mustNotFail(t, "allocate the copy", err)
		expect(t, "a Dirty page is not split", vmo.SplitAwaitingClean(0, copied), ErrBadState)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		copy(copied.data, pattern(ps, 'B'))
		mustNotFail(t, "split", vmo.SplitAwaitingClean(0, copied))
		expect(t, "the copy is the page", vmo.DebugGetPage(0), copied)
		expect(t, "the copy is dirty", copied.dirtyState, Dirty)
		expect(t, "the checkpoint's page is unchanged", pages[0].dirtyState, AwaitingClean)
		expect(t, "the checkpoint holds it", vmo.CowPages().HeldPageLocked(0), pages[0])
		held := make([]byte, ps)
		mustNotFail(t, "read the writeback", vmo.ReadWriteback(env.ctx, held, 0))
		expect(t, "the writeback holds the pause", bytes.Equal(held, pattern(ps, 'A')), true)
		expect(t, "the guest reads the copy", bytes.Equal(readPage(t, env, vmo, 0), pattern(ps, 'B')), true)
		pagesOut := env.pmm.out
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "the checkpoint's page was freed", env.pmm.out, pagesOut-1)
		expect(t, "nothing is held", vmo.CowPages().HeldPageLocked(0) == nil, true)
		expect(t, "the copy is still dirty", copied.dirtyState, Dirty)
	})
}

// D2's queue: on a node that spills its dirty pages, a Dirty or AwaitingClean
// page ages in the reclaim queues beside the Clean ones, and an access makes
// it the newest, so the pager's evictor takes pages in the order accesses
// touched them, whatever their state. The object still evicts only a Clean
// page itself.
func TestADirtyPageAgesWithTheCleanOnesWhereTheNodeSpillsDirtyPages(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		env.node = NewNode(env.pmm, ps, env.compression)
		env.node.AgeDirtyPages()
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 2, true)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, ps))
		expect(t, "the dirty page is in no dirty queue", pq.DebugPageIsPagerBackedDirty(pages[0]), false)
		reclaim, _ := pq.DebugPageIsReclaim(pages[0])
		expect(t, "the dirty page ages", reclaim, true)
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		reclaim, _ = pq.DebugPageIsReclaim(pages[0])
		expect(t, "the awaiting clean page ages", reclaim, true)
		pq.RotateReclaimQueues()
		pq.MarkAccessed(pages[0])
		_, age := pq.DebugPageIsReclaim(pages[0])
		_, cleanAge := pq.DebugPageIsReclaim(pages[1])
		expect(t, "the accessed dirty page is newer than the clean one", age < cleanAge, true)
		success, failure := vmo.CowPages().ReclaimRangeForEviction(0, ps, IgnoreHint)
		expect(t, "the object evicts no awaiting clean page", success.NumPages, uint64(0))
		expect(t, "and says why", failure, ReclaimOther)
	})
}

// A pager takes a page out of an object to move its bytes itself: from the
// page list, or from what a checkpoint holds beside it, whatever its state,
// and out of the page queues, without freeing it.
func TestAPagerTakesAPageOutOfAnObjectWithoutFreeingIt(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, pages := makeCommittedPagerVmo(t, env, 2, true)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, 2*ps))
		mustNotFail(t, "begin", vmo.WritebackBegin(0, 2*ps, false))
		copied, err := env.pmm.AllocPage()
		mustNotFail(t, "allocate the copy", err)
		mustNotFail(t, "split", vmo.SplitAwaitingClean(0, copied))
		pagesOut := env.pmm.out
		expect(t, "a page not at the offset", vmo.RemovePage(ps, pages[0]), false)
		expect(t, "the held page", vmo.RemovePage(0, pages[0]), true)
		expect(t, "nothing is held", vmo.CowPages().HeldPageLocked(0) == nil, true)
		expect(t, "the copy stays", vmo.DebugGetPage(0), copied)
		expect(t, "the page list's page", vmo.RemovePage(ps, pages[1]), true)
		expect(t, "the offset is empty", vmo.DebugGetPage(ps) == nil, true)
		for i, page := range pages {
			_, queued := env.node.PageQueues().Backlink(page)
			expect(t, "page "+string(rune('0'+i))+" is in no queue", queued, false)
			expect(t, "page "+string(rune('0'+i))+" is untracked", page.dirtyState, Untracked)
			_, reserved := page.DebugReservation()
			expect(t, "page "+string(rune('0'+i))+" holds no reservation", reserved, false)
		}
		expect(t, "nothing was freed", env.pmm.out, pagesOut)
		expect(t, "taken twice", vmo.RemovePage(ps, pages[1]), false)
	})
}
