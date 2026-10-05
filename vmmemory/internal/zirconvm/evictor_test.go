// Copyright 2021 The Fuchsia Authors
// Ported from zircon/kernel/vm/unittests/evictor_unittest.cc
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"testing/synctest"
)

// testPmmNode is the node a case's evictor reclaims from, Zircon's
// TestPmmNode. Each reclaim frees one page a pager backs, until a cap, and
// the node counts its free pages, which Zircon's global pmm cannot be made
// to do.
type testPmmNode struct {
	evictor        *Evictor[struct{}]
	freePages      uint64
	totalEvictions uint64
	maxEvictions   uint64
	// failAfter fails the reclaim after that many evictions with
	// errReclaimFailed, where it is not zero.
	failAfter uint64
}

// errReclaimFailed is a reclaim that failed for good.
var errReclaimFailed = errors.New("the reclaim failed")

// testEvictorPageSize is the page size a node's targets in bytes are
// converted at.
const testEvictorPageSize = 4 << 10

func newTestPmmNode() *testPmmNode {
	node := &testPmmNode{maxEvictions: math.MaxUint64}
	node.evictor = NewEvictorWith(testEvictorPageSize, node.testReclaim, func() uint64 { return node.freePages })
	node.evictor.EnableEviction(true)
	return node
}

func (n *testPmmNode) testReclaim(context.Context, struct{}, bool, EvictionLevel) (ReclaimAttempt, bool, error) {
	if n.failAfter != 0 && n.totalEvictions >= n.failAfter {
		return ReclaimAttempt{}, false, errReclaimFailed
	}
	if n.totalEvictions >= n.maxEvictions {
		return ReclaimAttempt{}, false, nil
	}
	n.freePages++
	n.totalEvictions++
	return ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}}, true, nil
}

// evictFromPreloadedTarget evicts to the node's set target.
func (n *testPmmNode) evictFromPreloadedTarget(t *testing.T) EvictedPageCounts {
	t.Helper()
	counts, err := n.evictor.evictFromPreloadedTarget(t.Context())
	mustNotFail(t, "evict", err)
	return counts
}

func expectTarget(t *testing.T, what string, got, want EvictionTarget) {
	t.Helper()
	if got != want {
		t.Errorf("%s: the target is %+v, want %+v", what, got, want)
	}
}

// evictor_set_target_test. Zircon draws the target at random; here every
// pending flag and level is tried with fixed counts.
func TestATargetCombinedWithNoneIsThatTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, pending := range []bool{false, true} {
			for _, level := range []EvictionLevel{OnlyOldest, IncludeNewest} {
				node := newTestPmmNode()
				expected := EvictionTarget{Pending: pending, FreePagesTarget: 1804289383, MinPagesToFree: 846930886, Level: level}
				node.evictor.combineEvictionTarget(expected)
				expectTarget(t, "combined with none", node.evictor.debugGetEvictionTarget(), expected)
			}
		}
	})
}

// evictor_combine_targets_test. Zircon draws the counts at random.
func TestCombinedTargetsAddTheirMinimumsAndTakeTheLargestFreeTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		targets := []EvictionTarget{
			{Pending: true, FreePagesTarget: 383, MinPagesToFree: 886, Level: IncludeNewest},
			{Pending: true, FreePagesTarget: 777, MinPagesToFree: 915, Level: IncludeNewest},
			{Pending: true, FreePagesTarget: 793, MinPagesToFree: 335, Level: IncludeNewest},
			{Pending: true, FreePagesTarget: 386, MinPagesToFree: 492, Level: IncludeNewest},
			{Pending: true, FreePagesTarget: 649, MinPagesToFree: 421, Level: IncludeNewest},
		}
		for _, target := range targets {
			node.evictor.combineEvictionTarget(target)
		}
		expectTarget(t, "five combined", node.evictor.debugGetEvictionTarget(),
			EvictionTarget{Pending: true, FreePagesTarget: 793, MinPagesToFree: 3049, Level: IncludeNewest})
		// The level is the larger one, and a pending target stays pending.
		node.evictor.combineEvictionTarget(EvictionTarget{Level: OnlyOldest, PrintCounts: true})
		expectTarget(t, "with one not pending", node.evictor.debugGetEvictionTarget(),
			EvictionTarget{Pending: true, FreePagesTarget: 793, MinPagesToFree: 3049, Level: IncludeNewest, PrintCounts: true})
	})
}

