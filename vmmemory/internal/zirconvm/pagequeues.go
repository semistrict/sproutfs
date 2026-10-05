// Copyright 2020 The Fuchsia Authors
// Copyright 2016 The Fuchsia Authors
// Copyright (c) 2014 Travis Geiselbrecht
// Ported from zircon/kernel/vm/page_queues.cc, vm/include/vm/page_queues.h and
// vm/include/vm/page.h at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import (
	"iter"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
)

// The page queues sort the pages of every object by how they can be
// reclaimed. Zircon keeps one PageQueues for the PMM; here a Node keeps one
// for its objects' VmPages, and the pager keeps one for its arena's resident
// pages, so the queues are generic over the page and the object a page's
// backlink names.
//
// Not ported, and why:
//
//   - The MRU and LRU threads (MruThread, LruThread, StartThreads, StopThreads
//     and the events that wake them), and aging on a timeout. The owner decided
//     that aging is driven by faults only (decision 4 of
//     plans/zircon-pager-port-2026-10-05.md): a userfaultfd pager cannot read
//     the VMM's accessed bits, so a timer would only rotate the queues and add
//     nothing. So there is no timeout, no last age time and no minimum rotate
//     time, and where Zircon signals a thread to age or to process the LRU
//     queue, the caller does the work: AgeOnAccess and RotateReclaimQueues make
//     room for themselves, and PeekIsolate processes what it needs.
//   - The scanner's accessed-bit harvest that TryAgingLocked waits for, for the
//     same reason.
//   - Loaned pages and the sweep that replaces pages with them
//     (GetCowForLoanedPage, the LruIsolate list's loan replacements): the arena
//     is a set of memfds, and no page is lent.
//   - The LRU action (SetLruAction and the LruIsolate list's reclaims), which
//     reclaims pages as the LRU queue is processed. Reclaim is the evictor's,
//     step 7 of the plan.
//   - The debug compressor, the kernel counters, Dump, GetLruPagesCompressed and
//     SetAgingEvent, which nothing here calls.
//   - The destructor's check that the queues are empty: a Go value is not
//     destroyed.

// The page queue numbers kept in a page, Zircon's PageQueue.
const (
	pageQueueNone uint8 = iota
	pageQueueAnonymous
	pageQueueWired
	pageQueueHighPriority
	pageQueueAnonymousZeroFork
	pageQueuePagerBackedDirty
	pageQueueFailedReclaim
	pageQueueReclaimIsolate
	pageQueueReclaimBase
	pageQueueReclaimLast = pageQueueReclaimBase + NumReclaim - 1
	pageQueueNumQueues   = pageQueueReclaimLast + 1
)

const (
	// NumReclaim is kNumReclaim, the number of reclaim queues.
	NumReclaim = 8
	// NumActiveQueues is kNumActiveQueues: the newest reclaim queues, whose
	// pages are active and never peeked for reclaim.
	NumActiveQueues = 2
	// numOldestQueues is kNumOldestQueues, for the counts Zircon reports.
	numOldestQueues = 2
	// The isolate queues: don't-need pages first, then standard aged pages.
	isolateQueueDontNeed = 0
	isolateQueueStandard = 1
	numIsolateQueues     = 2
	// opBatchSize is kOpBatchSize.
	opBatchSize = 64
	// noIsolateLimit is ProcessLruQueue's limit when Zircon passes
	// ktl::nullopt: value_or(SIZE_MAX).
	noIsolateLimit = math.MaxInt
	// peekIsolateBatch is how many pages a peek isolates at a time.
	peekIsolateBatch = 16
)

// Zircon's static_asserts on the queue numbering. The queue numbers are
// uint8, so a line whose condition fails overflows and does not compile;
// that a queue number fits a byte follows from the type.
const (
	_ = uint(NumReclaim - NumActiveQueues - 1)                   // at least one queue is not active
	_ = uint(NumReclaim - numOldestQueues - NumActiveQueues)     // the oldest are not active
	_ = uint(pageQueueReclaimBase - pageQueueReclaimIsolate - 1) // isolate is just before the LRU queues...
	_ = uint(pageQueueReclaimIsolate + 1 - pageQueueReclaimBase) // ...exactly
	_ = uint(pageQueueReclaimIsolate - pageQueuePagerBackedDirty - 1)
	_ = uint(isolateQueueStandard - isolateQueueDontNeed - 1) // don't-need comes first
	_ = uint(numIsolateQueues - isolateQueueStandard - 1)
)

// QueueObject is what a queued page's backlink names, a VmCowPages in Zircon,
// where it is a void*.
type QueueObject interface {
	comparable
	// CanEvict is VmCowPages::can_evict: whether the object's pages are a
	// pager's, which are evicted rather than compressed.
	CanEvict() bool
}

// CanEvict is can_evict, for the page queues.
func (c *CowPages) CanEvict() bool { return c.canEvict() }

// PageQueueNode is the part of vm_page_t the page queues own while a page is
// in one: its queue_node, and the backlink and queue number of its object
// state. A page type holds one and names it with QueueNode.
type PageQueueNode[P any, O QueueObject] struct {
	// queue is the page queue list the node is in, and prev and next its
	// neighbours there. Together they are Zircon's queue_node.
	queue      *pageQueueList[P, O]
	prev, next *PageQueueNode[P, O]
	// page is the page that holds this node.
	page P
	// object and pageOffset are the backlink: the object holding the page and
	// the page's offset in it, while the page is in a page queue.
	object     O
	pageOffset uint64
	// pageQueue is the queue the page is in, Zircon's page_queue_priv. Zircon
	// reads and changes it with atomics so that MarkAccessed takes no lock;
	// here it is guarded by the queues' list lock.
	pageQueue uint8
}

// InContainer is queue_node.InContainer: whether the page is in a queue.
func (n *PageQueueNode[P, O]) InContainer() bool { return n.queue != nil }

// QueuedPage is a page the queues can hold.
type QueuedPage[P any, O QueueObject] interface {
	comparable
	QueueNode() *PageQueueNode[P, O]
}

// pageQueueList is Zircon's VmPageDoublyLinkedList for one queue: nodes linked
// through their prev and next, with a sentinel.
type pageQueueList[P any, O QueueObject] struct {
	head PageQueueNode[P, O]
	len  int
}

func (l *pageQueueList[P, O]) init() {
	l.head.next = &l.head
	l.head.prev = &l.head
}

func (l *pageQueueList[P, O]) isEmpty() bool { return l.head.next == &l.head }

// pushFront puts n at the head, where newer pages go.
func (l *pageQueueList[P, O]) pushFront(n *PageQueueNode[P, O]) { l.insertAfter(n, &l.head) }

