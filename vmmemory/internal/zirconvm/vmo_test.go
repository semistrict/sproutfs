// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/vmo_unittest.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"bytes"
	"testing"
)

// The 33 cases of vmo_unittest.cc the plan ports (plan: Which Zircon tests
// port), at both page sizes, each in a synctest bubble over a simulated
// runtime. Zircon's kPageSize is env.ps. A case Zircon writes with a full
// snapshot, a modified snapshot of a child, or a slice, which need a hidden
// parent the port does not have, makes a snapshot-on-write child instead;
// its comment says so and what that changes.

// vmo_create_test
func TestAnObjectIsCreatedAtItsSizeAndNotResizable(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		vmo, err := CreateObjectPaged(env.node, env.ps)
		mustNotFail(t, "create", err)
		expect(t, "size", vmo.Size(), env.ps)
		// Zircon also checks the object is not contiguous; contiguous
		// objects are not ported.
		expect(t, "resizable", vmo.IsResizable(), false)
	})
}

// vmo_commit_test
func TestCommittingAnObjectAttributesItsPagesAndQueuesThemAnonymous(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		allocSize := 16 * env.ps
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, allocSize))
		expectAttribution(t, "after commit", vmo.GetAttributedMemory(), PrivateAttributionCounts(allocSize, 0))
		expectContinuousAttribution(t, env, vmo, allocSize)
		expect(t, "anonymous queue", pagesInAnyAnonymousQueue(env, vmo, 0, allocSize), true)
	})
}

// vmo_commit_compressed_pages_test
func TestCommittingCompressedPagesDecompressesThem(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		const pages = 8
		vmo, err := CreateObjectPaged(env.node, pages*env.ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, pages*env.ps))
		expectAttribution(t, "committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(pages*env.ps, 0))
		expectContinuousAttribution(t, env, vmo, pages*env.ps)
		expect(t, "anonymous queue", pagesInAnyAnonymousQueue(env, vmo, 0, pages*env.ps), true)
		// Write the page's index, which is zero for the first, to each page,
		// and compress it.
		for i := range uint64(pages) {
			mustNotFail(t, "write", vmo.Write(env.ctx, putUint64(i), i*env.ps))
			page, err := vmo.GetPageBlocking(env.ctx, i*env.ps, 0)
			mustNotFail(t, "get page", err)
			expect(t, "reclaimed", compressPage(t, env, vmo.DebugGetCowPages(), page, i*env.ps), uint64(1))
		}
		// No pages left, and the first was zeros, which is not even kept
		// compressed.
		expectAttribution(t, "compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, (pages-1)*env.ps))
		expectContinuousAttribution(t, env, vmo, (pages-1)*env.ps)
		// Committing again decompresses.
		mustNotFail(t, "commit again", vmo.CommitRange(env.ctx, 0, pages*env.ps))
		expectAttribution(t, "committed again", vmo.GetAttributedMemory(), PrivateAttributionCounts(pages*env.ps, 0))
		expectContinuousAttribution(t, env, vmo, pages*env.ps)
		expect(t, "anonymous queue", pagesInAnyAnonymousQueue(env, vmo, 0, pages*env.ps), true)
		got := make([]byte, 8)
		mustNotFail(t, "read", vmo.Read(env.ctx, got, 5*env.ps))
		expect(t, "the fifth page's bytes", string(got), string(putUint64(5)))
	})
}

// vmo_demand_paged_map_test
func TestAMappedObjectFaultsItsPagesInOnDemand(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		allocSize := 16 * env.ps
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		expect(t, "filled and read back", fillAndTestUser(t, mapping, allocSize), true)
		expect(t, "faults", mapping.faults, 16)
	})
}

// vmo_dropped_ref_test
func TestAMappingKeepsTheObjectItMapsAfterItsMakerLetsGo(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		allocSize := 16 * env.ps
		mapping := func() *testMapping {
			vmo, err := CreateObjectPaged(env.node, allocSize)
			mustNotFail(t, "create", err)
			m := newTestMapping(t, env, vmo)
			// Zircon maps with VMM_FLAG_COMMIT and then drops its own
			// reference; the mapping holds the object.
			m.commitAndMap(true)
			return m
		}()
		expect(t, "filled and read back", fillAndTestUser(t, mapping, allocSize), true)
		// Every page was committed by the map, so the fill took no fault.
		expect(t, "faults", mapping.faults, 16)
		mapping.unmap()
	})
}

// vmo_read_write_smoke_test
func TestReadsAndWritesStayInTheObjectAndMatchItsMapping(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		allocSize := 16 * env.ps
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		// A buffer longer than the object, for the writes past its end.
		a := make([]byte, allocSize+99)
		fillRegion(99, a[:allocSize])
		expectNoError(t, "write nothing", vmo.Write(env.ctx, a[:0], 0))
		expectNoError(t, "write 37", vmo.Write(env.ctx, a[:37], 0))
		expectNoError(t, "write 37 at 99", vmo.Write(env.ctx, a[:37], 99))
		// Not past the end.
		expect(t, "write past the end", vmo.Write(env.ctx, a[:allocSize+47], 0), ErrOutOfRange)
		expect(t, "write past the end at 31", vmo.Write(env.ctx, a[:allocSize+47], 31), ErrOutOfRange)
		expect(t, "write out of range", vmo.Write(env.ctx, a[:42], allocSize+99), ErrOutOfRange)
		mapping := newTestMapping(t, env, vmo)
		mapping.commitAndMap(true)
		// Odd offsets.
		expectNoError(t, "write 4197 at 31", vmo.Write(env.ctx, a[:4197], 31))
		got := make([]byte, 4197)
		mapping.readAt(got, 31)
		expect(t, "mapped bytes at 31", bytes.Equal(got, a[:4197]), true)
		// The whole object.
		expectNoError(t, "write all", vmo.Write(env.ctx, a[:allocSize], 0))
		all := make([]byte, allocSize)
		mapping.readAt(all, 0)
		expect(t, "mapped pattern", testRegion(99, all), true)
		mapping.unmap()
		b := make([]byte, allocSize)
		expectNoError(t, "read all", vmo.Read(env.ctx, b, 0))
		expect(t, "read back", bytes.Equal(b, a[:allocSize]), true)
		expectNoError(t, "read 4197 at 31", vmo.Read(env.ctx, b[:4197], 31))
		expect(t, "read at 31", bytes.Equal(b[:4197], a[31:31+4197]), true)
	})
}