// evictor_pager_backed_test. Zircon also expects no discardable pages, which
// are not ported.
func TestAnEvictionFreesTheLargerOfItsTwoTargets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		target := EvictionTarget{Pending: true, FreePagesTarget: 20, MinPagesToFree: 10, Level: IncludeNewest}
		// The node starts off with zero pages.
		expect(t, "free at first", node.freePages, uint64(0))
		node.evictor.combineEvictionTarget(target)
		counts := node.evictFromPreloadedTarget(t)
		// The free target was the larger, so exactly it was evicted.
		expect(t, "evicted for the free target", counts, EvictedPageCounts{PagerBacked: 20})
		expect(t, "free after the free target", node.freePages, uint64(20))

		target = EvictionTarget{Pending: true, FreePagesTarget: 10, MinPagesToFree: 20, Level: IncludeNewest}
		node.evictor.combineEvictionTarget(target)
		counts = node.evictFromPreloadedTarget(t)
		// The minimum was the larger, so exactly it was evicted.
		expect(t, "evicted for the minimum", counts, EvictedPageCounts{PagerBacked: 20})
		expect(t, "free after the minimum", node.freePages, uint64(40))
	})
}

// evictor_free_target_test
func TestAnEvictionStopsAtItsFreeTargetAndFreesItsMinimum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		target := EvictionTarget{Pending: true, FreePagesTarget: 20, Level: IncludeNewest}
		node.evictor.combineEvictionTarget(target)
		counts := node.evictFromPreloadedTarget(t)
		expect(t, "evicted for the free target", counts, EvictedPageCounts{PagerBacked: 20})
		expect(t, "free after the free target", node.freePages, uint64(20))

		// The same target again evicts nothing: it is met, and no minimum
		// was asked for.
		node.evictor.combineEvictionTarget(target)
		counts = node.evictFromPreloadedTarget(t)
		expect(t, "evicted for a met target", counts, EvictedPageCounts{})
		expect(t, "free after a met target", node.freePages, uint64(20))

		// A free target exactly met is reached.
		result, err := node.evictor.EvictFromExternalTarget(t.Context(), struct{}{}, target)
		mustNotFail(t, "evict to a met target", err)
		expect(t, "a target exactly met", result, EvictionResult{FreeTargetReached: true})
		// A free target one page higher is reached by evicting it.
		result, err = node.evictor.EvictFromExternalTarget(t.Context(), struct{}{},
			EvictionTarget{Pending: true, FreePagesTarget: 21, Level: IncludeNewest})
		mustNotFail(t, "evict to a target a page higher", err)
		expect(t, "a target a page higher", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 1}, FreeTargetReached: true})
		node.freePages--

		// A higher free target evicts the difference.
		const deltaPages = 10
		target.FreePagesTarget += deltaPages
		node.evictor.combineEvictionTarget(target)
		counts = node.evictFromPreloadedTarget(t)
		expect(t, "evicted for a higher free target", counts, EvictedPageCounts{PagerBacked: deltaPages})
		expect(t, "free after a higher free target", node.freePages, uint64(30))

		// A higher free target and a minimum: the difference is the minimum.
		target.FreePagesTarget += deltaPages
		target.MinPagesToFree = deltaPages
		node.evictor.combineEvictionTarget(target)
		counts = node.evictFromPreloadedTarget(t)
		expect(t, "evicted for a higher target and a minimum", counts, EvictedPageCounts{PagerBacked: deltaPages})
		expect(t, "free after a higher target and a minimum", node.freePages, uint64(40))

		// The same free target, with a minimum: the minimum is evicted.
		target.MinPagesToFree = 2
		node.evictor.combineEvictionTarget(target)
		counts = node.evictFromPreloadedTarget(t)
		expect(t, "evicted for a minimum alone", counts, EvictedPageCounts{PagerBacked: 2})
		expect(t, "free after a minimum alone", node.freePages, uint64(42))
	})
}