// pushBack puts n at the tail, where older pages are.
func (l *pageQueueList[P, O]) pushBack(n *PageQueueNode[P, O]) { l.insertAfter(n, l.head.prev) }

func (l *pageQueueList[P, O]) insertAfter(n, at *PageQueueNode[P, O]) {
	assert(n.queue == nil, "the page is in no queue list")
	n.prev = at
	n.next = at.next
	at.next.prev = n
	at.next = n
	n.queue = l
	l.len++
}

// popBack takes the page at the tail.
func (l *pageQueueList[P, O]) popBack() *PageQueueNode[P, O] {
	assert(!l.isEmpty(), "the queue list is not empty")
	n := l.head.prev
	removeFromQueueList(n)
	return n
}

// front is the page at the head.
func (l *pageQueueList[P, O]) front() *PageQueueNode[P, O] {
	assert(!l.isEmpty(), "the queue list is not empty")
	return l.head.next
}

// back is the page at the tail.
func (l *pageQueueList[P, O]) back() *PageQueueNode[P, O] {
	assert(!l.isEmpty(), "the queue list is not empty")
	return l.head.prev
}

// removeFromQueueList is RemoveFromContainer on a page's queue_node.
func removeFromQueueList[P any, O QueueObject](n *PageQueueNode[P, O]) {
	l := n.queue
	assert(l != nil, "the page is in a queue list")
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev, n.next, n.queue = nil, nil, nil
	l.len--
}

// AgeReason is PageQueues::AgeReason: why the queues aged.
type AgeReason int

const (
	// AgeReasonActiveRatio is ActiveRatio: the allowed ratio of active to
	// inactive pages was exceeded.
	AgeReasonActiveRatio AgeReason = iota
	// AgeReasonManual is Manual: an explicit RotateReclaimQueues.
	AgeReasonManual
	// AgeReasonAccess is not Zircon's; it takes the place of Timeout. A fault
	// made or marked a page accessed, and AgeOnAccess aged the queues for it.
	AgeReasonAccess
)

// String is string_from_age_reason.
func (r AgeReason) String() string {
	switch r {
	case AgeReasonActiveRatio:
		return "Active ratio"
	case AgeReasonManual:
		return "Manual"
	case AgeReasonAccess:
		return "Access"
	}
	panic("zirconvm: unreachable age reason")
}

// PageQueues sort the pages of every object by how they can be reclaimed,
// Zircon's PageQueues.
type PageQueues[P QueuedPage[P, O], O QueueObject] struct {
	// activeInactiveErrorMargin is kActiveInactiveErrorMargin: how many pages
	// may change queue before the active ratio is checked again. It is 2 MiB
	// of pages, a constant of kPageSize in Zircon.
	activeInactiveErrorMargin int64

	// listLock is Zircon's list_lock_, over the lists, the backlinks and the
	// pages' queue numbers, and lock its general lock_, over everything else.
	// Where both are taken, lock is taken first.
	listLock sync.Mutex
	lock     sync.Mutex

	// agingDisabled is aging_disabled_. Guarded by lock.
	agingDisabled bool
	// lastAgeReason is last_age_reason_. Guarded by lock.
	lastAgeReason AgeReason
	// activeRatioTriggered is active_ratio_triggered_: sticky until the next
	// aging. Guarded by lock.
	activeRatioTriggered bool

	// pageQueues is page_queues_. A page's queue number is always current,
	// and the counts with it, but a reclaim page marked accessed stays in the
	// list of its old generation until the LRU processing moves it. The lists
	// for pageQueueNone and pageQueueReclaimIsolate stay empty.
	pageQueues [pageQueueNumQueues]pageQueueList[P, O]
	// isolateQueues is isolate_queues_: every page whose queue is
	// pageQueueReclaimIsolate is in exactly one of them.
	isolateQueues [numIsolateQueues]pageQueueList[P, O]

	// lruGen and mruGen are the generations of the oldest and newest reclaim
	// queues; the queues are a ring the generations map onto. They are
	// atomic so they can be read without lock, but are changed with it held,
	// and lruGen with listLock too.
	lruGen, mruGen atomic.Uint64

	// pageQueueCounts is the number of pages in each queue. Guarded by
	// listLock.
	pageQueueCounts [pageQueueNumQueues]int

	// lazyActiveRatioAgingSkips is lazy_active_ratio_aging_skips_: the pages
	// that may have changed queue since the active ratio was last checked.
	lazyActiveRatioAgingSkips atomic.Int64

	// zeroForkIsReclaimable and anonymousIsReclaimable are Zircon's switches
	// for whether anonymous pages age in the reclaim queues. Guarded by
	// listLock.
	zeroForkIsReclaimable, anonymousIsReclaimable bool

	// activeRatioMultiplier is active_ratio_multiplier_. Guarded by lock.
	activeRatioMultiplier int
}

// NewPageQueues is a set of empty page queues for pages of pageSize bytes, a
// power of two of at most 2 MiB.
func NewPageQueues[P QueuedPage[P, O], O QueueObject](pageSize uint64) *PageQueues[P, O] {
	assert(pageSize != 0 && pageSize&(pageSize-1) == 0, "the page size is a power of two")
	assert(pageSize <= 2<<20, "the active ratio's margin is at least a page")
	pq := &PageQueues[P, O]{
		activeInactiveErrorMargin: int64((2 << 20) / pageSize),
		// kDefaultActiveRatioMultiplier.
		activeRatioMultiplier: 0,
	}
	for i := range pq.pageQueues {
		pq.pageQueues[i].init()
	}
	for i := range pq.isolateQueues {
		pq.isolateQueues[i].init()
	}
	pq.mruGen.Store(NumReclaim - 1)
	return pq
}

// genToQueue converts a free running generation to its reclaim queue.
func genToQueue(gen uint64) uint8 { return uint8(gen%NumReclaim) + pageQueueReclaimBase }

// queueIsValid checks whether a reclaim queue is between lru and mru.
func queueIsValid(pageQueue, lru, mru uint8) bool {
	assert(pageQueue >= pageQueueReclaimBase, "the queue is a reclaim queue")
	if lru <= mru {
		return pageQueue >= lru && pageQueue <= mru
	}
	return pageQueue <= mru || pageQueue >= lru
}

// queueIsReclaim reports whether a queue is reclaimable, and so can be active
// or inactive. The isolate queue is, so that an access moves a page out of it.
func queueIsReclaim(pageQueue uint8) bool { return pageQueue >= pageQueueReclaimIsolate }

// queueAge is a reclaim queue's age against mru, 0 meaning it is mru.
func queueAge(pageQueue, mru uint8) uint {
	assert(pageQueue >= pageQueueReclaimBase, "the queue is a reclaim queue")
	if pageQueue <= mru {
		return uint(mru - pageQueue)
	}
	return uint(NumReclaim) - uint(pageQueue) + uint(mru)
}