// vmo_lookup_test
func TestLookupSeesOnlyCommittedPages(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		allocSize := 16 * ps
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		pagesSeen := 0
		lookupFn := func(uint64, *VmPage) error {
			pagesSeen++
			return nil
		}
		lookup := func(what string, offset, length uint64, want int) {
			t.Helper()
			pagesSeen = 0
			expectNoError(t, what, vmo.Lookup(offset, length, lookupFn))
			expect(t, what, pagesSeen, want)
		}
		lookup("uncommitted", 0, allocSize, 0)
		mustNotFail(t, "commit one", vmo.CommitRange(env.ctx, ps, ps))
		expectAttribution(t, "one committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		lookup("the early range", 0, ps, 0)
		lookup("the whole range", 0, allocSize, 1)
		lookup("from the page", ps, allocSize-ps, 1)
		lookup("the page", ps, ps, 1)
		// A contiguous lookup of one page succeeds.
		_, err = vmo.LookupContiguous(ps, ps)
		expectNoError(t, "contiguous lookup of the page", err)
		mustNotFail(t, "commit all", vmo.CommitRange(env.ctx, 0, allocSize))
		expectAttribution(t, "all committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(allocSize, 0))
		expectContinuousAttribution(t, env, vmo, allocSize)
		lookup("all committed", 0, allocSize, 16)
		_, err = vmo.LookupContiguous(0, ps)
		expectNoError(t, "contiguous lookup of one page", err)
		_, err = vmo.LookupContiguous(0, allocSize)
		expect(t, "contiguous lookup of many pages", err, ErrBadState)
	})
}

// vmo_lookup_clone_test. Zircon makes a full snapshot, which moves the
// original's pages to a hidden parent, so the original then sees none of
// them. A snapshot-on-write child leaves the original its pages, and the
// child sees only the two it committed.
func TestAChildSeesOnlyThePagesItCommittedAndItsParentKeepsItsOwn(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const pageCount = 4
		allocSize := pageCount * ps
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, allocSize))
		clone, err := vmo.CreateClone(SnapshotOnWrite, 0, allocSize)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "commit the clone's first", clone.CommitRange(env.ctx, 0, ps))
		mustNotFail(t, "commit the clone's last", clone.CommitRange(env.ctx, allocSize-ps, ps))
		var vmoLookup, cloneLookup [pageCount]*VmPage
		expectNoError(t, "vmo lookup", vmo.Lookup(0, allocSize, func(offset uint64, page *VmPage) error {
			vmoLookup[offset/ps] = page
			return nil
		}))
		expectNoError(t, "clone lookup", clone.Lookup(0, allocSize, func(offset uint64, page *VmPage) error {
			cloneLookup[offset/ps] = page
			return nil
		}))
		cloneHasPage := [pageCount]bool{true, false, false, true}
		for i := range pageCount {
			expect(t, "the original's page", vmoLookup[i] != nil, true)
			expect(t, "a page in the clone", cloneLookup[i] != nil, cloneHasPage[i])
			expect(t, "no page shared", cloneLookup[i] == vmoLookup[i], false)
		}
	})
}

// vmo_clone_removes_write_test. Zircon makes a full snapshot, whose hidden
// parent takes the page, so the original's mapping becomes read-only. A
// snapshot-on-write child changes nothing in its parent: the parent's
// mapping stays writable, on the same page.
func TestACloneOnWriteLeavesItsParentsMappingWritableOnItsPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		vmo, err := CreateObjectPaged(env.node, env.ps)
		mustNotFail(t, "create", err)
		mapping := newTestMapping(t, env, vmo)
		defer mapping.unmap()
		mapping.commitAndMap(true)
		pageWritable, writable, mapped := mapping.query(0)
		expect(t, "mapped", mapped, true)
		expect(t, "writable", writable, true)
		_, err = vmo.CreateClone(SnapshotOnWrite, 0, env.ps)
		mustNotFail(t, "clone", err)
		pageReadable, writable, mapped := mapping.query(0)
		expect(t, "still mapped", mapped, true)
		expect(t, "still writable", writable, true)
		expect(t, "the same page", pageReadable, pageWritable)
	})
}

// vmo_clones_of_compressed_pages_test. Zircon makes a full snapshot, which
// shares the compressed page between the original and the clone through a
// hidden parent, and compresses the hidden parent's page later. A
// snapshot-on-write child is not attributed its parent's pages, and the
// parent's page is compressed in the parent.
func TestAChildCopiesItsParentsCompressedPageOnlyWhenItWrites(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, ps))
		data := putUint64(42)
		mustNotFail(t, "write", vmo.Write(env.ctx, data, 0))
		expectAttribution(t, "committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		page, err := vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get page", err)
		expect(t, "compressed", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		// Making a child keeps the page compressed.
		clone, err := vmo.CreateClone(SnapshotOnWrite, 0, ps)
		mustNotFail(t, "clone", err)
		expectAttribution(t, "parent after the clone", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		expectAttribution(t, "child", clone.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, clone, 0)
		expect(t, "still a reference", vmo.DebugGetCowPages().DebugIsReference(0), true)
		// A write in the child decompresses the page to copy it.
		mustNotFail(t, "write the clone", clone.Write(env.ctx, data, 0))
		expectAttribution(t, "parent after the fork", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectAttribution(t, "child after the fork", clone.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expectContinuousAttribution(t, env, clone, ps)
		// Compress the parent's page again.
		page = vmo.DebugGetPage(0)
		expect(t, "the parent has its page", page != nil, true)
		expect(t, "compressed again", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "parent compressed again", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectAttribution(t, "child keeps its page", clone.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expectContinuousAttribution(t, env, clone, ps)
		// The child going leaves the parent's page compressed.
		clone.Destroy()
		expectAttribution(t, "parent alone", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
	})
}

// vmo_move_pages_on_access_test
func TestAnAccessMovesAPageToTheNewestQueueEvenThroughAChild(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 1, false)
		page := pages[0]
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "in a reclaim queue", ok, true)
		// A lookup moves it to the first queue.
		_, err := vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "lookup", err)
		expectReclaim(t, env, "looked up", page, 0)
		pq.RotateReclaimQueues()
		expectReclaim(t, env, "rotated", page, 1)
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "lookup again", err)
		expectReclaim(t, env, "touched", page, 0)
		// A lookup in a child moves it too.
		child, err := vmo.CreateClone(SnapshotOnWrite, 0, env.ps)
		mustNotFail(t, "clone", err)
		_, err = child.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "child lookup", err)
		expectReclaim(t, env, "touched through the child", page, 0)
		pq.RotateReclaimQueues()
		expectReclaim(t, env, "rotated again", page, 1)
		_, err = child.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "child lookup again", err)
		expectReclaim(t, env, "touched through the child again", page, 0)
	})
}