// evictor_external_target_test. Zircon draws the pending flag and the level
// at random; here every combination is tried.
func TestAnExternalTargetLeavesTheSetTargetAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, pending := range []bool{false, true} {
			for _, level := range []EvictionLevel{OnlyOldest, IncludeNewest} {
				node := newTestPmmNode()
				expected := EvictionTarget{Pending: pending, FreePagesTarget: 111, MinPagesToFree: 33, Level: level}
				node.evictor.combineEvictionTarget(expected)
				external := EvictionTarget{Pending: !pending, FreePagesTarget: 99, MinPagesToFree: 22, Level: IncludeNewest - level}
				result, err := node.evictor.EvictFromExternalTarget(t.Context(), struct{}{}, external)
				mustNotFail(t, "evict", err)
				expectTarget(t, "after an external eviction", node.evictor.debugGetEvictionTarget(), expected)
				// The external target was evicted to, where it was pending.
				want := EvictionResult{}
				if external.Pending {
					want = EvictionResult{Counts: EvictedPageCounts{PagerBacked: 99}, FreeTargetReached: true}
				}
				expect(t, "the external eviction", result, want)
			}
		}
	})
}

// evictor_min_target_carried_over_test
func TestAMinimumNotMetIsCarriedOverToTheNextEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		target := EvictionTarget{Pending: true, FreePagesTarget: 10, MinPagesToFree: 15, Level: IncludeNewest}
		// Only five pages can be evicted.
		node.maxEvictions = 5
		node.evictor.combineEvictionTarget(target)
		counts := node.evictFromPreloadedTarget(t)
		expect(t, "evicted with a cap", counts, EvictedPageCounts{PagerBacked: 5})
		expectTarget(t, "carried over", node.evictor.debugGetEvictionTarget(), EvictionTarget{MinPagesToFree: 10})

		node.maxEvictions = math.MaxUint64
		// A target with no minimum of its own.
		node.evictor.combineEvictionTarget(EvictionTarget{Pending: true, Level: IncludeNewest})
		counts = node.evictFromPreloadedTarget(t)
		expect(t, "the rest evicted", counts, EvictedPageCounts{PagerBacked: 10})
		expect(t, "free", node.freePages, uint64(15))
		expectTarget(t, "nothing left over", node.evictor.debugGetEvictionTarget(), EvictionTarget{})
	})
}

// What follows tests what Zircon's cases leave untested.

// A synchronous eviction converts its targets from bytes to pages, and does
// nothing while eviction is off.
func TestASynchronousEvictionCountsBytesInPagesAndNeedsEvictionOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		node.evictor.DisableEviction()
		result, err := node.evictor.EvictSynchronous(t.Context(), struct{}{}, 3*testEvictorPageSize, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict while off", err)
		expect(t, "evicted while off", result, EvictionResult{})
		expect(t, "enabled", node.evictor.IsEvictionEnabled(), false)

		node.evictor.EnableEviction(false)
		expect(t, "compression", node.evictor.IsCompressionEnabled(), false)
		// A part of a page counts as no page.
		result, err = node.evictor.EvictSynchronous(t.Context(), struct{}{},
			3*testEvictorPageSize+testEvictorPageSize/2, 5*testEvictorPageSize-1, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict", err)
		expect(t, "evicted for the minimum", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 4}, FreeTargetReached: true})
		expect(t, "the stats", node.evictor.Stats(), EvictorStats{PagerBackedOther: 4})

		// An eviction for an out of memory condition is counted as one, and
		// a printed one evicts the same.
		result, err = node.evictor.EvictSynchronous(t.Context(), struct{}{}, 2*testEvictorPageSize, 0, IncludeNewest, Print, OOM)
		mustNotFail(t, "evict for OOM", err)
		expect(t, "evicted for OOM", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 2}, FreeTargetReached: true})
		expect(t, "the stats after OOM", node.evictor.Stats(), EvictorStats{PagerBackedOOM: 2, PagerBackedOther: 4})
	})
}