func queueIsActive(pageQueue, mru uint8) bool {
	if pageQueue < pageQueueReclaimBase {
		return false
	}
	return queueAge(pageQueue, mru) < NumActiveQueues
}

func queueIsInactive(pageQueue, mru uint8) bool {
	// The isolate queue has no age, but is inactive.
	if pageQueue == pageQueueReclaimIsolate {
		return true
	}
	if pageQueue < pageQueueReclaimBase {
		return false
	}
	return queueAge(pageQueue, mru) >= NumActiveQueues
}

// mruGenToQueue is the MRU queue. It requires lock or listLock.
func (pq *PageQueues[P, O]) mruGenToQueue() uint8 { return genToQueue(pq.mruGen.Load()) }

// canIncrementMruGenLocked reports whether the MRU generation may advance, or
// the LRU queue must be processed first. It requires lock.
func (pq *PageQueues[P, O]) canIncrementMruGenLocked() bool {
	return pq.mruGen.Load()-pq.lruGen.Load() < NumReclaim-1
}

// canIncrementLruGenLocked is CanIncrementLruGenLocked. It requires lock.
func (pq *PageQueues[P, O]) canIncrementLruGenLocked() bool {
	return pq.mruGen.Load()-pq.lruGen.Load() > NumActiveQueues
}

// SetActiveRatioMultiplier sets the active ratio multiplier.
func (pq *PageQueues[P, O]) SetActiveRatioMultiplier(multiplier uint32) {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	pq.activeRatioMultiplier = int(multiplier)
	// The change in multiplier might mean the queues need to age.
	pq.checkActiveRatioAgingLocked()
}

// checkActiveRatioAgingLocked records whether the active ratio calls for
// aging. Zircon also wakes its MRU thread; here the next aging or peek
// consumes the trigger. It requires lock.
func (pq *PageQueues[P, O]) checkActiveRatioAgingLocked() {
	if pq.activeRatioTriggered {
		return
	}
	if pq.isActiveRatioTriggeringAging() {
		pq.activeRatioTriggered = true
	}
}

// isActiveRatioTriggeringAging reports whether the active pages, multiplied,
// outnumber the inactive.
func (pq *PageQueues[P, O]) isActiveRatioTriggeringAging() bool {
	counts := pq.getActiveInactiveCountsLocked()
	return counts.Active*pq.activeRatioMultiplier > counts.Inactive
}

// synchronizeWithAging performs any aging outstanding, so that a reclaim does
// not fail to find what that aging would have made reclaimable.
func (pq *PageQueues[P, O]) synchronizeWithAging() {
	for iterations := 1; ; iterations++ {
		// This should end by its second round, unless racing other reclaims
		// that empty both the LRU queues and the isolate list, which would be
		// a bug.
		if iterations%20 == 0 {
			slog.Warn("zirconvm: synchronizing with the page queues' aging is taking many rounds",
				"rounds", iterations)
		}
		pq.lock.Lock()
		// If the LRU queue can be processed, the aging already happened.
		if pq.canIncrementLruGenLocked() {
			pq.lock.Unlock()
			return
		}
		// LRU processing may already have filled the isolate list.
		pq.listLock.Lock()
		isolated := pq.pageQueueCounts[pageQueueReclaimIsolate]
		pq.listLock.Unlock()
		if isolated > 0 {
			pq.lock.Unlock()
			return
		}
		reason, due := pq.getAgeReasonLocked()
		if !due {
			pq.lock.Unlock()
			return
		}
		pq.tryAgingLocked(reason)
	}
}

// getAgeReasonLocked is the reason aging is due, if it is. With no timeout,
// only a tripped active ratio is one, and with no minimum rotate time it is
// due at once. It requires lock.
func (pq *PageQueues[P, O]) getAgeReasonLocked() (AgeReason, bool) {
	if pq.activeRatioTriggered {
		return AgeReasonActiveRatio, true
	}
	return 0, false
}

// tryAgingLocked ages, and releases lock, which the caller holds. Zircon first
// waits for the scanner to harvest accessed bits, which are not ported.
func (pq *PageQueues[P, O]) tryAgingLocked(reason AgeReason) {
	defer pq.lock.Unlock()
	pq.incrementMruGenLocked(reason)
}

// DisableAging stops aging on access until EnableAging. A manual rotation
// still ages. A second DisableAging without an EnableAging between panics.
func (pq *PageQueues[P, O]) DisableAging() {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	if pq.agingDisabled {
		panic("zirconvm: mismatched disable/enable pair")
	}
	pq.agingDisabled = true
}

// EnableAging lets aging on access go on.
func (pq *PageQueues[P, O]) EnableAging() {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	if !pq.agingDisabled {
		panic("zirconvm: mismatched disable/enable pair")
	}
	pq.agingDisabled = false
}

// incrementMruGenLocked ages the queues by one generation. The caller holds
// lock and has made sure the MRU generation can advance.
func (pq *PageQueues[P, O]) incrementMruGenLocked(reason AgeReason) {
	assert(pq.canIncrementMruGenLocked(), "the MRU generation can advance")
	pq.mruGen.Add(1)
	pq.lastAgeReason = reason
	// Aging consumes any active ratio trigger, which is calculated again.
	pq.activeRatioTriggered = false
	pq.checkActiveRatioAgingLocked()
	// Zircon signals its aging event and its LRU thread here. Neither exists:
	// whoever ages next makes room for itself.
}

// RotateReclaimQueues ages every reclaimable page by one queue, processing the
// LRU queue first if the MRU generation cannot advance. Zircon calls it for
// tests and debugging. It ignores DisableAging.
func (pq *PageQueues[P, O]) RotateReclaimQueues() { pq.rotate(AgeReasonManual) }

// AgeOnAccess is not Zircon's. It is the aging Zircon's MRU thread does, done
// by the pager at each fault instead of by a thread on a timer (decision 4 of
// the plan). The pager ages the queues by one generation after each page a
// fault makes or marks accessed, so its pages are ordered by when a fault last
// touched them. Like the MRU thread it respects DisableAging, and like
// RotateReclaimQueues it makes room for itself, since there is no LRU thread.
func (pq *PageQueues[P, O]) AgeOnAccess() {
	pq.lock.Lock()
	disabled := pq.agingDisabled
	pq.lock.Unlock()
	if disabled {
		return
	}
	pq.rotate(AgeReasonAccess)
}

// rotate is RotateReclaimQueues with its reason.
func (pq *PageQueues[P, O]) rotate(reason AgeReason) {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	for !pq.canIncrementMruGenLocked() {
		target := pq.lruGen.Load() + 1
		pq.lock.Unlock()
		pq.processLruQueue(target, noIsolateLimit)
		pq.lock.Lock()
	}
	pq.incrementMruGenLocked(reason)
}