// vmo_eviction_hints_test
func TestEvictionHintsMoveAPageAndAlwaysNeedSticks(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 2, false)
		expectReclaim(t, env, "new", pages[0], 0)
		// Not needed: the page goes to the isolate queue.
		mustNotFail(t, "don't need", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		ok, _ := pq.DebugPageIsReclaim(pages[0])
		expect(t, "not in a reclaim queue", ok, false)
		expect(t, "isolated", pq.DebugPageIsReclaimIsolate(pages[0]), true)
		// Always needed: the page goes to the first queue.
		mustNotFail(t, "always need", vmo.HintRange(env.ctx, 0, ps, AlwaysNeed))
		pages[0] = vmo.DebugGetPage(0)
		expect(t, "not isolated", pq.DebugPageIsReclaimIsolate(pages[0]), false)
		expectReclaim(t, env, "always needed", pages[0], 0)
		// It cannot be evicted.
		expect(t, "reclaimed fewer than two", reclaim(env, vmo, pages[0], 0, FollowHint) < 2, true)
		expectAttribution(t, "kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// Not needed again.
		mustNotFail(t, "don't need again", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		pages[0] = vmo.DebugGetPage(0)
		ok, _ = pq.DebugPageIsReclaim(pages[0])
		expect(t, "not in a reclaim queue again", ok, false)
		expect(t, "isolated again", pq.DebugPageIsReclaimIsolate(pages[0]), true)
		// Still not evicted: always need sticks.
		expect(t, "reclaimed fewer than two again", reclaim(env, vmo, pages[0], 0, FollowHint) < 2, true)
		expectAttribution(t, "kept again", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// The failed eviction moved it out of the isolate queue.
		expect(t, "out of isolation", pq.DebugPageIsReclaimIsolate(pages[0]), false)
		expectReclaim(t, env, "accessed", pages[0], 0)
		// It rotates as usual.
		pq.RotateReclaimQueues()
		expectReclaim(t, env, "rotated", pages[0], 1)
		_, err := vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "touch", err)
		expectReclaim(t, env, "touched", pages[0], 0)
		// Told to ignore the hint, eviction evicts.
		expect(t, "evicted ignoring the hint", reclaim(env, vmo, pages[0], 0, IgnoreHint) >= 1, true)
		expectAttribution(t, "evicted", vmo.GetAttributedMemoryInRange(0, ps), AttributionCounts{})
		// Supply again, and hint the second page always needed.
		pages = supplyPagerVmoPages(t, env, vmo, 0, 2)
		mustNotFail(t, "always need the second", vmo.HintRange(env.ctx, ps, ps, AlwaysNeed))
		pages[1] = vmo.DebugGetPage(ps)
		expect(t, "the second is always needed", pages[1].alwaysNeed, true)
	})
}

