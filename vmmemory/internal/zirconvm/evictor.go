// Copyright 2021 The Fuchsia Authors
// Ported from zircon/kernel/vm/evictor.cc and vm/include/vm/evictor.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"context"
	"fmt"
	"log/slog"
	"math/bits"
	"sync"
	"sync/atomic"
)

// The evictor frees pages under memory pressure: it evicts pages a pager
// backs, which the pager supplies again, and compresses anonymous pages. It
// takes its candidates from the page queues and reclaims each with
// ReclaimPage, until a target is met. Zircon keeps one for the PMM; here a
// Node can have one over its page queues, and the pager has one over its
// arena's resident pages, whose reclaim it supplies itself.
//
// Departures, each marked where it is made:
//
//   - A synchronous eviction names whom it evicts for: a request, which the
//     evictor hands to each reclaim. Zircon evicts for the whole system. The
//     pager keeps a fair share of its arena for each memory region, which
//     depends on which region the page is for (plan: eviction).
//   - The eviction lock guards the counts, not a reclaim. Zircon holds it
//     across each round of reclaims, so that parallel evictions do not
//     overshoot a free target. A reclaim here can wait for other processes:
//     the pager revokes a victim's mappings from every VMM that maps it. One
//     VMM that stops answering would then stop every eviction on the host.
//   - A reclaim can fail with an error, which ends the eviction and is
//     returned. The pager's reclaims write to a spill file and send commands
//     to other processes, and either can fail for good; Zircon's cannot fail
//     that way.
//   - The eviction goroutine is started by the first asynchronous request,
//     not by EnableEviction. Zircon's thread costs nothing while it waits. A
//     goroutine that waits for ever keeps a synctest bubble from ending, and
//     the pager makes no asynchronous request.
//   - The counters are the evictor's, not the kernel's: Stats replaces
//     GetGlobalStats.
//
// Not ported, and why:
//
//   - Discardable VMOs and loaned pages: neither is ported, so the counts of
//     pages discarded and of loaned pages evicted are always zero and are
//     left out, with the discardable test.
//   - The page queues' Dump after a suspected livelock, which is not ported.

// EvictionLevel is a rough control of how old a page must be to be evicted.
type EvictionLevel uint8

const (
	// OnlyOldest evicts only from the oldest queues, and follows the
	// always_need hint.
	OnlyOldest EvictionLevel = iota
	// IncludeNewest evicts from every queue that is not active, and ignores
	// the hint.
	IncludeNewest
)

// Output says whether an eviction logs what it evicted.
type Output bool

const (
	// Print logs the counts.
	Print Output = true
	// NoPrint does not.
	NoPrint Output = false
)

// TriggerReason is why an eviction was asked for.
type TriggerReason bool

const (
	// OOM is an eviction for an out of memory condition, which is not capped
	// and is counted on its own.
	OOM TriggerReason = true
	// Other is any other eviction.
	Other TriggerReason = false
)

// EvictionTarget is what an eviction is to reach. Different goroutines can
// set a target and evict, so it is kept behind a lock.
type EvictionTarget struct {
	Pending bool
	// FreePagesTarget is the free page count to reach.
	FreePagesTarget uint64
	// MinPagesToFree is how many pages to evict at least, however many are
	// free.
	MinPagesToFree uint64
	Level          EvictionLevel
	PrintCounts    bool
	OOMTrigger     bool
}

// EvictedPageCounts are the pages an eviction freed, by how.
type EvictedPageCounts struct {
	// PagerBacked were evicted from objects a pager backs.
	PagerBacked uint64
	// Compressed were evicted from anonymous objects by compression.
	Compressed uint64
}

// add is operator+=.
func (c *EvictedPageCounts) add(counts EvictedPageCounts) {
	c.PagerBacked += counts.PagerBacked
	c.Compressed += counts.Compressed
}

// sub is operator-.
func (c EvictedPageCounts) sub(counts EvictedPageCounts) EvictedPageCounts {
	return EvictedPageCounts{
		PagerBacked: c.PagerBacked - counts.PagerBacked,
		Compressed:  c.Compressed - counts.Compressed,
	}
}