// LastAgeReason is why the queues last aged, last_age_reason_, which Zircon
// reports in Dump.
func (pq *PageQueues[P, O]) LastAgeReason() AgeReason {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	return pq.lastAgeReason
}

// processLruQueue moves pages out of the LRU queue until lruGen reaches
// target, or until it has isolated isolate pages: a page whose queue was
// updated by an access goes to its queue, and the rest go to the standard
// isolate queue. It is ProcessLruQueue with no LRU action.
func (pq *PageQueues[P, O]) processLruQueue(target uint64, isolate int) {
	pq.lock.Lock()
	// To evict queue X the target is X+1, so the target may reach the first
	// active queue but not pass it. mru_gen_ only grows, so this stays true.
	assert(target <= pq.mruGen.Load()-(NumActiveQueues-1), "the target leaves the active queues")
	// A worst case loop count, for diagnosis: every page in the LRU queue and
	// every queue to step through.
	counts := pq.getActiveInactiveCountsLocked()
	pq.lock.Unlock()
	maxLruIterations := counts.Active + counts.Inactive + NumReclaim
	loopIterations := 0
	for isolate > 0 {
		if loopIterations == maxLruIterations {
			slog.Error("zirconvm: processing the LRU queue exceeded its expected iterations",
				"iterations", maxLruIterations)
		}
		loopIterations++
		if pq.processLruQueueOnce(target, &isolate) {
			return
		}
		// Zircon wakes its MRU thread here when the LRU generation moved.
	}
}

// processLruQueueOnce is one round of ProcessLruQueue's loop, with both locks
// held so that lruGen does not change under it. It reports whether lruGen has
// reached target.
func (pq *PageQueues[P, O]) processLruQueueOnce(target uint64, isolate *int) bool {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	lru := pq.lruGen.Load()
	if lru >= target {
		return true
	}
	mruQueue := pq.mruGenToQueue()
	lruQueue := genToQueue(lru)
	list := &pq.pageQueues[lruQueue]
	for iterations := 0; !list.isEmpty() && *isolate > 0; {
		// Newer pages are at the head and older at the tail, so the tail goes
		// first.
		node := list.popBack()
		pageQueue := node.pageQueue
		assert(pageQueue >= pageQueueReclaimBase, "the page is in a reclaim queue")
		// A page whose queue does not match was accessed since and goes to its
		// queue, unless MarkAccessed raced and its queue is invalid, in which
		// case it is very old and is isolated.
		if pageQueue != lruQueue && queueIsValid(pageQueue, lruQueue, mruQueue) {
			// The head, because it is newer.
			pq.pageQueues[pageQueue].pushFront(node)
		} else {
			// Aged pages go to the standard isolate queue, at its tail, so
			// older pages stay at its head.
			oldQueue := node.pageQueue
			node.pageQueue = pageQueueReclaimIsolate
			assert(oldQueue >= pageQueueReclaimBase, "the aged page was in a reclaim queue")
			pq.pageQueueCounts[oldQueue]--
			pq.pageQueueCounts[pageQueueReclaimIsolate]++
			pq.isolateQueues[isolateQueueStandard].pushBack(node)
			*isolate--
		}
		iterations++
		if pq.batchOpShouldDropLock(iterations) {
			break
		}
	}
	if list.isEmpty() {
		// Both locks were held throughout, so this is exactly lru + 1.
		pq.lruGen.Add(1)
	}
	return false
}

// batchOpShouldDropLock is BatchOpShouldDropLock: every opBatchSize pages, a
// batch drops the list lock if it is contested. A Go mutex cannot say whether
// it is, so it is never dropped early: a batch ends as an uncontested one
// does, with the lock held longer.
func (pq *PageQueues[P, O]) batchOpShouldDropLock(int) bool { return false }