// vmo_eviction_hints_clone_test
func TestEvictionHintsThroughChildrenApplyToThePagesTheySee(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 2, false)
		expectReclaim(t, env, "first new", pages[0], 0)
		expectReclaim(t, env, "second new", pages[1], 0)
		clone, err := vmo.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone", err)
		isolated := func(what string, page *VmPage) {
			t.Helper()
			ok, _ := pq.DebugPageIsReclaim(page)
			expect(t, what+": not in a reclaim queue", ok, false)
			expect(t, what+": isolated", pq.DebugPageIsReclaimIsolate(page), true)
		}
		// Hints through the clone.
		mustNotFail(t, "don't need", clone.HintRange(env.ctx, 0, ps, DontNeed))
		isolated("don't need", pages[0])
		mustNotFail(t, "always need", clone.HintRange(env.ctx, 0, ps, AlwaysNeed))
		pages[0] = vmo.DebugGetPage(0)
		expect(t, "not isolated", pq.DebugPageIsReclaimIsolate(pages[0]), false)
		expectReclaim(t, env, "always needed", pages[0], 0)
		expect(t, "not evicted", reclaim(env, vmo, pages[0], 0, FollowHint) < 2, true)
		expectAttribution(t, "kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// Through a clone of the clone.
		clone2, err := clone.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "clone of the clone", err)
		mustNotFail(t, "don't need through the second", clone2.HintRange(env.ctx, 0, ps, DontNeed))
		isolated("don't need through the second", pages[0])
		mustNotFail(t, "always need through the second", clone2.HintRange(env.ctx, 0, ps, AlwaysNeed))
		pages[0] = vmo.DebugGetPage(0)
		expect(t, "not isolated through the second", pq.DebugPageIsReclaimIsolate(pages[0]), false)
		expectReclaim(t, env, "always needed through the second", pages[0], 0)
		expect(t, "not evicted through the second", reclaim(env, vmo, pages[0], 0, FollowHint) < 2, true)
		expectAttribution(t, "kept through the second", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// Supply the second page again, in case it was evicted.
		pages[1] = supplyPagerVmoPages(t, env, vmo, 1, 1)[0]
		ok, _ := pq.DebugPageIsReclaim(pages[1])
		expect(t, "the second page is reclaimable", ok, true)
		// Hints through the parent still work.
		mustNotFail(t, "don't need through the parent", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		isolated("don't need through the parent", pages[0])
		// Forking the page in the clone means its hints no longer reach it.
		mustNotFail(t, "write the clone", clone.Write(env.ctx, putUint64(0xff), 0))
		expectAttribution(t, "the clone's fork", clone.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, clone, ps)
		// The write accessed the page; isolate it again through the parent.
		mustNotFail(t, "don't need after the fork", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		isolated("don't need after the fork", pages[0])
		// Always need through the clone reaches its fork, not the page.
		mustNotFail(t, "always need through the clone", clone.HintRange(env.ctx, 0, ps, AlwaysNeed))
		isolated("always need through the clone", pages[0])
		// Through the second clone, made before the fork, it reaches the page.
		mustNotFail(t, "always need through the second after the fork", clone2.HintRange(env.ctx, 0, ps, AlwaysNeed))
		ok, _ = pq.DebugPageIsReclaim(pages[0])
		expect(t, "back in a reclaim queue", ok, true)
		expect(t, "not isolated after the fork", pq.DebugPageIsReclaimIsolate(pages[0]), false)
		// A clone made after the fork sees the fork.
		clone3, err := clone.CreateClone(SnapshotOnWrite, 0, 2*ps)
		mustNotFail(t, "third clone", err)
		mustNotFail(t, "don't need once more", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		isolated("don't need once more", pages[0])
		mustNotFail(t, "always need through the third", clone3.HintRange(env.ctx, 0, ps, AlwaysNeed))
		isolated("always need through the third", pages[0])
		// The second page was not forked, so the third clone sees it.
		expectReclaim(t, env, "the second page", pages[1], 0)
		mustNotFail(t, "don't need the second through the third", clone3.HintRange(env.ctx, ps, ps, DontNeed))
		isolated("the second page through the third", pages[1])
	})
}

// vmo_reclamation_test. Zircon's middle part, which reclaims around pinned
// pages, is not ported, as pinning is not.
func TestReclamationEvictsIsolatedPagesAndCountsTheEvent(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, pages := makeCommittedPagerVmo(t, env, 1, false)
		expectAttribution(t, "committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expect(t, "reclaimed", reclaim(env, vmo, pages[0], 0, FollowHint), uint64(1))
		expectAttribution(t, "reclaimed", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		expect(t, "reclamation events", vmo.ReclamationEventCount(), uint64(1))
		// An object with no page isolated fails as accessed.
		vmo, pages = makeCommittedPagerVmo(t, env, 2, false)
		for i := range pages {
			expect(t, "not reclaimable", env.node.PageQueues().IsPageReclaimable(pages[i]), false)
		}
		_, failure := vmo.DebugGetCowPages().ReclaimPage(env.ctx, pages[0], 0, FollowHint, nil)
		expect(t, "failure", failure, EvictAccessed)
	})
}

// vmo_attribution_clones_test. Zircon makes a full snapshot, which shares the
// original's pages with the clone through a hidden parent, and a slice. A
// snapshot-on-write child is attributed only what it commits, and its parent
// keeps all of its own; the slice's part, which committed pages in the
// original, commits them in the original directly.
func TestAttributionOfAParentAndItsChildCountsEachOnlyItsOwnPages(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 4*ps)
		mustNotFail(t, "create", err)
		expectAttribution(t, "new", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		mustNotFail(t, "commit two", vmo.CommitRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "two committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		// A clone of the second and third pages.
		clone, err := vmo.CreateClone(SnapshotOnWrite, ps, 2*ps)
		mustNotFail(t, "clone", err)
		expectAttribution(t, "parent after the clone", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		expectAttribution(t, "new clone", clone.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, clone, 0)
		// The clone commits both its pages.
		mustNotFail(t, "commit the clone", clone.CommitRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "parent after the clone's commit", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectAttribution(t, "clone committed", clone.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		expectContinuousAttribution(t, env, clone, 2*ps)
		// The original commits its last page.
		mustNotFail(t, "commit the last", vmo.CommitRange(env.ctx, 3*ps, ps))
		expectAttribution(t, "three committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(3*ps, 0))
		expectContinuousAttribution(t, env, vmo, 3*ps)
		// And the rest.
		mustNotFail(t, "commit all", vmo.CommitRange(env.ctx, 0, 4*ps))
		expectAttribution(t, "all committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		expectAttribution(t, "clone unchanged", clone.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 4*ps)
		expectContinuousAttribution(t, env, clone, 2*ps)
		clone.Destroy()
		expectAttribution(t, "clone gone", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		expectContinuousAttribution(t, env, vmo, 4*ps)
	})
}

// vmo_attribution_pager_test
func TestAttributionFollowsPagesTakenSuppliedAndCopiedIntoAChild(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const numPages = 2
		allocSize := numPages * ps
		vmo := makeUncommittedPagerVmo(t, env, numPages, false)
		expectAttribution(t, "new", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		aux, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create aux", err)
		expectAttribution(t, "new aux", aux.GetAttributedMemory(), AttributionCounts{})
		mustNotFail(t, "commit aux", aux.CommitRange(env.ctx, 0, allocSize))
		expectAttribution(t, "aux committed", aux.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, aux, 2*ps)
		pageList := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, ps, pageList))
		expectAttribution(t, "aux after the take", aux.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, aux, ps)
		expectAttribution(t, "vmo before the supply", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, ps, pageList, PagerSupply))
		expectAttribution(t, "vmo supplied", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expectAttribution(t, "aux after the supply", aux.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, aux, ps)
		aux.Destroy()
		// A child that sees the first page.
		clone, err := vmo.CreateClone(SnapshotOnWrite, 0, ps)
		mustNotFail(t, "clone", err)
		expectAttribution(t, "vmo after the clone", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expectAttribution(t, "new clone", clone.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, clone, 0)
		mustNotFail(t, "commit the clone", clone.CommitRange(env.ctx, 0, ps))
		expectAttribution(t, "vmo after the clone's commit", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expectAttribution(t, "clone committed", clone.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, clone, ps)
		clone.Destroy()
		expectAttribution(t, "vmo alone", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
	})
}

// vmo_attribution_dedup_test
func TestDedupingZeroPagesDropsTheirAttribution(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		expectAttribution(t, "new", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		page, err := vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get the first", err)
		expect(t, "deduped the first", vmo.DebugGetCowPages().DedupZeroPage(page, 0), true)
		expectAttribution(t, "first deduped", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		page, err = vmo.GetPageBlocking(env.ctx, ps, 0)
		mustNotFail(t, "get the second", err)
		expect(t, "deduped the second", vmo.DebugGetCowPages().DedupZeroPage(page, ps), true)
		expectAttribution(t, "both deduped", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		mustNotFail(t, "commit again", vmo.CommitRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "committed again", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
	})
}

// vmo_attribution_compression_test
func TestCompressingAndDecompressingMovesAttributionAndCountsReclamation(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		expectAttribution(t, "new", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		reclamationCount := vmo.ReclamationEventCount()
		// A commit is no reclamation.
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "committed", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		expect(t, "reclamations after the commit", vmo.ReclamationEventCount(), reclamationCount)
		mustNotFail(t, "write", vmo.Write(env.ctx, putUint64(42), 0))
		expect(t, "reclamations after the write", vmo.ReclamationEventCount(), reclamationCount)
		// Compress the first page.
		page, err := vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get the first", err)
		expect(t, "compressed the first", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "first compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, ps))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		expect(t, "reclamations after the first", vmo.ReclamationEventCount(), reclamationCount+1)
		// The second, zeros, is not kept at all.
		page, err = vmo.GetPageBlocking(env.ctx, ps, 0)
		mustNotFail(t, "get the second", err)
		expect(t, "compressed the second", compressPage(t, env, vmo.DebugGetCowPages(), page, ps), uint64(1))
		expectAttribution(t, "both compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		expect(t, "reclamations after the second", vmo.ReclamationEventCount(), reclamationCount+2)
		// Reading the first decompresses it.
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagHwFault)
		mustNotFail(t, "read the first", err)
		expectAttribution(t, "first decompressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expect(t, "reclamations after the read", vmo.ReclamationEventCount(), reclamationCount+2)
		// Reading the second gets the zero page.
		page, err = vmo.GetPageBlocking(env.ctx, ps, PfFlagHwFault)
		mustNotFail(t, "read the second", err)
		expect(t, "the zero page", page, env.pmm.ZeroPage())
		expectAttribution(t, "second read", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		expect(t, "reclamations after the second read", vmo.ReclamationEventCount(), reclamationCount+2)
	})
}

// vmo_lookup_compressed_pages_test
func TestOnlyAFaultDecompressesAPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, putUint64(42), 0))
		expectAttribution(t, "written", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		page, err := vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get page", err)
		expect(t, "compressed", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		// A lookup that is not a fault neither finds nor decompresses it.
		_, err = vmo.GetPageBlocking(env.ctx, 0, 0)
		expect(t, "read lookup", err, ErrNotFound)
		expectAttribution(t, "after the read lookup", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagWrite)
		expect(t, "write lookup", err, ErrNotFound)
		expectAttribution(t, "after the write lookup", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		// A fault does.
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagHwFault)
		mustNotFail(t, "read fault", err)
		expectAttribution(t, "after the read fault", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
		page, err = vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get page again", err)
		expect(t, "compressed again", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "compressed again", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagWrite|PfFlagSwFault)
		mustNotFail(t, "write fault", err)
		expectAttribution(t, "after the write fault", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
	})
}

// vmo_write_does_not_commit_test. Zircon makes a full snapshot; a
// snapshot-on-write child reads its parent's page the same way.
func TestALookupToWriteAChildsParentPageNeedsAFault(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		vmo, err := CreateObjectPaged(env.node, env.ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, putUint64(42), 0))
		clone, err := vmo.CreateClone(SnapshotOnWrite, 0, env.ps)
		mustNotFail(t, "clone", err)
		// The parent's page reads in the clone.
		_, err = clone.GetPageBlocking(env.ctx, 0, 0)
		expectNoError(t, "read", err)
		// Without a fault, nothing is committed in the clone to write.
		_, err = clone.GetPageBlocking(env.ctx, 0, PfFlagWrite)
		expect(t, "write without a fault", err, ErrNotFound)
		// With one, it is.
		_, err = clone.GetPageBlocking(env.ctx, 0, PfFlagWrite|PfFlagSwFault)
		expectNoError(t, "write fault", err)
	})
}

// vmo_dirty_pages_test
func TestADirtiedPageIsInTheDirtyQueueAndNotEvicted(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 1, true)
		page := pages[0]
		expectReclaim(t, env, "new", page, 0)
		pq.RotateReclaimQueues()
		expectReclaim(t, env, "rotated", page, 1)
		_, err := vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "touch", err)
		expectReclaim(t, env, "touched", page, 0)
		// A write moves it to the dirty queue.
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, ps))
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "not reclaimable", ok, false)
		expect(t, "dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "dirty count", pq.QueueCounts().PagerBackedDirty, 1)
		// It cannot be evicted.
		expect(t, "reclaimed", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// An access does not move it out of the dirty queue.
		_, err = vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		expectNoError(t, "touch again", err)
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "still not reclaimable", ok, false)
		expect(t, "still dirty", pq.DebugPageIsPagerBackedDirty(page), true)
	})
}