// Total is non_loaned_total: with no loaned pages, every page freed.
func (c EvictedPageCounts) Total() uint64 { return c.PagerBacked + c.Compressed }

// EvictionResult is what an eviction did and why it stopped: the pages it
// evicted, which can be more or fewer than asked for, and whether the free
// target was reached.
type EvictionResult struct {
	Counts            EvictedPageCounts
	FreeTargetReached bool
}

// EvictorStats are the pages every eviction of one evictor freed, by how and
// whether it was for an out of memory condition.
type EvictorStats struct {
	PagerBackedOOM, PagerBackedOther uint64
	CompressionOOM, CompressionOther uint64
}

// ReclaimAttempt is one step of reclamation: how it went and the page it was
// attempted on. It is Zircon's pair of a VmCowReclaimResult and its page.
type ReclaimAttempt struct {
	// Success is what was reclaimed when Failure is ReclaimSucceeded.
	Success ReclaimSuccess
	Failure ReclaimFailure
	// Page is the page reclamation was attempted on, which the evictor
	// compares with the page before it to find a reclaim that does not move
	// its page. A fake leaves it nil.
	Page any
}

// ReclaimFunction is one step of reclamation for request: it reports the
// attempt, or false when there is nothing left to try. compress says whether
// the step may compress. Zircon's is a test's fake; the evictor of a Node
// reclaims from the node's page queues, and the pager supplies its own.
type ReclaimFunction[R any] func(ctx context.Context, request R, compress bool, level EvictionLevel) (ReclaimAttempt, bool, error)

// FreePagesFunction counts the free pages the targets are measured against.
type FreePagesFunction func() uint64

// Evictor frees pages to meet targets, synchronously for a caller or
// asynchronously on its own goroutine. R is what a synchronous eviction is
// for; an asynchronous one is for R's zero value.
type Evictor[R any] struct {
	// pageSize converts a target in bytes to pages: Zircon's kPageSize.
	pageSize uint64

	// lock is lock_, over the target and the switches.
	lock            sync.Mutex
	evictionTarget  EvictionTarget
	evictionEnabled bool
	useCompression  bool

	// evictionLock is eviction_lock_, over totalEvicted. Zircon holds it
	// across a round of reclaims; see the departures.
	evictionLock sync.Mutex
	// totalEvicted counts every eviction, so that parallel evictions combine
	// their progress.
	totalEvicted EvictedPageCounts

	// evictionThread is closed when the eviction goroutine has ended, and
	// nil while there is none. Guarded by lock.
	evictionThread        chan struct{}
	evictionThreadExiting atomic.Bool
	// evictionSignal is the AutounsignalEvent the goroutine waits on.
	evictionSignal chan struct{}

	reclaim   ReclaimFunction[R]
	freePages FreePagesFunction

	// The kernel counters, kept per evictor.
	pagerBackedEvicted, pagerBackedEvictedOOM atomic.Uint64
	compressionEvicted, compressionEvictedOOM atomic.Uint64
}

// NewEvictor is an evictor of node's pages: it reclaims from the node's page
// queues, with the node's compression, and counts the node's pmm's free
// pages. Eviction is off until EnableEviction.
func NewEvictor[R any](node *Node) *Evictor[R] {
	return NewEvictorWith(node.pageSize,
		func(ctx context.Context, _ R, compress bool, level EvictionLevel) (ReclaimAttempt, bool, error) {
			attempt, ok := reclaimFromPageQueues(ctx, node, compress, level)
			return attempt, ok, nil
		},
		node.pmm.CountFreePages)
}

// NewEvictorWith is an evictor of pages of pageSize bytes that reclaims with
// reclaim and counts free pages with freePages. It is Zircon's constructor
// for tests to fake the reclamation; the pager, whose pages are not a node's,
// uses it too. Eviction is off until EnableEviction.
func NewEvictorWith[R any](pageSize uint64, reclaim ReclaimFunction[R], freePages FreePagesFunction) *Evictor[R] {
	return &Evictor[R]{
		pageSize:       pageSize,
		evictionSignal: make(chan struct{}, 1),
		reclaim:        reclaim,
		freePages:      freePages,
	}
}