// markAccessedMaybeIsolate is MarkAccessed for a page that may be in the
// isolate queue, which must be moved between the lists.
func (pq *PageQueues[P, O]) markAccessedMaybeIsolate(node *PageQueueNode[P, O]) {
	pq.listLock.Lock()
	// A page can only leave the reclaim queues under the list lock.
	if !queueIsReclaim(node.pageQueue) {
		pq.listLock.Unlock()
		return
	}
	pq.moveToQueueLockedList(node, pq.mruGenToQueue())
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// MarkAccessed tells the queues the page was accessed, so it moves to the
// newest reclaim queue. A page in no reclaim queue does not move.
func (pq *PageQueues[P, O]) MarkAccessed(page P) {
	node := page.QueueNode()
	pq.listLock.Lock()
	target := pq.mruGenToQueue()
	oldGen := node.pageQueue
	if oldGen == target {
		pq.listLock.Unlock()
		return
	}
	// Not in the reclaim queues, so there is nothing to mark.
	if !queueIsReclaim(oldGen) {
		pq.listLock.Unlock()
		return
	}
	// The isolate queue is a reclaim queue, but the page must move lists.
	if oldGen == pageQueueReclaimIsolate {
		pq.listLock.Unlock()
		pq.markAccessedMaybeIsolate(node)
		return
	}
	// Zircon changes only the page's queue number, with a compare and swap
	// and no lock, and leaves the page in its old list for the LRU processing
	// to move. Here the number is guarded by the list lock.
	node.pageQueue = target
	pq.pageQueueCounts[oldGen]--
	pq.pageQueueCounts[target]++
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// maybeCheckActiveRatioAging checks the active ratio once the pages that may
// have changed queue since it was last checked reach the margin.
func (pq *PageQueues[P, O]) maybeCheckActiveRatioAging(pages int) {
	if !pq.recordActiveRatioSkips(pages) {
		pq.lock.Lock()
		pq.checkActiveRatioAgingLocked()
		pq.lock.Unlock()
	}
}

// recordActiveRatioSkips adds pages that may have changed queue and reports
// whether the active ratio check can be skipped. Only the addition that
// crosses the margin checks, so threads do not all check at once.
func (pq *PageQueues[P, O]) recordActiveRatioSkips(pages int) bool {
	oldCount := pq.lazyActiveRatioAgingSkips.Add(int64(pages)) - int64(pages)
	if oldCount < pq.activeInactiveErrorMargin && oldCount+int64(pages) >= pq.activeInactiveErrorMargin {
		// This may lose counts, which is fine: the ratio is checked now.
		pq.lazyActiveRatioAgingSkips.Store(0)
		return false
	}
	return true
}

// setQueueBacklinkLockedList puts a page in no queue into queue, with its
// backlink. It requires listLock.
func (pq *PageQueues[P, O]) setQueueBacklinkLockedList(page P, object O, pageOffset uint64, queue uint8) {
	var none O
	node := page.QueueNode()
	assert(queue != pageQueueReclaimIsolate, "a page is not set straight into the isolate queue")
	assert(node.queue == nil, "the page is in no queue list")
	assert(object != none, "the backlink names an object")
	assert(node.object == none, "the page has no backlink yet")
	assert(node.pageOffset == 0, "the page has no offset yet")
	node.page = page
	node.object = object
	node.pageOffset = pageOffset
	assert(node.pageQueue == pageQueueNone, "the page is in no queue")
	node.pageQueue = queue
	pq.pageQueues[queue].pushFront(node)
	pq.pageQueueCounts[queue]++
}

// moveToQueueLockedList moves a queued page to the head of queue. It requires
// listLock.
func (pq *PageQueues[P, O]) moveToQueueLockedList(node *PageQueueNode[P, O], queue uint8) {
	var none O
	assert(queue != pageQueueReclaimIsolate, "a page is not moved straight into the isolate queue")
	assert(node.queue != nil, "the page is in a queue list")
	assert(node.object != none, "the page has a backlink")
	oldQueue := node.pageQueue
	node.pageQueue = queue
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	removeFromQueueList(node)
	pq.pageQueues[queue].pushFront(node)
	pq.pageQueueCounts[oldQueue]--
	pq.pageQueueCounts[queue]++
}

// moveToIsolateLockedList moves a queued page to an isolate queue's tail. It
// requires listLock.
func (pq *PageQueues[P, O]) moveToIsolateLockedList(node *PageQueueNode[P, O], isolateQueueIndex int) {
	var none O
	assert(isolateQueueIndex < numIsolateQueues, "the isolate queue exists")
	assert(node.queue != nil, "the page is in a queue list")
	assert(node.object != none, "the page has a backlink")
	oldQueue := node.pageQueue
	node.pageQueue = pageQueueReclaimIsolate
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	removeFromQueueList(node)
	pq.isolateQueues[isolateQueueIndex].pushBack(node)
	pq.pageQueueCounts[oldQueue]--
	pq.pageQueueCounts[pageQueueReclaimIsolate]++
}

// set puts a new page in the queue queue chooses, under the list lock, and
// checks the active ratio after if check: the body of every Set.
func (pq *PageQueues[P, O]) set(page P, object O, pageOffset uint64, queue func() uint8, check bool) {
	pq.listLock.Lock()
	pq.setQueueBacklinkLockedList(page, object, pageOffset, queue())
	pq.listLock.Unlock()
	if check {
		pq.maybeCheckActiveRatioAging(1)
	}
}

// move moves a queued page to the queue queue chooses, under the list lock,
// and checks the active ratio after: the body of every Move.
func (pq *PageQueues[P, O]) move(page P, queue func() uint8) {
	pq.listLock.Lock()
	pq.moveToQueueLockedList(page.QueueNode(), queue())
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

func fixedQueue(q uint8) func() uint8 { return func() uint8 { return q } }

// anonymousQueue is the queue an anonymous page goes to. It is called with
// listLock held.
func (pq *PageQueues[P, O]) anonymousQueue(skipReclaim bool) func() uint8 {
	return func() uint8 {
		if pq.anonymousIsReclaimable && !skipReclaim {
			return pq.mruGenToQueue()
		}
		return pageQueueAnonymous
	}
}

// SetWired puts a new page of object in the wired queue.
func (pq *PageQueues[P, O]) SetWired(page P, object O, pageOffset uint64) {
	pq.set(page, object, pageOffset, fixedQueue(pageQueueWired), false)
}

// MoveToWired moves a page to the wired queue.
func (pq *PageQueues[P, O]) MoveToWired(page P) { pq.move(page, fixedQueue(pageQueueWired)) }

// SetAnonymous puts a new anonymous page in its queue. skipReclaim keeps it
// out of the reclaim queues even if anonymous pages are reclaimable.
func (pq *PageQueues[P, O]) SetAnonymous(page P, object O, pageOffset uint64, skipReclaim bool) {
	pq.set(page, object, pageOffset, pq.anonymousQueue(skipReclaim), true)
}

// MoveToAnonymous moves a page to the anonymous queue.
func (pq *PageQueues[P, O]) MoveToAnonymous(page P, skipReclaim bool) {
	pq.move(page, pq.anonymousQueue(skipReclaim))
}

// SetHighPriority puts a new page in the high priority queue.
func (pq *PageQueues[P, O]) SetHighPriority(page P, object O, pageOffset uint64) {
	pq.set(page, object, pageOffset, fixedQueue(pageQueueHighPriority), false)
}

// MoveToHighPriority moves a page to the high priority queue.
func (pq *PageQueues[P, O]) MoveToHighPriority(page P) {
	pq.move(page, fixedQueue(pageQueueHighPriority))
}

// SetReclaim puts a new page a pager backs in the newest reclaim queue.
func (pq *PageQueues[P, O]) SetReclaim(page P, object O, pageOffset uint64) {
	pq.set(page, object, pageOffset, pq.mruGenToQueue, true)
}

// MoveToReclaim moves a page to the newest reclaim queue.
func (pq *PageQueues[P, O]) MoveToReclaim(page P) { pq.move(page, pq.mruGenToQueue) }

// MoveToReclaimDontNeed moves a page to the tail of the don't-need isolate
// queue, which is reclaimed first.
func (pq *PageQueues[P, O]) MoveToReclaimDontNeed(page P) {
	pq.listLock.Lock()
	pq.moveToIsolateLockedList(page.QueueNode(), isolateQueueDontNeed)
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// SetPagerBackedDirty puts a new dirty page in the dirty queue.
func (pq *PageQueues[P, O]) SetPagerBackedDirty(page P, object O, pageOffset uint64) {
	pq.set(page, object, pageOffset, fixedQueue(pageQueuePagerBackedDirty), false)
}

// MoveToPagerBackedDirty moves a page to the dirty queue.
func (pq *PageQueues[P, O]) MoveToPagerBackedDirty(page P) {
	pq.move(page, fixedQueue(pageQueuePagerBackedDirty))
}

// zeroForkQueue is the queue a zero fork goes to. It is called with listLock
// held.
func (pq *PageQueues[P, O]) zeroForkQueue() uint8 {
	if pq.zeroForkIsReclaimable {
		return pq.mruGenToQueue()
	}
	return pageQueueAnonymousZeroFork
}

// SetAnonymousZeroFork puts a new zero fork in its queue.
func (pq *PageQueues[P, O]) SetAnonymousZeroFork(page P, object O, pageOffset uint64) {
	pq.set(page, object, pageOffset, pq.zeroForkQueue, true)
}

// MoveAnonymousToAnonymousZeroFork moves a page in the anonymous queue to the
// zero fork queue. A page in another queue does not move.
func (pq *PageQueues[P, O]) MoveAnonymousToAnonymousZeroFork(page P) {
	node := page.QueueNode()
	pq.listLock.Lock()
	// Already where it belongs when both are the reclaim queues. Zircon
	// checks this first without the lock.
	if pq.zeroForkIsReclaimable && pq.anonymousIsReclaimable && queueIsReclaim(node.pageQueue) {
		pq.listLock.Unlock()
		return
	}
	queue := node.pageQueue
	if pq.anonymousIsReclaimable && !queueIsReclaim(queue) {
		pq.listLock.Unlock()
		return
	}
	if !pq.anonymousIsReclaimable && queue != pageQueueAnonymous {
		pq.listLock.Unlock()
		return
	}
	pq.moveToQueueLockedList(node, pq.zeroForkQueue())
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// CompressFailed moves a page in a reclaim queue to the failed reclaim queue,
// so it is not tried again.
func (pq *PageQueues[P, O]) CompressFailed(page P) {
	node := page.QueueNode()
	pq.listLock.Lock()
	if queueIsReclaim(node.pageQueue) {
		pq.moveToQueueLockedList(node, pageQueueFailedReclaim)
	}
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// ChangeObjectOffset changes a queued page's backlink. Only the page's owner
// may call it, under the object's lock.
func (pq *PageQueues[P, O]) ChangeObjectOffset(page P, object O, pageOffset uint64) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.changeObjectOffsetLockedList(page, object, pageOffset)
}

// ChangeObjectOffsetArray is ChangeObjectOffset for pages[i] at offsets[i].
func (pq *PageQueues[P, O]) ChangeObjectOffsetArray(pages []P, object O, offsets []uint64) {
	var none O
	assert(len(pages) == len(offsets), "every page has an offset")
	assert(object != none, "the backlink names an object")
	for i := 0; i < len(pages); {
		pq.listLock.Lock()
		// Some progress is made before the lock is checked.
		for {
			pq.changeObjectOffsetLockedList(pages[i], object, offsets[i])
			i++
			if i >= len(pages) || pq.batchOpShouldDropLock(i) {
				break
			}
		}
		pq.listLock.Unlock()
	}
}

// changeObjectOffsetLockedList is ChangeObjectOffsetLockedList. It requires
// listLock and the owner's lock.
func (pq *PageQueues[P, O]) changeObjectOffsetLockedList(page P, object O, pageOffset uint64) {
	var none O
	node := page.QueueNode()
	assert(node.queue != nil, "the page is in a queue list")
	assert(object != none, "the backlink names an object")
	assert(node.object != none, "the page has a backlink")
	node.object = object
	node.pageOffset = pageOffset
}

// Remove takes a page out of the queues and clears its backlink.
func (pq *PageQueues[P, O]) Remove(page P) {
	pq.listLock.Lock()
	pq.removeLockedList(page.QueueNode())
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
}

// RemoveArrayIntoList is Remove for every page, each then appended to out,
// Zircon's out_list.
func (pq *PageQueues[P, O]) RemoveArrayIntoList(pages []P, out *[]P) {
	for i := 0; i < len(pages); {
		pq.listLock.Lock()
		// Some progress is made before the lock is checked.
		for {
			pq.removeLockedList(pages[i].QueueNode())
			*out = append(*out, pages[i])
			i++
			if i >= len(pages) || pq.batchOpShouldDropLock(i) {
				break
			}
		}
		pq.listLock.Unlock()
	}
	pq.maybeCheckActiveRatioAging(len(pages))
}

func (pq *PageQueues[P, O]) removeLockedList(node *PageQueueNode[P, O]) {
	var none O
	oldQueue := node.pageQueue
	node.pageQueue = pageQueueNone
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	pq.pageQueueCounts[oldQueue]--
	node.object = none
	node.pageOffset = 0
	removeFromQueueList(node)
}

// IsPageReclaimable reports whether a page is in an isolate queue, where
// reclamation takes pages from. Zircon's is static and reads the queue number
// with an atomic load.
func (pq *PageQueues[P, O]) IsPageReclaimable(page P) bool {
	return pq.queueOf(page) == pageQueueReclaimIsolate
}

// queueOf is the queue a page is in.
func (pq *PageQueues[P, O]) queueOf(page P) uint8 {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return page.QueueNode().pageQueue
}

// VmoBacklink is a page with the object and offset it was found at, which
// the caller must check again under the object's lock.
type VmoBacklink[P any, O QueueObject] struct {
	Cow    O
	Page   P
	Offset uint64
}

// backlink is the backlink of a queued node. It requires listLock.
func backlink[P any, O QueueObject](node *PageQueueNode[P, O]) VmoBacklink[P, O] {
	var none O
	assert(node.object != none, "a queued page has a backlink")
	return VmoBacklink[P, O]{Cow: node.object, Page: node.page, Offset: node.pageOffset}
}

// PopAnonymousZeroFork moves the oldest page of the zero fork queue to the
// anonymous queue and returns its backlink, if the queue has a page.
func (pq *PageQueues[P, O]) PopAnonymousZeroFork() (VmoBacklink[P, O], bool) {
	pq.listLock.Lock()
	q := &pq.pageQueues[pageQueueAnonymousZeroFork]
	if q.isEmpty() {
		pq.listLock.Unlock()
		return VmoBacklink[P, O]{}, false
	}
	node := q.back()
	result := backlink(node)
	pq.moveToQueueLockedList(node, pageQueueAnonymous)
	pq.listLock.Unlock()
	pq.maybeCheckActiveRatioAging(1)
	return result, true
}

// peekIsolateList is the first page of the isolate queues, if any.
func (pq *PageQueues[P, O]) peekIsolateList() (VmoBacklink[P, O], bool) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	for i := range pq.isolateQueues {
		list := &pq.isolateQueues[i]
		if !list.isEmpty() {
			head := list.front()
			assert(head.pageQueue == pageQueueReclaimIsolate, "an isolate page is in the isolate queue")
			return backlink(head), true
		}
	}
	return VmoBacklink[P, O]{}, false
}

// PeekIsolate is the first page of the isolate queues. If they are empty it
// isolates pages from the LRU queues up to lowestQueue generations from the
// newest, never from the active queues. The page stays where it is.
func (pq *PageQueues[P, O]) PeekIsolate(lowestQueue uint64) (VmoBacklink[P, O], bool) {
	// Never take from the active queues.
	lowestQueue = max(lowestQueue, NumActiveQueues)
	const maxIterations = NumReclaim * 2
	loopIterations := 0
	for {
		if result, ok := pq.peekIsolateList(); ok {
			return result, true
		}
		if loopIterations > maxIterations {
			slog.Error("zirconvm: peeking the isolate queues iterated too often", "iterations", loopIterations)
		}
		loopIterations++
		// Aging outstanding when the peek began is synchronized with once,
		// so that the end computed from the MRU generation is reached.
		if loopIterations == 1 {
			pq.synchronizeWithAging()
		}
		pq.lock.Lock()
		// The limit is one larger than the lowest queue, since evicting
		// queue X is done by making X+1 the LRU queue.
		lruLimit := pq.mruGen.Load() - (lowestQueue - 1)
		lruTarget := pq.lruGen.Load() + 1
		pq.lock.Unlock()
		if lruTarget > lruLimit {
			return pq.peekIsolateList()
		}
		// A caller that peeks probably peeks again, so a few pages are
		// isolated at a time, and not the whole queue, which Zircon leaves to
		// its LRU thread.
		pq.processLruQueue(lruTarget, peekIsolateBatch)
	}
}

// ReclaimCounts are the reclaimable pages, the newest and the oldest of them,
// Zircon's PageQueues::ReclaimCounts.
type ReclaimCounts struct{ Total, Newest, Oldest int }

// GetReclaimQueueCounts counts the reclaimable pages. The isolate queues count
// as oldest, since their pages are reclaimed first.
func (pq *PageQueues[P, O]) GetReclaimQueueCounts() ReclaimCounts {
	var counts ReclaimCounts
	pq.lock.Lock()
	defer pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	lru, mru := pq.lruGen.Load(), pq.mruGen.Load()
	for index := lru; index <= mru; index++ {
		count := pq.pageQueueCounts[genToQueue(index)]
		// The distance to the MRU decides the bucket, as it does for a peek.
		if index > mru-NumActiveQueues {
			counts.Newest += count
		} else if index <= mru-(NumReclaim-numOldestQueues) {
			counts.Oldest += count
		}
		counts.Total += count
	}
	isolated := pq.pageQueueCounts[pageQueueReclaimIsolate]
	counts.Oldest += isolated
	counts.Total += isolated
	return counts
}

// Counts are the pages in each queue, Zircon's PageQueues::Counts. Reclaim is
// by age, the newest first.
type Counts struct {
	Reclaim           [NumReclaim]int
	ReclaimIsolate    int
	PagerBackedDirty  int
	Anonymous         int
	Wired             int
	AnonymousZeroFork int
	FailedReclaim     int
	HighPriority      int
}

// Total is every page counted, which Zircon does not sum.
func (c Counts) Total() int {
	total := c.ReclaimIsolate + c.PagerBackedDirty + c.Anonymous + c.Wired + c.AnonymousZeroFork +
		c.FailedReclaim + c.HighPriority
	for _, n := range c.Reclaim {
		total += n
	}
	return total
}

// QueueCounts are the pages in each queue, the reclaim queues by age.
func (pq *PageQueues[P, O]) QueueCounts() Counts {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	lru, mru := pq.lruGen.Load(), pq.mruGen.Load()
	var counts Counts
	for index := lru; index <= mru; index++ {
		counts.Reclaim[mru-index] = pq.pageQueueCounts[genToQueue(index)]
	}
	counts.ReclaimIsolate = pq.pageQueueCounts[pageQueueReclaimIsolate]
	counts.PagerBackedDirty = pq.pageQueueCounts[pageQueuePagerBackedDirty]
	counts.Anonymous = pq.pageQueueCounts[pageQueueAnonymous]
	counts.Wired = pq.pageQueueCounts[pageQueueWired]
	counts.AnonymousZeroFork = pq.pageQueueCounts[pageQueueAnonymousZeroFork]
	counts.FailedReclaim = pq.pageQueueCounts[pageQueueFailedReclaim]
	counts.HighPriority = pq.pageQueueCounts[pageQueueHighPriority]
	return counts
}

// ActiveInactiveCounts are the reclaimable pages that are active and that
// are not.
type ActiveInactiveCounts struct{ Active, Inactive int }

// GetActiveInactiveCounts counts the active and inactive reclaimable pages.
func (pq *PageQueues[P, O]) GetActiveInactiveCounts() ActiveInactiveCounts {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	return pq.getActiveInactiveCountsLocked()
}

// getActiveInactiveCountsLocked is GetActiveInactiveCounts. It requires lock.
func (pq *PageQueues[P, O]) getActiveInactiveCountsLocked() ActiveInactiveCounts {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	mru := pq.mruGenToQueue()
	var counts ActiveInactiveCounts
	for queue := range uint8(pageQueueNumQueues) {
		count := pq.pageQueueCounts[queue]
		if queueIsActive(queue, mru) {
			counts.Active += count
		}
		if queueIsInactive(queue, mru) {
			counts.Inactive += count
		}
	}
	return counts
}

// EnableAnonymousReclaim puts anonymous pages in the reclaim queues, and zero
// forks too if zeroForks. Pages already queued move over. It cannot be undone.
func (pq *PageQueues[P, O]) EnableAnonymousReclaim(zeroForks bool) {
	pq.listLock.Lock()
	pq.anonymousIsReclaimable = true
	pq.zeroForkIsReclaimable = zeroForks
	mruQueue := pq.mruGenToQueue()
	for !pq.pageQueues[pageQueueAnonymous].isEmpty() {
		pq.moveToQueueLockedList(pq.pageQueues[pageQueueAnonymous].front(), mruQueue)
	}
	for zeroForks && !pq.pageQueues[pageQueueAnonymousZeroFork].isEmpty() {
		pq.moveToQueueLockedList(pq.pageQueues[pageQueueAnonymousZeroFork].front(), mruQueue)
	}
	pq.listLock.Unlock()
	pq.lock.Lock()
	defer pq.lock.Unlock()
	pq.checkActiveRatioAgingLocked()
}

// ReclaimIsOnlyPagerBacked reports whether the reclaim queues hold only pages
// a pager backs.
func (pq *PageQueues[P, O]) ReclaimIsOnlyPagerBacked() bool {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return !pq.anonymousIsReclaimable
}

// debugPageIsSpecificReclaim reports whether a page is in a reclaim queue
// whose object passes validator, and its age.
func (pq *PageQueues[P, O]) debugPageIsSpecificReclaim(page P, validator func(O) bool) (bool, uint64) {
	var none O
	node := page.QueueNode()
	pq.listLock.Lock()
	q := node.pageQueue
	if q < pageQueueReclaimBase || q > pageQueueReclaimLast {
		pq.listLock.Unlock()
		return false, 0
	}
	age := uint64(queueAge(q, pq.mruGenToQueue()))
	cow := node.object
	assert(cow != none, "a reclaim page has a backlink")
	pq.listLock.Unlock()
	return validator(cow), age
}

func (pq *PageQueues[P, O]) debugPageIsSpecificQueue(page P, queue uint8, validator func(O) bool) bool {
	var none O
	node := page.QueueNode()
	pq.listLock.Lock()
	if node.pageQueue != queue {
		pq.listLock.Unlock()
		return false
	}
	cow := node.object
	assert(cow != none, "a queued page has a backlink")
	pq.listLock.Unlock()
	return validator(cow)
}

// DebugPageIsReclaim reports whether a page is in a reclaim queue, and which:
// 0 is the newest.
func (pq *PageQueues[P, O]) DebugPageIsReclaim(page P) (bool, uint64) {
	return pq.debugPageIsSpecificReclaim(page, func(O) bool { return true })
}

// DebugPageIsReclaimIsolate reports whether a page of an object that can
// evict is in the isolate queue.
func (pq *PageQueues[P, O]) DebugPageIsReclaimIsolate(page P) bool {
	return pq.debugPageIsSpecificQueue(page, pageQueueReclaimIsolate, O.CanEvict)
}

func cannotEvict[O QueueObject](cow O) bool { return !cow.CanEvict() }

// DebugPageIsAnonymous reports whether a page is in the anonymous queue.
func (pq *PageQueues[P, O]) DebugPageIsAnonymous(page P) bool {
	if pq.ReclaimIsOnlyPagerBacked() {
		return pq.queueOf(page) == pageQueueAnonymous
	}
	ok, _ := pq.debugPageIsSpecificReclaim(page, cannotEvict[O])
	return ok
}

// DebugPageIsAnonymousZeroFork reports whether a page is in the zero fork
// queue.
func (pq *PageQueues[P, O]) DebugPageIsAnonymousZeroFork(page P) bool {
	if pq.ReclaimIsOnlyPagerBacked() {
		return pq.queueOf(page) == pageQueueAnonymousZeroFork
	}
	ok, _ := pq.debugPageIsSpecificReclaim(page, cannotEvict[O])
	return ok
}

// DebugPageIsAnyAnonymous reports whether a page is in either anonymous queue.
func (pq *PageQueues[P, O]) DebugPageIsAnyAnonymous(page P) bool {
	return pq.DebugPageIsAnonymous(page) || pq.DebugPageIsAnonymousZeroFork(page)
}

// DebugPageIsPagerBackedDirty reports whether a page is in the dirty queue.
func (pq *PageQueues[P, O]) DebugPageIsPagerBackedDirty(page P) bool {
	return pq.queueOf(page) == pageQueuePagerBackedDirty
}

// DebugPageIsWired reports whether a page is in the wired queue.
func (pq *PageQueues[P, O]) DebugPageIsWired(page P) bool {
	return pq.queueOf(page) == pageQueueWired
}

// DebugPageIsHighPriority reports whether a page is in the high priority
// queue.
func (pq *PageQueues[P, O]) DebugPageIsHighPriority(page P) bool {
	return pq.queueOf(page) == pageQueueHighPriority
}

// DebugPageIsFailedReclaim reports whether a page is in the failed reclaim
// queue. Zircon reads that queue's count only.
func (pq *PageQueues[P, O]) DebugPageIsFailedReclaim(page P) bool {
	return pq.queueOf(page) == pageQueueFailedReclaim
}

// The walks below are not Zircon's. Zircon's evictor peeks the head of the
// isolate queues one page at a time and reclaims it (vm/evictor.cc). Until the
// evictor is ported, step 7 of the plan, the pager's own victim loop chooses
// among pages it may not be able to take, so it walks the queues instead.
// Each walk holds the list lock throughout, so its body must not call into
// the queues.

// DontNeed walks the don't-need isolate queue in the order a peek takes it:
// the page put there first comes first.
func (pq *PageQueues[P, O]) DontNeed() iter.Seq[P] {
	return func(yield func(P) bool) {
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		walkFront(&pq.isolateQueues[isolateQueueDontNeed], yield)
	}
}

// AnonymousZeroFork walks the zero fork queue in the order
// PopAnonymousZeroFork takes it: the page put there first comes first.
func (pq *PageQueues[P, O]) AnonymousZeroFork() iter.Seq[P] {
	return func(yield func(P) bool) {
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		walkBack(&pq.pageQueues[pageQueueAnonymousZeroFork], yield)
	}
}

// Reclaimable walks every page of the isolate and reclaim queues, oldest
// first: the isolate queues in the order a peek takes them, and then the
// reclaim queues from the LRU generation to the MRU one, which Zircon never
// peeks because they are active. First every generation older than the
// active ones is isolated, as a peek of every inactive queue would. A page
// accessed in a generation but still in an older generation's list is walked
// in the generation it was accessed in.
func (pq *PageQueues[P, O]) Reclaimable() iter.Seq[P] {
	return func(yield func(P) bool) {
		pq.lock.Lock()
		target := pq.mruGen.Load() - (NumActiveQueues - 1)
		pq.lock.Unlock()
		pq.processLruQueue(target, noIsolateLimit)
		pq.lock.Lock()
		defer pq.lock.Unlock()
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		for i := range pq.isolateQueues {
			if !walkFront(&pq.isolateQueues[i], yield) {
				return
			}
		}
		for gen := pq.lruGen.Load(); gen <= pq.mruGen.Load(); gen++ {
			queue := genToQueue(gen)
			for older := pq.lruGen.Load(); older <= gen; older++ {
				list := &pq.pageQueues[genToQueue(older)]
				for n := list.head.prev; n != &list.head; n = n.prev {
					if n.pageQueue == queue && !yield(n.page) {
						return
					}
				}
			}
		}
	}
}

// Pages walks every queued page, in no particular order.
func (pq *PageQueues[P, O]) Pages() iter.Seq[P] {
	return func(yield func(P) bool) {
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		for i := range pq.isolateQueues {
			if !walkFront(&pq.isolateQueues[i], yield) {
				return
			}
		}
		for i := range pq.pageQueues {
			if !walkFront(&pq.pageQueues[i], yield) {
				return
			}
		}
	}
}

// walkFront yields a list's pages from its head, and reports whether it went
// on to the end.
func walkFront[P any, O QueueObject](list *pageQueueList[P, O], yield func(P) bool) bool {
	for n := list.head.next; n != &list.head; n = n.next {
		if !yield(n.page) {
			return false
		}
	}
	return true
}

// walkBack yields a list's pages from its tail.
func walkBack[P any, O QueueObject](list *pageQueueList[P, O], yield func(P) bool) bool {
	for n := list.head.prev; n != &list.head; n = n.prev {
		if !yield(n.page) {
			return false
		}
	}
	return true
}