// vmo_dirty_pages_writeback_test. The plan expects D1 to change this case
// most, but as Zircon wrote it no store comes while the page is
// AwaitingClean, so it expects what Zircon does; the store into an
// AwaitingClean page is TestAStoreIntoAPageACheckpointHoldsGetsADirtyCopy.
func TestAWritebackCleansADirtyPageSoItCanBeEvicted(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 1, true)
		page := pages[0]
		expectReclaim(t, env, "new", page, 0)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, ps))
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "dirty: not reclaimable", ok, false)
		expect(t, "dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "dirty: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "dirty: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// The writeback begins; the page stays in the dirty queue.
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "awaiting clean: not reclaimable", ok, false)
		expect(t, "awaiting clean: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "awaiting clean: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "awaiting clean: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// An access does not move it either.
		_, err := vmo.GetPageBlocking(env.ctx, 0, PfFlagSwFault)
		mustNotFail(t, "touch", err)
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "touched: not reclaimable", ok, false)
		expect(t, "touched: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "touched: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "touched: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// The writeback ends; the page leaves the dirty queue.
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "clean: not in the dirty queue", pq.DebugPageIsPagerBackedDirty(page), false)
		expectReclaim(t, env, "clean", page, 0)
		pq.RotateReclaimQueues()
		expectReclaim(t, env, "rotated", page, 1)
		// Another write makes it dirty again.
		mustNotFail(t, "dirty again", vmo.DirtyPages(env.ctx, 0, ps))
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "dirty again: not reclaimable", ok, false)
		expect(t, "dirty again: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		// Clean it again, and evict it.
		mustNotFail(t, "begin again", vmo.WritebackBegin(0, ps, false))
		mustNotFail(t, "end again", vmo.WritebackEnd(0, ps))
		expect(t, "clean again: not in the dirty queue", pq.DebugPageIsPagerBackedDirty(page), false)
		expectReclaim(t, env, "clean again", page, 0)
		expect(t, "evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(1))
		expectAttribution(t, "evicted", vmo.GetAttributedMemoryInRange(0, ps), AttributionCounts{})
	})
}

// vmo_dirty_pages_with_hints_test
func TestHintsLeaveADirtyPageInTheDirtyQueueAndAlwaysNeedOutlivesTheWriteback(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 1, true)
		page := pages[0]
		expectReclaim(t, env, "new", page, 0)
		mustNotFail(t, "dirty", vmo.DirtyPages(env.ctx, 0, ps))
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "dirty: not reclaimable", ok, false)
		expect(t, "dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		// Don't need leaves it dirty.
		mustNotFail(t, "don't need", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		expect(t, "don't need: not isolated", pq.DebugPageIsReclaimIsolate(page), false)
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "don't need: not reclaimable", ok, false)
		expect(t, "don't need: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "don't need: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "don't need: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		// So does always need.
		mustNotFail(t, "always need", vmo.HintRange(env.ctx, 0, ps, AlwaysNeed))
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "always need: not reclaimable", ok, false)
		expect(t, "always need: not isolated", pq.DebugPageIsReclaimIsolate(page), false)
		expect(t, "always need: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		// Clean it.
		mustNotFail(t, "begin", vmo.WritebackBegin(0, ps, false))
		mustNotFail(t, "end", vmo.WritebackEnd(0, ps))
		expect(t, "clean: not in the dirty queue", pq.DebugPageIsPagerBackedDirty(page), false)
		expectReclaim(t, env, "clean", page, 0)
		// Always need still stops eviction.
		expect(t, "always needed: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "always needed: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
		expect(t, "always needed: not in the dirty queue", pq.DebugPageIsPagerBackedDirty(page), false)
		expectReclaim(t, env, "always needed", page, 0)
		// Unless the hint is ignored.
		expect(t, "evicted ignoring the hint", reclaim(env, vmo, page, 0, IgnoreHint), uint64(1))
		expectAttribution(t, "evicted", vmo.GetAttributedMemoryInRange(0, ps), AttributionCounts{})
		// Again, dirtying the page after the hint.
		vmo.Destroy()
		vmo, pages = makeCommittedPagerVmo(t, env, 1, true)
		page = pages[0]
		expectReclaim(t, env, "new again", page, 0)
		mustNotFail(t, "don't need first", vmo.HintRange(env.ctx, 0, ps, DontNeed))
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "isolated: not reclaimable", ok, false)
		expect(t, "isolated: not dirty", pq.DebugPageIsPagerBackedDirty(page), false)
		expect(t, "isolated", pq.DebugPageIsReclaimIsolate(page), true)
		mustNotFail(t, "dirty after the hint", vmo.DirtyPages(env.ctx, 0, ps))
		ok, _ = pq.DebugPageIsReclaim(page)
		expect(t, "dirty after the hint: not reclaimable", ok, false)
		expect(t, "dirty after the hint: not isolated", pq.DebugPageIsReclaimIsolate(page), false)
		expect(t, "dirty after the hint: dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		expect(t, "dirty after the hint: not evicted", reclaim(env, vmo, page, 0, FollowHint), uint64(0))
		expectAttribution(t, "dirty after the hint: kept", vmo.GetAttributedMemoryInRange(0, ps), PrivateAttributionCounts(ps, 0))
	})
}