// A round frees at most 128 pages unless it is for an out of memory
// condition, and a target larger than a round is met over several. The free
// pages are counted once before the eviction, once a round, once to find the
// targets met, and once after.
func TestAnEvictionLargerThanARoundTakesSeveral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		counted := 0
		node.evictor.freePages = func() uint64 { counted++; return node.freePages }
		result, err := node.evictor.EvictSynchronous(t.Context(), struct{}{}, 300*testEvictorPageSize, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict 300", err)
		expect(t, "evicted", result.Counts, EvictedPageCounts{PagerBacked: 300})
		expect(t, "free pages counted in three rounds", counted, 6)
		counted = 0
		result, err = node.evictor.EvictSynchronous(t.Context(), struct{}{}, 300*testEvictorPageSize, 0, IncludeNewest, NoPrint, OOM)
		mustNotFail(t, "evict 300 for OOM", err)
		expect(t, "evicted for OOM", result.Counts, EvictedPageCounts{PagerBacked: 300})
		expect(t, "free pages counted in one round", counted, 4)
	})
}

// A reclaim that fails for good ends the eviction with its error, and
// counts what was evicted before it.
func TestAReclaimThatFailsEndsTheEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		node.failAfter = 2
		result, err := node.evictor.EvictSynchronous(t.Context(), struct{}{}, 5*testEvictorPageSize, 0, IncludeNewest, NoPrint, Other)
		if !errors.Is(err, errReclaimFailed) {
			t.Fatalf("the eviction failed with %v, want %v", err, errReclaimFailed)
		}
		expect(t, "evicted before the failure", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 2}, FreeTargetReached: true})
		expect(t, "the stats", node.evictor.Stats(), EvictorStats{PagerBackedOther: 2})
	})
}

// Failed reclaims and compressions are counted apart from evictions, and a
// reclaim that fails goes on to the next.
func TestAnEvictionCountsCompressionsAndGoesPastFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := []ReclaimAttempt{
			{Failure: IncorrectPage},
			{Success: ReclaimSuccess{Type: ReclaimCompress, NumPages: 1}},
			{Failure: CompressFailedReclaim},
			{Failure: CompressAccessed},
			{Failure: EvictAccessed},
			{Failure: ReclaimOther},
			{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 3}},
		}
		var compressed []bool
		evictor := NewEvictorWith(testEvictorPageSize,
			func(_ context.Context, _ struct{}, compress bool, _ EvictionLevel) (ReclaimAttempt, bool, error) {
				compressed = append(compressed, compress)
				if len(attempts) == 0 {
					return ReclaimAttempt{}, false, nil
				}
				attempt := attempts[0]
				attempts = attempts[1:]
				return attempt, true, nil
			}, func() uint64 { return 0 })
		evictor.EnableEviction(true)
		result, err := evictor.EvictSynchronous(t.Context(), struct{}{}, 10*testEvictorPageSize, 0, OnlyOldest, NoPrint, OOM)
		mustNotFail(t, "evict", err)
		expect(t, "evicted", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 3, Compressed: 1}, FreeTargetReached: true})
		expect(t, "the stats", evictor.Stats(), EvictorStats{PagerBackedOOM: 3, CompressionOOM: 1})
		// The seven attempts and the step that found nothing, and a second
		// round for the rest of the minimum, which finds nothing at once.
		expect(t, "reclaims tried", len(compressed), 9)
		expect(t, "each told to compress", compressed[0] && compressed[8], true)
	})
}