// reclaimFromPageQueues is ReclaimFromGlobalPageQueues: it peeks the oldest
// isolated page of node's queues and reclaims it, compressing with the node's
// compression if compress.
func reclaimFromPageQueues(ctx context.Context, node *Node, compress bool, level EvictionLevel) (ReclaimAttempt, bool) {
	// Avoid evicting from the newest queue to prevent thrashing.
	lowestEvictQueue := uint64(NumReclaim - numOldestQueues)
	// If we're going to include newest pages, ignore eviction hints as well,
	// i.e. also consider evicting pages with always_need set if we encounter
	// them in LRU order.
	hintAction := FollowHint
	if level == IncludeNewest {
		lowestEvictQueue = NumActiveQueues
		hintAction = IgnoreHint
	}
	backlink, ok := node.queues.PeekIsolate(lowestEvictQueue)
	if !ok {
		return ReclaimAttempt{}, false
	}
	var compressor *Compressor
	if compression := node.compression; compress && compression != nil {
		guard := compression.AcquireCompressor()
		defer guard.Release()
		compressor = guard.Get()
		if err := compressor.Arm(); err != nil {
			slog.WarnContext(ctx, "zirconvm: the evictor could not arm its compressor", "error", err)
			return ReclaimAttempt{}, false
		}
	}
	success, failure := backlink.Cow.ReclaimPage(ctx, backlink.Page, backlink.Offset, hintAction, compressor)
	return ReclaimAttempt{Success: success, Failure: failure, Page: backlink.Page}, true
}

// reclaimFailureComparisonBase is kComparisonBase: how many reclaims in a
// row fail before the evictor suspects a livelock.
const reclaimFailureComparisonBase = 1000

// reclaimFailureStats are ReclaimFailureStats: the failures of one
// EvictPageQueues, for diagnosis.
type reclaimFailureStats struct {
	consecutiveReclaimFailures uint64
	failuresToCompare          uint64

	prevPageEvictions uint64
	// prevAttempt is the attempt before, whose page and result the next is
	// compared with.
	prevAttempt         ReclaimAttempt
	printedSamePageLog  bool
	compressFailed      uint64
	compressAccessed    uint64
	evictAccessed       uint64
	incorrectPage       uint64
	otherFailureReasons uint64
}

func newReclaimFailureStats() reclaimFailureStats {
	return reclaimFailureStats{failuresToCompare: reclaimFailureComparisonBase}
}

// update counts a reclaim that failed, or starts the count again.
func (s *reclaimFailureStats) update(reclaimFailed bool) {
	if reclaimFailed {
		s.consecutiveReclaimFailures++
		return
	}
	s.consecutiveReclaimFailures = 0
	s.failuresToCompare = reclaimFailureComparisonBase
	s.compressFailed, s.compressAccessed, s.evictAccessed, s.incorrectPage, s.otherFailureReasons = 0, 0, 0, 0, 0
}

// shouldPrintLivelock reports whether the failures in a row just reached the
// count to report, and waits ten times longer for the next report, so that a
// real livelock does not flood the log.
func (s *reclaimFailureStats) shouldPrintLivelock() bool {
	if s.consecutiveReclaimFailures != s.failuresToCompare {
		return false
	}
	if hi, lo := bits.Mul64(s.failuresToCompare, 10); hi == 0 {
		s.failuresToCompare = lo
	}
	return true
}

// reclaimResultString is ToResultString.
func reclaimResultString(attempt ReclaimAttempt) string {
	switch attempt.Failure {
	case ReclaimSucceeded:
	case CompressFailedReclaim:
		return "fail:compress"
	case CompressAccessed:
		return "fail:access_c"
	case EvictAccessed:
		return "fail:access_e"
	case IncorrectPage:
		return "fail:incorrect"
	default:
		return "fail:other"
	}
	if attempt.Success.Type == ReclaimCompress {
		return "ok:compress"
	}
	return "ok:evict"
}