// vmo_supply_compressed_pages_test
func TestAPagerSupplyOfACompressedPageDecompressesIt(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmop := makeUncommittedPagerVmo(t, env, 1, false)
		vmo, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", vmo.Write(env.ctx, putUint64(42), 0))
		page, err := vmo.GetPageBlocking(env.ctx, 0, 0)
		mustNotFail(t, "get page", err)
		expect(t, "compressed", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expectAttribution(t, "compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(0, ps))
		expectContinuousAttribution(t, env, vmo, ps)
		pl := NewPageSpliceList[VmPage](ps, env.node)
		mustNotFail(t, "take", vmo.TakePages(env.ctx, 0, ps, pl))
		expectAttribution(t, "taken", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		// The pager backed object holds no compressed page after the supply.
		mustNotFail(t, "supply", vmop.SupplyPages(env.ctx, 0, ps, pl, PagerSupply))
		expectAttribution(t, "supplied", vmop.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmop, ps)
		got := make([]byte, 8)
		mustNotFail(t, "read", vmop.Read(env.ctx, got, 0))
		expect(t, "the bytes", string(got), string(putUint64(42)))
	})
}

// vmo_dedup_dirty_test
func TestAZeroPageCanBeDedupedOnlyWhileClean(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		vmo, pages := makeCommittedPagerVmo(t, env, 1, false)
		page := pages[0]
		ok, _ := pq.DebugPageIsReclaim(page)
		expect(t, "reclaimable", ok, true)
		// It is clean, so it dedups.
		expect(t, "deduped", vmo.DebugGetCowPages().DedupZeroPage(page, 0), true)
		expectAttribution(t, "deduped", vmo.GetAttributedMemory(), AttributionCounts{})
		expectContinuousAttribution(t, env, vmo, 0)
		// A write makes it dirty.
		mustNotFail(t, "write", vmo.Write(env.ctx, []byte{0xff}, 0))
		page = vmo.DebugGetPage(0)
		expect(t, "dirty queue", pq.DebugPageIsPagerBackedDirty(page), true)
		// So it does not dedup.
		expect(t, "not deduped", vmo.DebugGetCowPages().DedupZeroPage(page, 0), false)
		expectAttribution(t, "kept", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, 0))
		expectContinuousAttribution(t, env, vmo, ps)
	})
}

// vmo_prefetch_compressed_pages_test
func TestPrefetchingDecompressesPages(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		vmo, err := CreateObjectPaged(env.node, 2*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the first", vmo.Write(env.ctx, putUint64(42), 0))
		mustNotFail(t, "write the second", vmo.Write(env.ctx, putUint64(42), ps))
		expectAttribution(t, "written", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		page, err := vmo.GetPageBlocking(env.ctx, ps, 0)
		mustNotFail(t, "get page", err)
		expect(t, "compressed", compressPage(t, env, vmo.DebugGetCowPages(), page, ps), uint64(1))
		expectAttribution(t, "compressed", vmo.GetAttributedMemory(), PrivateAttributionCounts(ps, ps))
		expectContinuousAttribution(t, env, vmo, 2*ps)
		mustNotFail(t, "prefetch", vmo.PrefetchRange(env.ctx, 0, 2*ps))
		expectAttribution(t, "prefetched", vmo.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectContinuousAttribution(t, env, vmo, 2*ps)
	})
}

// vmo_skip_range_update_test. Zircon makes a full snapshot and updates the
// hidden parent. A snapshot-on-write child's parent is the original, which
// is updated instead; the child's own pages skip the update the same way.
func TestARangeUpdateSkipsTheRangesAChildHoldsItself(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const numPages = 16
		vmo, err := CreateObjectPaged(env.node, numPages*ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, numPages*ps))
		child, err := vmo.CreateClone(SnapshotOnWrite, 0, numPages*ps)
		mustNotFail(t, "clone", err)
		// Fork some pages into the child, which then need no update.
		for _, page := range []uint64{4, 5, 6, 10, 11, 12} {
			mustNotFail(t, "fork", child.Write(env.ctx, putUint64(42), page*ps))
		}
		mapping := newTestMapping(t, env, child)
		defer mapping.unmap()
		testRanges := []struct {
			pageStart, numPages uint64
			unmapped            []uint64
		}{
			// A range the child does not hold is unmapped.
			{0, 1, []uint64{0}},
			// Ranges the child holds are not.
			{4, 1, nil},
			{4, 3, nil},
			{6, 1, nil},
			// A range partly over one the child holds is trimmed.
			{3, 2, []uint64{3}},
			{6, 2, []uint64{7}},
			// A range over one gap is trimmed to it at both ends.
			{4, 9, []uint64{7, 8, 9}},
			// A range across a held range still unmaps it.
			{3, 10, []uint64{3, 4, 5, 6, 7, 8, 9}},
			{4, 10, []uint64{7, 8, 9, 10, 11, 12, 13}},
			{3, 11, []uint64{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}},
		}
		for _, r := range testRanges {
			// Every page starts mapped.
			for i := range uint64(numPages) {
				mapping.fault(i*ps, false)
			}
			vmo.RangeChangeUpdate(CowRange{r.pageStart * ps, r.numPages * ps}, Unmap)
			for i := range uint64(numPages) {
				_, _, mapped := mapping.query(i * ps)
				want := true
				for _, u := range r.unmapped {
					if u == i {
						want = false
					}
				}
				if mapped != want {
					t.Errorf("range %d+%d: page %d mapped %v, want %v", r.pageStart, r.numPages, i, mapped, want)
				}
			}
		}
	})
}

// vmo_zero_marker_transfer_test. Zircon compresses a zero page in a hidden
// parent, which leaves a marker there, and then transfers data over it. A
// snapshot-on-write child whose zero page is compressed while its parent
// holds a page there gets the marker, and the transfer overwrites it.
func TestATransferOverwritesAZeroMarker(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		allocSize := env.ps
		parent, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write the parent", parent.Write(env.ctx, putUint64(7), 0))
		vmo, err := parent.CreateClone(SnapshotOnWrite, 0, allocSize)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "zero the child's page", vmo.Write(env.ctx, make([]byte, allocSize), 0))
		page := vmo.DebugGetPage(0)
		expect(t, "compressed", compressPage(t, env, vmo.DebugGetCowPages(), page, 0), uint64(1))
		expect(t, "a marker", vmo.DebugGetCowPages().DebugIsMarker(0), true)
		aux, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create aux", err)
		mustNotFail(t, "write aux", aux.Write(env.ctx, putUint64(9), 0))
		pages := NewPageSpliceList[VmPage](env.ps, env.node)
		mustNotFail(t, "take", aux.TakePages(env.ctx, 0, allocSize, pages))
		mustNotFail(t, "transfer", vmo.SupplyPages(env.ctx, 0, allocSize, pages, TransferData))
		expect(t, "a page", vmo.DebugGetCowPages().DebugIsPage(0), true)
		got := make([]byte, 8)
		mustNotFail(t, "read", vmo.Read(env.ctx, got, 0))
		expect(t, "the bytes transferred", string(got), string(putUint64(9)))
	})
}