// The failure diagnosis counts each reason, starts again at a success, and
// reports a run of failures at a thousand and then ten times as many.
func TestTheFailureDiagnosisReportsRunsOfFailures(t *testing.T) {
	ctx := t.Context()
	stats := newReclaimFailureStats()
	page := new(int)
	for range 999 {
		diagnoseReclamationFailure(ctx, ReclaimAttempt{Failure: EvictAccessed, Page: page}, &stats)
	}
	expect(t, "failures in a row", stats.consecutiveReclaimFailures, uint64(999))
	expect(t, "evict accessed", stats.evictAccessed, uint64(999))
	expect(t, "the same page", stats.prevPageEvictions, uint64(998))
	expect(t, "next report", stats.failuresToCompare, uint64(1000))
	diagnoseReclamationFailure(ctx, ReclaimAttempt{Failure: CompressAccessed, Page: page}, &stats)
	expect(t, "next report after the first", stats.failuresToCompare, uint64(10000))
	// IncorrectPage is a race over ownership, after which the same page is
	// expected again.
	diagnoseReclamationFailure(ctx, ReclaimAttempt{Failure: IncorrectPage, Page: page}, &stats)
	diagnoseReclamationFailure(ctx, ReclaimAttempt{Failure: CompressFailedReclaim, Page: page}, &stats)
	expect(t, "the same page after an incorrect page", stats.prevPageEvictions, uint64(1000))
	diagnoseReclamationFailure(ctx, ReclaimAttempt{Failure: ReclaimOther, Page: new(int)}, &stats)
	expect(t, "counts by reason", [5]uint64{stats.compressFailed, stats.compressAccessed, stats.evictAccessed, stats.incorrectPage, stats.otherFailureReasons},
		[5]uint64{1, 1, 999, 1, 1})
	diagnoseReclamationFailure(ctx, ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}, Page: page}, &stats)
	expect(t, "after a success", stats, reclaimFailureStats{
		failuresToCompare: 1000, prevPageEvictions: 1000, printedSamePageLog: true,
		prevAttempt: ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}, Page: page},
	})
	for attempt, want := range map[ReclaimAttempt]string{
		{Failure: CompressFailedReclaim}: "fail:compress", {Failure: CompressAccessed}: "fail:access_c",
		{Failure: EvictAccessed}: "fail:access_e", {Failure: IncorrectPage}: "fail:incorrect", {Failure: ReclaimOther}: "fail:other",
		{Success: ReclaimSuccess{Type: ReclaimCompress}}: "ok:compress", {}: "ok:evict",
	} {
		expect(t, "the result's string", reclaimResultString(attempt), want)
	}
}

// An asynchronous request starts the eviction goroutine, which evicts to the
// combined target and clears it. Disabling eviction ends the goroutine.
func TestAnAsynchronousRequestEvictsOnTheEvictionGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newTestPmmNode()
		node.evictor.EvictAsynchronous(t.Context(), 3*testEvictorPageSize, 0, OnlyOldest, NoPrint)
		synctest.Wait()
		expect(t, "free after the first request", node.freePages, uint64(3))
		expectTarget(t, "after the first request", node.evictor.debugGetEvictionTarget(), EvictionTarget{})
		node.evictor.EvictAsynchronous(t.Context(), 0, 7*testEvictorPageSize, IncludeNewest, Print)
		synctest.Wait()
		expect(t, "free after the second request", node.freePages, uint64(7))
		// A reclaim that fails is logged by the goroutine, which goes on.
		node.failAfter = 7
		node.evictor.EvictAsynchronous(t.Context(), testEvictorPageSize, 0, OnlyOldest, NoPrint)
		synctest.Wait()
		expect(t, "free after a failure", node.freePages, uint64(7))
		node.failAfter = 0
		node.evictor.EvictAsynchronous(t.Context(), testEvictorPageSize, 0, OnlyOldest, NoPrint)
		synctest.Wait()
		expect(t, "free after the failure", node.freePages, uint64(9))
		node.evictor.DisableEviction()
		expect(t, "enabled after disabling", node.evictor.IsEvictionEnabled(), false)
		// With eviction off, a request is dropped and starts nothing.
		node.evictor.EvictAsynchronous(t.Context(), testEvictorPageSize, 0, OnlyOldest, NoPrint)
		synctest.Wait()
		expect(t, "free while off", node.freePages, uint64(9))
		expectTarget(t, "while off", node.evictor.debugGetEvictionTarget(), EvictionTarget{})
	})
}