// checkForSamePage reports a reclaim of the page the last one was attempted
// on. Every failure but IncorrectPage moves its page out of the way, so the
// next peek should not see it again; IncorrectPage is a race over who owns
// the page, which its object cannot move. A page reclaimed and reused can
// rarely come back too (https://fxbug.dev/434361683).
func (s *reclaimFailureStats) checkForSamePage(ctx context.Context, attempt ReclaimAttempt) {
	prev := s.prevAttempt
	s.prevAttempt = attempt
	if attempt.Page != prev.Page || prev.Failure == IncorrectPage {
		return
	}
	s.prevPageEvictions++
	// Log only once per eviction attempt.
	if !s.printedSamePageLog {
		s.printedSamePageLog = true
		slog.WarnContext(ctx, "zirconvm: the evictor is reclaiming the same page again",
			"page", fmt.Sprintf("%p", attempt.Page), "previous", reclaimResultString(prev), "current", reclaimResultString(attempt))
	}
}

// diagnoseReclamationFailure is DiagnoseReclamationFailure: it counts a
// failed attempt by its reason, checks for the same page again, and logs a
// run of failures long enough to be a livelock.
func diagnoseReclamationFailure(ctx context.Context, attempt ReclaimAttempt, stats *reclaimFailureStats) {
	reclaimFailed := attempt.Failure != ReclaimSucceeded
	switch attempt.Failure {
	case ReclaimSucceeded:
	case CompressFailedReclaim:
		stats.compressFailed++
	case CompressAccessed:
		stats.compressAccessed++
	case EvictAccessed:
		stats.evictAccessed++
	case IncorrectPage:
		stats.incorrectPage++
	default:
		stats.otherFailureReasons++
	}
	// A fake reclaim might not even evict an actual page, so there is
	// nothing to check.
	if attempt.Page != nil {
		stats.checkForSamePage(ctx, attempt)
	}
	// After many failures in a row, make some noise.
	stats.update(reclaimFailed)
	if stats.shouldPrintLivelock() {
		slog.WarnContext(ctx, "zirconvm: the evictor failed many reclaims in a row, possibly a livelock",
			"failures", stats.consecutiveReclaimFailures, "same page", stats.prevPageEvictions,
			"compress failed", stats.compressFailed, "compress accessed", stats.compressAccessed,
			"evict accessed", stats.evictAccessed, "incorrect page", stats.incorrectPage, "other", stats.otherFailureReasons)
	}
}

// Stats are the pages this evictor's evictions freed. It is GetGlobalStats,
// for one evictor.
func (e *Evictor[R]) Stats() EvictorStats {
	var stats EvictorStats
	stats.PagerBackedOOM = e.pagerBackedEvictedOOM.Load()
	stats.PagerBackedOther = e.pagerBackedEvicted.Load() - stats.PagerBackedOOM
	stats.CompressionOOM = e.compressionEvictedOOM.Load()
	stats.CompressionOther = e.compressionEvicted.Load() - stats.CompressionOOM
	return stats
}

// IsEvictionEnabled reports whether any eviction can happen.
func (e *Evictor[R]) IsEvictionEnabled() bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.evictionEnabled
}

// IsCompressionEnabled reports whether eviction may compress.
func (e *Evictor[R]) IsCompressionEnabled() bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.useCompression
}

// EnableEviction lets evictions happen, compressing if useCompression.
// Departure: Zircon starts the eviction thread here; the goroutine is started
// by the first asynchronous request instead.
func (e *Evictor[R]) EnableEviction(useCompression bool) {
	e.lock.Lock()
	defer e.lock.Unlock()
	// It is an error to call this while the eviction goroutine is exiting.
	assert(!e.evictionThreadExiting.Load(), "the eviction goroutine is not exiting")
	e.evictionEnabled = true
	e.useCompression = useCompression
}