// vmo_pager_supply_test. Zircon's second level clone is a modified snapshot
// of a child, made through a hidden node that the two then share. A
// snapshot-on-write child of the clone sees the clone's pages without being
// attributed them, and the clone keeps them private.
func TestSuppliesFillAPagerObjectAndTransfersReplaceAChildsPages(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const numPages = 4
		allocSize := numPages * ps
		halfSize := allocSize / 2
		aux, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create aux", err)
		vmo := makeUncommittedPagerVmo(t, env, numPages, false)
		// Two pages of one pattern.
		bufRand1 := make([]byte, halfSize)
		fillRegion(0x77, bufRand1)
		mustNotFail(t, "write aux", aux.Write(env.ctx, bufRand1, 0))
		sl := NewPageSpliceList[VmPage](ps, env.node)
		expectNoError(t, "take", aux.TakePages(env.ctx, 0, halfSize, sl))
		mustNotFail(t, "supply", vmo.SupplyPages(env.ctx, 0, halfSize, sl, PagerSupply))
		expect(t, "processed", sl.IsProcessed(), true)
		// Four of another.
		bufRand2 := make([]byte, allocSize)
		fillRegion(0x88, bufRand2)
		expectNoError(t, "write aux again", aux.Write(env.ctx, bufRand2, 0))
		sl2 := NewPageSpliceList[VmPage](ps, env.node)
		expectNoError(t, "take again", aux.TakePages(env.ctx, 0, allocSize, sl2))
		mustNotFail(t, "supply again", vmo.SupplyPages(env.ctx, 0, allocSize, sl2, PagerSupply))
		expect(t, "processed again", sl2.IsProcessed(), true)
		bufCheck := make([]byte, allocSize)
		expectNoError(t, "read", vmo.Read(env.ctx, bufCheck, 0))
		// The first two were not overwritten.
		expect(t, "first half", bytes.Equal(bufRand1, bufCheck[:halfSize]), true)
		// The second two are new.
		expect(t, "second half", bytes.Equal(bufRand2[halfSize:], bufCheck[halfSize:]), true)
		expectAttribution(t, "supplied", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		// A modified snapshot of an object with no parent is a
		// snapshot-on-write child.
		clone, err := vmo.CreateClone(SnapshotModified, 0, allocSize)
		mustNotFail(t, "clone", err)
		expectAttribution(t, "vmo after the clone", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		expectAttribution(t, "new clone", clone.GetAttributedMemory(), PrivateAttributionCounts(0, 0))
		// Two pages into the middle of the clone.
		bufRand3 := make([]byte, allocSize)
		fillRegion(0x99, bufRand3)
		expectNoError(t, "write aux a third time", aux.Write(env.ctx, bufRand3, 0))
		sl3 := NewPageSpliceList[VmPage](ps, env.node)
		expectNoError(t, "take a third time", aux.TakePages(env.ctx, ps, halfSize, sl3))
		mustNotFail(t, "transfer", clone.SupplyPages(env.ctx, ps, halfSize, sl3, TransferData))
		expect(t, "processed a third time", sl3.IsProcessed(), true)
		expectAttribution(t, "clone after the transfer", clone.GetAttributedMemory(), PrivateAttributionCounts(2*ps, 0))
		expectAttribution(t, "vmo after the transfer", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		expectNoError(t, "read the clone", clone.Read(env.ctx, bufCheck, 0))
		// The first and last pages read from the parent.
		expect(t, "clone's first page", bytes.Equal(bufRand1[:ps], bufCheck[:ps]), true)
		expect(t, "clone's last page", bytes.Equal(bufRand2[allocSize-ps:], bufCheck[allocSize-ps:]), true)
		// The middle ones are new.
		expect(t, "clone's middle", bytes.Equal(bufRand3[ps:ps+halfSize], bufCheck[ps:ps+halfSize]), true)
		// The parent is unchanged.
		expectNoError(t, "read the vmo", vmo.Read(env.ctx, bufCheck, 0))
		expect(t, "vmo's first half", bytes.Equal(bufRand1, bufCheck[:halfSize]), true)
		expect(t, "vmo's second half", bytes.Equal(bufRand2[halfSize:], bufCheck[halfSize:]), true)
		// New data in every page of the clone.
		bufRand4 := make([]byte, allocSize)
		fillRegion(0x99, bufRand4)
		expectNoError(t, "write aux a fourth time", aux.Write(env.ctx, bufRand4, 0))
		sl4 := NewPageSpliceList[VmPage](ps, env.node)
		expectNoError(t, "take a fourth time", aux.TakePages(env.ctx, 0, allocSize, sl4))
		mustNotFail(t, "transfer all", clone.SupplyPages(env.ctx, 0, allocSize, sl4, TransferData))
		expect(t, "processed a fourth time", sl4.IsProcessed(), true)
		expectNoError(t, "read the clone again", clone.Read(env.ctx, bufCheck, 0))
		expect(t, "the clone's new data", bytes.Equal(bufRand4, bufCheck), true)
		expectNoError(t, "read the vmo again", vmo.Read(env.ctx, bufCheck, 0))
		expect(t, "vmo's first half again", bytes.Equal(bufRand1, bufCheck[:halfSize]), true)
		expect(t, "vmo's second half again", bytes.Equal(bufRand2[halfSize:], bufCheck[halfSize:]), true)
		expectAttribution(t, "clone has four", clone.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		expectAttribution(t, "vmo has four", vmo.GetAttributedMemory(), PrivateAttributionCounts(4*ps, 0))
		// A clone of the clone. A modified snapshot of a child needs a
		// hidden node, and is refused.
		_, err = clone.CreateClone(SnapshotModified, 0, allocSize)
		expect(t, "modified snapshot of a child", err, ErrNotSupported)
		clone2, err := clone.CreateClone(SnapshotOnWrite, 0, allocSize)
		mustNotFail(t, "clone of the clone", err)
		expect(t, "clone keeps its pages private", clone.GetAttributedMemory().TotalPrivateBytes(), 4*ps)
		expect(t, "clone2 is attributed nothing", clone2.GetAttributedMemory().TotalPrivateBytes(), uint64(0))
		// Two pages into the second clone.
		bufRand5 := make([]byte, allocSize)
		fillRegion(0xaa, bufRand5)
		expectNoError(t, "write aux a fifth time", aux.Write(env.ctx, bufRand5, 0))
		sl5 := NewPageSpliceList[VmPage](ps, env.node)
		expectNoError(t, "take a fifth time", aux.TakePages(env.ctx, 0, halfSize, sl5))
		mustNotFail(t, "transfer to the second clone", clone2.SupplyPages(env.ctx, 0, halfSize, sl5, TransferData))
		expect(t, "processed a fifth time", sl5.IsProcessed(), true)
		expect(t, "clone2 has two private", clone2.GetAttributedMemory().TotalPrivateBytes(), 2*ps)
		expect(t, "clone still has four", clone.GetAttributedMemory().TotalPrivateBytes(), 4*ps)
		expectNoError(t, "read the second clone", clone2.Read(env.ctx, bufCheck, 0))
		expect(t, "clone2's own half", bytes.Equal(bufRand5[:halfSize], bufCheck[:halfSize]), true)
		expect(t, "clone2 sees the clone's other half", bytes.Equal(bufRand4[halfSize:], bufCheck[halfSize:]), true)
	})
}

// vmo_compress_to_marker_pager_test. Zircon compresses a zero page that two
// children share in their hidden node, to a marker with a share count of
// one. A snapshot-on-write child holds its zero page alone; compressed it
// becomes a marker of share count zero, since the parent a pager backs shows
// through, and a write replaces the marker.
func TestAZeroPageOverAPagerParentCompressesToAMarkerAWriteReplaces(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const numPages = 2
		vmoSize := ps * numPages
		zeroBuff := make([]byte, vmoSize)
		val := putUint64(42)[:4]
		vmo, _ := makeCommittedPagerVmo(t, env, numPages, false)
		// A modified snapshot of an object with no parent is a
		// snapshot-on-write child.
		clone1, err := vmo.CreateClone(SnapshotModified, 0, vmoSize)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write zeros", clone1.Write(env.ctx, zeroBuff, 0))
		clone2, err := clone1.CreateClone(SnapshotOnWrite, 0, vmoSize)
		mustNotFail(t, "clone of the clone", err)
		cow := clone1.DebugGetCowPages()
		expect(t, "no parent content markers", cow.treeHasParentContentMarkers(), false)
		expect(t, "a page", cow.DebugIsPage(0), true)
		expect(t, "not shared", cow.DebugGetPage(0).shareCount, uint32(0))
		page := cow.DebugGetPage(0)
		expect(t, "compressed", compressPage(t, env, cow, page, 0), uint64(1))
		expect(t, "a marker", cow.DebugIsMarker(0), true)
		expect(t, "the marker's share count", cow.pageList.Lookup(0).GetMarkerShareCount(), uint32(0))
		mustNotFail(t, "write the clone", clone1.Write(env.ctx, val, 0))
		expect(t, "a page again", cow.DebugIsPage(0), true)
		got := make([]byte, 4)
		mustNotFail(t, "read the second clone", clone2.Read(env.ctx, got, 0))
		expect(t, "the second clone sees the write", string(got), string(val))
		clone2.Destroy()
		clone1.Destroy()
		vmo.Destroy()
	})
}