// A node's evictor peeks the node's isolated pages: a page a pager backs is
// evicted with the node's worth around it, and an anonymous page is
// compressed. The oldest queues alone hold no page that has just been made.
func TestANodesEvictorEvictsPagerPagesAndCompressesAnonymousOnes(t *testing.T) {
	forEachVmoPageSize(t, func(t *testing.T, env *vmoEnv) {
		ps := env.ps
		pq := env.node.PageQueues()
		pq.EnableAnonymousReclaim(false)
		evictor := NewEvictor[struct{}](env.node)
		evictor.EnableEviction(true)
		makeCommittedPagerVmo(t, env, 2, false)
		// Three generations old: inactive, and not among the oldest.
		for range 3 {
			pq.RotateReclaimQueues()
		}
		anonymous, err := CreateObjectPaged(env.node, ps)
		mustNotFail(t, "create", err)
		mustNotFail(t, "write", anonymous.Write(env.ctx, pattern(ps, 'A'), 0))
		out := env.pmm.out
		result, err := evictor.EvictSynchronous(env.ctx, struct{}{}, ps, 0, OnlyOldest, NoPrint, Other)
		mustNotFail(t, "evict the oldest", err)
		expect(t, "evicted from the oldest", result, EvictionResult{FreeTargetReached: true})
		result, err = evictor.EvictSynchronous(env.ctx, struct{}{}, ps, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict", err)
		expect(t, "evicted the pager's pages", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 2}, FreeTargetReached: true})
		// Arming the compressor took its spare page.
		expect(t, "pages out after evicting", env.pmm.out, out-2+1)
		// The anonymous page was still active.
		pq.MoveToReclaimDontNeed(anonymous.DebugGetPage(0))
		result, err = evictor.EvictSynchronous(env.ctx, struct{}{}, ps, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "compress", err)
		expect(t, "compressed the anonymous page", result, EvictionResult{Counts: EvictedPageCounts{Compressed: 1}, FreeTargetReached: true})
		expect(t, "the anonymous page", anonymous.DebugGetPage(0) == nil, true)
		got := make([]byte, ps)
		mustNotFail(t, "read back", anonymous.Read(env.ctx, got, 0))
		expect(t, "read back", string(got), string(pattern(ps, 'A')))
		// Nothing is left to reclaim.
		result, err = evictor.EvictSynchronous(env.ctx, struct{}{}, ps, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict again", err)
		expect(t, "evicted again", result, EvictionResult{FreeTargetReached: true})
		expect(t, "the stats", evictor.Stats(), EvictorStats{PagerBackedOther: 2, CompressionOther: 1})
		// Pages in the oldest queues are taken from them alone: the
		// anonymous page read back, and then a pager's page made after it.
		makeCommittedPagerVmo(t, env, 1, false)
		for range NumReclaim - 1 {
			pq.RotateReclaimQueues()
		}
		result, err = evictor.EvictSynchronous(env.ctx, struct{}{}, 2*ps, 0, OnlyOldest, NoPrint, Other)
		mustNotFail(t, "evict the oldest pages", err)
		expect(t, "evicted the oldest pages", result, EvictionResult{Counts: EvictedPageCounts{PagerBacked: 1, Compressed: 1}, FreeTargetReached: true})
	})
}