// DisableEviction stops every eviction, and the eviction goroutine if there
// is one, waiting for it to end. It must not run beside another
// DisableEviction or an EnableEviction.
func (e *Evictor[R]) DisableEviction() {
	e.lock.Lock()
	thread := e.evictionThread
	if thread != nil {
		// It is an error to call this in parallel with another
		// DisableEviction.
		assert(!e.evictionThreadExiting.Load(), "the eviction goroutine is not exiting")
		e.evictionThreadExiting.Store(true)
		e.signal()
	}
	e.lock.Unlock()
	// Wait for the goroutine with the lock dropped.
	if thread != nil {
		<-thread
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	e.evictionThread = nil
	e.evictionEnabled = false
	e.evictionThreadExiting.Store(false)
}

// signal sets the AutounsignalEvent the eviction goroutine waits on.
func (e *Evictor[R]) signal() {
	select {
	case e.evictionSignal <- struct{}{}:
	default:
	}
}

// debugGetEvictionTarget is DebugGetEvictionTarget, for tests.
func (e *Evictor[R]) debugGetEvictionTarget() EvictionTarget {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.evictionTarget
}

// checkedIncrement is CheckedIncrement: an addition that must not overflow.
func checkedIncrement(a *uint64, b uint64) {
	sum, carry := bits.Add64(*a, b, 0)
	assert(carry == 0, "the target does not overflow")
	*a = sum
}

// combineEvictionTarget combines target with the target already set: the
// pending flags and print flags are or'd, the levels and free targets take
// the larger, and the minimums add.
func (e *Evictor[R]) combineEvictionTarget(target EvictionTarget) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.evictionTarget.Pending = e.evictionTarget.Pending || target.Pending
	e.evictionTarget.Level = max(e.evictionTarget.Level, target.Level)
	checkedIncrement(&e.evictionTarget.MinPagesToFree, target.MinPagesToFree)
	e.evictionTarget.FreePagesTarget = max(e.evictionTarget.FreePagesTarget, target.FreePagesTarget)
	e.evictionTarget.PrintCounts = e.evictionTarget.PrintCounts || target.PrintCounts
}

// EvictFromExternalTarget evicts for request to target, which is used for
// this eviction only and leaves the evictor's own target alone.
func (e *Evictor[R]) EvictFromExternalTarget(ctx context.Context, request R, target EvictionTarget) (EvictionResult, error) {
	return e.evictFromTargetInternal(ctx, request, target)
}

// evictFromPreloadedTarget evicts to the evictor's own target, which
// combineEvictionTarget set, and clears it, keeping any minimum of pages
// still to free for the next eviction.
func (e *Evictor[R]) evictFromPreloadedTarget(ctx context.Context) (EvictedPageCounts, error) {
	// A copy of the target to work against.
	e.lock.Lock()
	target := e.evictionTarget
	e.lock.Unlock()
	var request R
	result, err := e.evictFromTargetInternal(ctx, request, target)
	e.lock.Lock()
	defer e.lock.Unlock()
	total := result.Counts.Total()
	e.evictionTarget = EvictionTarget{}
	if total < target.MinPagesToFree {
		e.evictionTarget.MinPagesToFree = target.MinPagesToFree - total
	}
	return result.Counts, err
}