// vmo_compress_to_marker_anon_test. As the pager case, over an anonymous
// parent: Zircon's hidden node holds the marker there.
func TestAZeroPageOverAnAnonymousParentCompressesToAMarkerAWriteReplaces(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const numPages = 2
		vmoSize := ps * numPages
		zeroBuff := make([]byte, vmoSize)
		val := putUint64(42)[:4]
		anonVmo, err := CreateObjectPaged(env.node, vmoSize)
		mustNotFail(t, "create", err)
		expectNoError(t, "write the first", anonVmo.Write(env.ctx, val, 0))
		expectNoError(t, "write the second", anonVmo.Write(env.ctx, val, ps))
		anonClone1, err := anonVmo.CreateClone(SnapshotModified, 0, vmoSize)
		mustNotFail(t, "clone", err)
		mustNotFail(t, "write zeros", anonClone1.Write(env.ctx, zeroBuff, 0))
		anonClone2, err := anonClone1.CreateClone(SnapshotOnWrite, 0, vmoSize)
		mustNotFail(t, "clone of the clone", err)
		cow := anonClone1.DebugGetCowPages()
		expect(t, "no parent content markers", cow.treeHasParentContentMarkers(), false)
		expect(t, "a page", cow.DebugIsPage(0), true)
		page := cow.DebugGetPage(0)
		expect(t, "compressed", compressPage(t, env, cow, page, 0), uint64(1))
		expect(t, "a marker", cow.DebugIsMarker(0), true)
		expect(t, "the marker's share count", cow.pageList.Lookup(0).GetMarkerShareCount(), uint32(0))
		// The second clone reads the marker's zeros, not the parent's page.
		got := make([]byte, 4)
		mustNotFail(t, "read the second clone", anonClone2.Read(env.ctx, got, 0))
		expect(t, "zeros through the marker", string(got), string(make([]byte, 4)))
		// A write replaces the marker.
		mustNotFail(t, "write the clone", anonClone1.Write(env.ctx, val, 0))
		expect(t, "a page again", cow.DebugIsPage(0), true)
		// A write to the second clone forks it there.
		mustNotFail(t, "write the second clone", anonClone2.Write(env.ctx, val, 0))
		expect(t, "the second clone's page", anonClone2.DebugGetCowPages().DebugIsPage(0), true)
		anonClone1.Destroy()
		anonClone2.Destroy()
		anonVmo.Destroy()
	})
}

// vmo_lookup_readable_simple_test
func TestLookupReadableFindsEveryCommittedPage(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		const pageCount = 4
		allocSize := env.ps * pageCount
		vmo, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", vmo.CommitRange(env.ctx, 0, allocSize))
		pagesSeen := 0
		err = vmo.DebugGetCowPages().DebugLookupReadable(CowRange{0, allocSize}, func(uint64, *VmPage) error {
			pagesSeen++
			return nil
		})
		expectNoError(t, "lookup readable", err)
		expect(t, "pages seen", pagesSeen, pageCount)
	})
}

// vmo_lookup_readable_clone_test. Zircon makes a full snapshot; a
// snapshot-on-write child reads its parent's pages the same way.
func TestLookupReadableOfAChildFindsItsParentsPagesAroundItsOwn(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		const pageCount = 4
		allocSize := ps * pageCount
		parent, err := CreateObjectPaged(env.node, allocSize)
		mustNotFail(t, "create", err)
		mustNotFail(t, "commit", parent.CommitRange(env.ctx, 0, allocSize))
		clone, err := parent.CreateClone(SnapshotOnWrite, 0, allocSize)
		mustNotFail(t, "clone", err)
		// A page of the clone's own splits the parent's run.
		mustNotFail(t, "commit one in the clone", clone.CommitRange(env.ctx, ps, ps))
		offsets := []uint64{}
		err = clone.DebugGetCowPages().DebugLookupReadable(CowRange{0, allocSize}, func(offset uint64, _ *VmPage) error {
			offsets = append(offsets, offset)
			return nil
		})
		expectNoError(t, "lookup readable", err)
		expectOffsets(t, "pages seen", offsets, []uint64{0, 1, 2, 3}, ps)
	})
}