// An eviction asked to print logs what it evicted, in KiB below a MiB and in
// MiB from there, and the free memory before and after. One that evicted
// nothing logs nothing.
func TestAPrintedEvictionLogsItsCounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logged bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			},
		})))
		defer slog.SetDefault(previous)
		// Exactly a MiB of pages, which is counted in MiB.
		pages := uint64(1<<20) / testEvictorPageSize
		results := []ReclaimAttempt{{Success: ReclaimSuccess{Type: ReclaimCompress, NumPages: 3}}}
		for range pages {
			results = append(results, ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}})
		}
		// Then one eviction of each kind alone.
		results = append(results, ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}},
			ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimCompress, NumPages: 1}},
			ReclaimAttempt{Success: ReclaimSuccess{Type: ReclaimEvict, NumPages: 1}})
		free := uint64(1<<20)/testEvictorPageSize*2 + 1
		evictor := NewEvictorWith(testEvictorPageSize,
			func(context.Context, struct{}, bool, EvictionLevel) (ReclaimAttempt, bool, error) {
				if len(results) == 0 {
					return ReclaimAttempt{}, false, nil
				}
				attempt := results[0]
				results = results[1:]
				free += attempt.Success.NumPages
				return attempt, true, nil
			}, func() uint64 { return free })
		evictor.EnableEviction(true)
		_, err := evictor.EvictSynchronous(t.Context(), struct{}{}, 0, 0, IncludeNewest, Print, Other)
		mustNotFail(t, "evict nothing", err)
		_, err = evictor.EvictSynchronous(t.Context(), struct{}{}, (pages+3)*testEvictorPageSize, 0, IncludeNewest, Print, Other)
		mustNotFail(t, "evict", err)
		_, err = evictor.EvictSynchronous(t.Context(), struct{}{}, testEvictorPageSize, 0, IncludeNewest, NoPrint, Other)
		mustNotFail(t, "evict without printing", err)
		_, err = evictor.EvictSynchronous(t.Context(), struct{}{}, testEvictorPageSize, 0, IncludeNewest, Print, Other)
		mustNotFail(t, "evict a compressed page", err)
		_, err = evictor.EvictSynchronous(t.Context(), struct{}{}, testEvictorPageSize, 0, IncludeNewest, Print, Other)
		mustNotFail(t, "evict a pager's page", err)
		want := `level=INFO msg="zirconvm: evicted" pager=1M compressed=12K "free before"=2M "free after"=3M` + "\n" +
			`level=INFO msg="zirconvm: evicted" compressed=4K "free before"=3M "free after"=3M` + "\n" +
			`level=INFO msg="zirconvm: evicted" pager=4K "free before"=3M "free after"=3M` + "\n"
		if got := logged.String(); got != want {
			t.Errorf("the log is %q, want %q", got, want)
		}
		if strings.Count(logged.String(), "evicted") != 3 {
			t.Errorf("logged %d evictions, want 3", strings.Count(logged.String(), "evicted"))
		}
	})
}

// A peek of the active queues ages them as far as its lowest queue: one
// below the active ones ages them once, so the older active queue is
// isolated and the newest is not. A peek after it ages them again.
func TestAPeekBelowTheActiveQueuesAgesThemOnlyAsFarAsItAsks(t *testing.T) {
	forEachPageSize(t, func(t *testing.T, ps uint64) {
		pq := newTestQueues(ps)
		pages := makePages(2)
		pq.SetReclaim(pages[0], pagerVmo, 0)
		pq.RotateReclaimQueues()
		pq.SetReclaim(pages[1], pagerVmo, ps)
		taken, ok := pq.PeekIsolateWhere(1, func(*queuedPage) bool { return true })
		if !ok || taken.Page != pages[0] {
			t.Fatalf("the peek took %v, %t; want page 1", taken.Page, ok)
		}
		expectCounts(t, pq, Counts{Reclaim: [NumReclaim]int{0, 1}, ReclaimIsolate: 1})
	})
}