// evictFromTargetInternal is the eviction both kinds of target share: it
// evicts until the target is met, logs the counts if asked, and counts an
// eviction for an out of memory condition.
func (e *Evictor[R]) evictFromTargetInternal(ctx context.Context, request R, target EvictionTarget) (EvictionResult, error) {
	if !target.Pending {
		return EvictionResult{}, nil
	}
	freePagesBefore := e.countFreePages()
	result, err := e.evictUntilTargetsMet(ctx, request, target.MinPagesToFree, target.FreePagesTarget, target.Level, target.OOMTrigger)
	freePagesAfter := e.countFreePages()
	if target.PrintCounts && result.Counts.Total() > 0 {
		// The counts in KiB below a MiB, and in MiB from there.
		format := func(count uint64) string {
			if count < (1<<20)/e.pageSize {
				return fmt.Sprintf("%dK", count*e.pageSize>>10)
			}
			return fmt.Sprintf("%dM", count*e.pageSize>>20)
		}
		var attrs []any
		if result.Counts.PagerBacked > 0 {
			attrs = append(attrs, "pager", format(result.Counts.PagerBacked))
		}
		if result.Counts.Compressed > 0 {
			attrs = append(attrs, "compressed", format(result.Counts.Compressed))
		}
		attrs = append(attrs, "free before", fmt.Sprintf("%dM", freePagesBefore*e.pageSize>>20),
			"free after", fmt.Sprintf("%dM", freePagesAfter*e.pageSize>>20))
		slog.InfoContext(ctx, "zirconvm: evicted", attrs...)
	}
	if target.OOMTrigger {
		e.pagerBackedEvictedOOM.Add(result.Counts.PagerBacked)
		e.compressionEvictedOOM.Add(result.Counts.Compressed)
	}
	return result, err
}

// EvictSynchronous evicts for request until free memory is freeMemTarget
// bytes and at least minMemToFree bytes have been freed. level is a rough
// control of how old a page must be to be evicted. It reports what it
// evicted and whether the free target was reached.
func (e *Evictor[R]) EvictSynchronous(ctx context.Context, request R, minMemToFree, freeMemTarget uint64,
	level EvictionLevel, output Output, reason TriggerReason) (EvictionResult, error) {
	if !e.IsEvictionEnabled() {
		return EvictionResult{}, nil
	}
	target := EvictionTarget{
		Pending:         true,
		FreePagesTarget: freeMemTarget / e.pageSize,
		MinPagesToFree:  minMemToFree / e.pageSize,
		Level:           level,
		PrintCounts:     output == Print,
		OOMTrigger:      reason == OOM,
	}
	return e.EvictFromExternalTarget(ctx, request, target)
}

// EvictAsynchronous asks the eviction goroutine to evict until free memory
// is freeMemTarget bytes and at least minMemToFree bytes have been freed, and
// returns at once. Requests made before the goroutine gets to them combine:
// the minimums add, and the free targets and levels take the larger. The
// goroutine stops when the target is met or nothing more can be reclaimed,
// and clears the target.
//
// Departure: the first request starts the goroutine, with ctx's values and
// without its cancellation. Zircon starts its thread in EnableEviction.
func (e *Evictor[R]) EvictAsynchronous(ctx context.Context, minMemToFree, freeMemTarget uint64, level EvictionLevel, output Output) {
	if !e.IsEvictionEnabled() {
		return
	}
	e.combineEvictionTarget(EvictionTarget{
		Pending:         true,
		FreePagesTarget: freeMemTarget / e.pageSize,
		MinPagesToFree:  minMemToFree / e.pageSize,
		Level:           level,
		PrintCounts:     output == Print,
	})
	e.lock.Lock()
	if e.evictionThread == nil && !e.evictionThreadExiting.Load() {
		thread := make(chan struct{})
		e.evictionThread = thread
		go func() {
			defer close(thread)
			e.evictionThreadLoop(context.WithoutCancel(ctx))
		}()
	}
	e.lock.Unlock()
	// Unblock the eviction goroutine.
	e.signal()
}

// evictUntilTargetsMet evicts for request until minPagesToEvict pages have
// been evicted and at least freePagesTarget pages are free. It stops as soon
// as both are met. The counts include every eviction that ran beside it, so
// they can be higher than asked for.
func (e *Evictor[R]) evictUntilTargetsMet(ctx context.Context, request R, minPagesToEvict, freePagesTarget uint64,
	level EvictionLevel, oomTrigger bool) (EvictionResult, error) {
	if !e.IsEvictionEnabled() {
		return EvictionResult{}, nil
	}
	readCounts := func() EvictedPageCounts {
		e.evictionLock.Lock()
		defer e.evictionLock.Unlock()
		return e.totalEvicted
	}
	// The counts at the beginning of these attempts.
	startingCounts := readCounts()
	freeTargetReached := false
	for {
		// Departure: Zircon holds the eviction lock from here to the end of
		// the round. It is taken to read the counts and to add the round's,
		// and the free pages are counted outside it: the pager counts them
		// under its own lock, which a reclaim takes too.
		freePages := e.countFreePages()
		pagesFreedSoFar := readCounts().Total() - startingCounts.Total()
		var pagesToFree uint64
		// Evict at least minPagesToEvict, and then up to the free target.
		if pagesFreedSoFar < minPagesToEvict {
			pagesToFree = minPagesToEvict - pagesFreedSoFar
		}
		if freePages < freePagesTarget {
			pagesToFree = max(freePagesTarget-freePages, pagesToFree)
		} else {
			freeTargetReached = true
		}
		if pagesToFree == 0 {
			// The targets have been met.
			break
		}
		// Cap the pages to free so that one eviction does not keep the
		// others waiting. An eviction for an out of memory condition is not
		// capped, so it ends the condition as fast as it can.
		if !oomTrigger {
			pagesToFree = min(evictionRoundPages, pagesToFree)
		}
		pagesFreed, err := e.evictPageQueues(ctx, request, pagesToFree, level)
		e.evictionLock.Lock()
		e.totalEvicted.add(pagesFreed)
		e.evictionLock.Unlock()
		if err != nil {
			return EvictionResult{Counts: readCounts().sub(startingCounts), FreeTargetReached: freeTargetReached}, err
		}
		// Having freed nothing, give up and consider the eviction complete.
		if pagesFreed.Total() == 0 {
			break
		}
	}
	return EvictionResult{Counts: readCounts().sub(startingCounts), FreeTargetReached: freeTargetReached}, nil
}

// evictionRoundPages is the cap on the pages one round of an eviction frees.
const evictionRoundPages = 128

// evictPageQueues evicts for request until it has counted targetPages pages
// freed, or there is nothing more to try.
func (e *Evictor[R]) evictPageQueues(ctx context.Context, request R, targetPages uint64, level EvictionLevel) (EvictedPageCounts, error) {
	var counts EvictedPageCounts
	if !e.IsEvictionEnabled() {
		return counts, nil
	}
	compress := e.IsCompressionEnabled()
	failureStats := newReclaimFailureStats()
	var err error
	for counts.Total() < targetPages {
		// One step of eviction: EvictPageQueuesHelper, which calls the
		// reclaim the evictor was made with.
		attempt, ok, reclaimErr := e.reclaim(ctx, request, compress, level)
		if reclaimErr != nil {
			err = reclaimErr
			break
		}
		// Nothing left to try, whatever the target.
		if !ok {
			break
		}
		diagnoseReclamationFailure(ctx, attempt, &failureStats)
		if attempt.Failure != ReclaimSucceeded {
			continue
		}
		switch attempt.Success.Type {
		case ReclaimEvict:
			counts.PagerBacked += attempt.Success.NumPages
		case ReclaimCompress:
			counts.Compressed += attempt.Success.NumPages
		}
	}
	e.pagerBackedEvicted.Add(counts.PagerBacked)
	e.compressionEvicted.Add(counts.Compressed)
	return counts, err
}

// evictionThreadLoop is the eviction goroutine: it waits for a request and
// evicts to the target, until it is told to exit.
func (e *Evictor[R]) evictionThreadLoop(ctx context.Context) {
	for !e.evictionThreadExiting.Load() {
		<-e.evictionSignal
		if e.evictionThreadExiting.Load() {
			break
		}
		// With no target pending this evicts nothing.
		if _, err := e.evictFromPreloadedTarget(ctx); err != nil {
			slog.ErrorContext(ctx, "zirconvm: an asynchronous eviction failed", "error", err)
		}
	}
}

// countFreePages counts the free pages the targets are measured against.
func (e *Evictor[R]) countFreePages() uint64 { return e.freePages() }
