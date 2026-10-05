// Copyright 2020 The Fuchsia Authors
// Ported from zircon/kernel/vm/page_queues.cc and vm/include/vm/page_queues.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

import "sync"

// This is the part of Zircon's page queues that VmCowPages calls, ported for
// step 9 of the plan before step 5 ports the rest with its tests. Left out:
// the MRU and LRU threads, aging on a timer and by the active ratio (decision
// 4: a userfaultfd pager ages by faults only), the LRU actions that reclaim
// as pages are isolated, borrowing of loaned pages, high priority and the
// debug compressor. Aging happens on RotateReclaimQueues alone, which
// processes the LRU queue in line where Zircon waits for its LRU thread.

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
	// NumActiveQueues is kNumActiveQueues.
	NumActiveQueues = 2
	// kNumOldestQueues is kept for the counts Zircon reports.
	numOldestQueues = 2
	// The isolate queues: don't-need pages first, then standard aged pages.
	isolateQueueDontNeed = 0
	isolateQueueStandard = 1
	numIsolateQueues     = 2
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
)

// pageQueueList is Zircon's VmPageDoublyLinkedList for one queue: pages linked
// through their prev and next, with a sentinel.
type pageQueueList struct {
	head VmPage
	len  int
}

func (l *pageQueueList) init() {
	l.head.next = &l.head
	l.head.prev = &l.head
}

func (l *pageQueueList) isEmpty() bool { return l.head.next == &l.head }

// pushFront puts p at the head, where newer pages go.
func (l *pageQueueList) pushFront(p *VmPage) { l.insertAfter(p, &l.head) }

// pushBack puts p at the tail, where older pages are.
func (l *pageQueueList) pushBack(p *VmPage) { l.insertAfter(p, l.head.prev) }

func (l *pageQueueList) insertAfter(p, at *VmPage) {
	assert(p.queue == nil, "the page is in no queue list")
	p.prev = at
	p.next = at.next
	at.next.prev = p
	at.next = p
	p.queue = l
	l.len++
}

// popBack takes the page at the tail.
func (l *pageQueueList) popBack() *VmPage {
	assert(!l.isEmpty(), "the queue list is not empty")
	p := l.head.prev
	removeFromQueueList(p)
	return p
}

// front is the page at the head.
func (l *pageQueueList) front() *VmPage {
	assert(!l.isEmpty(), "the queue list is not empty")
	return l.head.next
}

// removeFromQueueList is RemoveFromContainer on a page's queue_node.
func removeFromQueueList(p *VmPage) {
	l := p.queue
	assert(l != nil, "the page is in a queue list")
	p.prev.next = p.next
	p.next.prev = p.prev
	p.prev, p.next, p.queue = nil, nil, nil
	l.len--
}

// PageQueues sort the pages of every object by how they can be reclaimed,
// Zircon's PageQueues.
type PageQueues struct {
	// listLock is Zircon's list_lock_, over the lists and the backlinks, and
	// lock its general lock_, over the generations. Where both are taken,
	// lock is taken first.
	listLock sync.Mutex
	lock     sync.Mutex

	pageQueues    [pageQueueNumQueues]pageQueueList
	isolateQueues [numIsolateQueues]pageQueueList

	// lruGen and mruGen are the generations of the oldest and newest reclaim
	// queues; the queues are a ring the generations map onto.
	lruGen, mruGen uint64

	// pageQueueCounts is the number of pages in each queue.
	pageQueueCounts [pageQueueNumQueues]int

	// zeroForkIsReclaimable and anonymousIsReclaimable are Zircon's switches
	// for whether anonymous pages age in the reclaim queues.
	zeroForkIsReclaimable, anonymousIsReclaimable bool
}

// NewPageQueues is a set of empty page queues.
func NewPageQueues() *PageQueues {
	pq := &PageQueues{mruGen: NumReclaim - 1}
	for i := range pq.pageQueues {
		pq.pageQueues[i].init()
	}
	for i := range pq.isolateQueues {
		pq.isolateQueues[i].init()
	}
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

func (pq *PageQueues) mruGenToQueue() uint8 { return genToQueue(pq.mruGen) }

// canIncrementMruGenLocked reports whether the MRU generation may advance, or
// the LRU queue must be processed first. It requires lock.
func (pq *PageQueues) canIncrementMruGenLocked() bool { return pq.mruGen-pq.lruGen < NumReclaim-1 }

// RotateReclaimQueues ages every reclaimable page by one queue. Zircon calls
// it for tests and debugging only; here it is the only aging there is.
func (pq *PageQueues) RotateReclaimQueues() {
	pq.lock.Lock()
	defer pq.lock.Unlock()
	// Force aging by processing the LRU queue until the MRU generation can
	// advance.
	for !pq.canIncrementMruGenLocked() {
		target := pq.lruGen + 1
		// Zircon drops the lock over the processing; ProcessLruQueue takes
		// it itself, so it is processed in line with the lock held, which
		// is the same here as no thread races it.
		pq.processLruQueueLocked(target)
	}
	pq.mruGen++
}

// processLruQueueLocked moves pages out of the LRU queue until lruGen reaches
// target: a page whose queue was updated by an access goes to its queue, and
// the rest go to the isolate queue. It is ProcessLruQueue with no limit on
// the pages isolated and no LRU action. It requires lock.
func (pq *PageQueues) processLruQueueLocked(target uint64) {
	assert(target <= pq.mruGen-(NumActiveQueues-1), "the target leaves the active queues")
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	for pq.lruGen < target {
		mruQueue := pq.mruGenToQueue()
		lruQueue := genToQueue(pq.lruGen)
		list := &pq.pageQueues[lruQueue]
		for !list.isEmpty() {
			// Newer pages are at the head and older at the tail, so take the
			// tail first.
			page := list.popBack()
			pageQueue := page.pageQueue
			assert(pageQueue >= pageQueueReclaimBase, "the page is in a reclaim queue")
			if pageQueue != lruQueue && queueIsValid(pageQueue, lruQueue, mruQueue) {
				// The page was accessed and belongs in a newer queue.
				pq.pageQueues[pageQueue].pushFront(page)
			} else {
				// Aged pages go to the standard isolate queue, at its tail,
				// so older pages stay at its head.
				oldQueue := page.pageQueue
				page.pageQueue = pageQueueReclaimIsolate
				pq.pageQueueCounts[oldQueue]--
				pq.pageQueueCounts[pageQueueReclaimIsolate]++
				pq.isolateQueues[isolateQueueStandard].pushBack(page)
			}
		}
		pq.lruGen++
	}
}

// markAccessedMaybeIsolate is MarkAccessed for a page that may be in the
// isolate queue, which must be moved between the lists.
func (pq *PageQueues) markAccessedMaybeIsolate(page *VmPage) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	// A page can only leave the reclaim queues under the list lock.
	if !queueIsReclaim(page.pageQueue) {
		return
	}
	pq.moveToQueueLockedList(page, pq.mruGenToQueue())
}

// MarkAccessed tells the queues the page was accessed, so it moves to the
// newest reclaim queue. A page in no reclaim queue does not move.
func (pq *PageQueues) MarkAccessed(page *VmPage) {
	pq.lock.Lock()
	target := pq.mruGenToQueue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	oldGen := page.pageQueue
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
		pq.markAccessedMaybeIsolate(page)
		return
	}
	// Zircon changes only the page's queue number, with a compare and swap,
	// and leaves the page in its old list for the LRU processing to move.
	page.pageQueue = target
	pq.pageQueueCounts[oldGen]--
	pq.pageQueueCounts[target]++
	pq.listLock.Unlock()
}

// setQueueBacklinkLockedList puts a page in no queue into queue, with its
// backlink. It requires listLock.
func (pq *PageQueues) setQueueBacklinkLockedList(page *VmPage, object *CowPages, pageOffset uint64, queue uint8) {
	assert(queue != pageQueueReclaimIsolate, "a page is not set straight into the isolate queue")
	assert(page.queue == nil, "the page is in no queue list")
	assert(object != nil, "the backlink names an object")
	assert(page.object == nil, "the page has no backlink yet")
	assert(page.pageOffset == 0, "the page has no offset yet")
	page.object = object
	page.pageOffset = pageOffset
	assert(page.pageQueue == pageQueueNone, "the page is in no queue")
	page.pageQueue = queue
	pq.pageQueues[queue].pushFront(page)
	pq.pageQueueCounts[queue]++
}

// moveToQueueLockedList moves a queued page to queue. It requires listLock.
func (pq *PageQueues) moveToQueueLockedList(page *VmPage, queue uint8) {
	assert(queue != pageQueueReclaimIsolate, "a page is not moved straight into the isolate queue")
	assert(page.queue != nil, "the page is in a queue list")
	assert(page.object != nil, "the page has a backlink")
	oldQueue := page.pageQueue
	page.pageQueue = queue
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	removeFromQueueList(page)
	pq.pageQueues[queue].pushFront(page)
	pq.pageQueueCounts[oldQueue]--
	pq.pageQueueCounts[queue]++
}

// moveToIsolateLockedList moves a queued page to an isolate queue's tail. It
// requires listLock.
func (pq *PageQueues) moveToIsolateLockedList(page *VmPage, isolateQueueIndex int) {
	assert(isolateQueueIndex < numIsolateQueues, "the isolate queue exists")
	assert(page.queue != nil, "the page is in a queue list")
	assert(page.object != nil, "the page has a backlink")
	oldQueue := page.pageQueue
	page.pageQueue = pageQueueReclaimIsolate
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	removeFromQueueList(page)
	pq.isolateQueues[isolateQueueIndex].pushBack(page)
	pq.pageQueueCounts[oldQueue]--
	pq.pageQueueCounts[pageQueueReclaimIsolate]++
}

func (pq *PageQueues) setQueue(page *VmPage, object *CowPages, pageOffset uint64, queue func() uint8) {
	pq.lock.Lock()
	q := queue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.setQueueBacklinkLockedList(page, object, pageOffset, q)
}

func (pq *PageQueues) moveToQueue(page *VmPage, queue func() uint8) {
	pq.lock.Lock()
	q := queue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.moveToQueueLockedList(page, q)
}

func fixedQueue(q uint8) func() uint8 { return func() uint8 { return q } }

// anonymousQueue is the queue an anonymous page goes to.
func (pq *PageQueues) anonymousQueue(skipReclaim bool) func() uint8 {
	return func() uint8 {
		if pq.anonymousIsReclaimable && !skipReclaim {
			return pq.mruGenToQueue()
		}
		return pageQueueAnonymous
	}
}

// SetWired puts a new page of object in the wired queue.
func (pq *PageQueues) SetWired(page *VmPage, object *CowPages, pageOffset uint64) {
	pq.setQueue(page, object, pageOffset, fixedQueue(pageQueueWired))
}

// MoveToWired moves a page to the wired queue.
func (pq *PageQueues) MoveToWired(page *VmPage) { pq.moveToQueue(page, fixedQueue(pageQueueWired)) }

// SetAnonymous puts a new anonymous page in its queue. skipReclaim keeps it
// out of the reclaim queues even if anonymous pages are reclaimable.
func (pq *PageQueues) SetAnonymous(page *VmPage, object *CowPages, pageOffset uint64, skipReclaim bool) {
	pq.setQueue(page, object, pageOffset, pq.anonymousQueue(skipReclaim))
}

// MoveToAnonymous moves a page to the anonymous queue.
func (pq *PageQueues) MoveToAnonymous(page *VmPage, skipReclaim bool) {
	pq.moveToQueue(page, pq.anonymousQueue(skipReclaim))
}

// SetReclaim puts a new page a pager backs in the newest reclaim queue.
func (pq *PageQueues) SetReclaim(page *VmPage, object *CowPages, pageOffset uint64) {
	pq.setQueue(page, object, pageOffset, pq.mruGenToQueue)
}

// MoveToReclaim moves a page to the newest reclaim queue.
func (pq *PageQueues) MoveToReclaim(page *VmPage) { pq.moveToQueue(page, pq.mruGenToQueue) }

// MoveToReclaimDontNeed moves a page to the don't-need isolate queue, which
// is reclaimed first.
func (pq *PageQueues) MoveToReclaimDontNeed(page *VmPage) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.moveToIsolateLockedList(page, isolateQueueDontNeed)
}

// SetPagerBackedDirty puts a new dirty page in the dirty queue.
func (pq *PageQueues) SetPagerBackedDirty(page *VmPage, object *CowPages, pageOffset uint64) {
	pq.setQueue(page, object, pageOffset, fixedQueue(pageQueuePagerBackedDirty))
}

// MoveToPagerBackedDirty moves a page to the dirty queue.
func (pq *PageQueues) MoveToPagerBackedDirty(page *VmPage) {
	pq.moveToQueue(page, fixedQueue(pageQueuePagerBackedDirty))
}

// zeroForkQueue is the queue a zero fork goes to.
func (pq *PageQueues) zeroForkQueue() uint8 {
	if pq.zeroForkIsReclaimable {
		return pq.mruGenToQueue()
	}
	return pageQueueAnonymousZeroFork
}

// SetAnonymousZeroFork puts a new zero fork in its queue.
func (pq *PageQueues) SetAnonymousZeroFork(page *VmPage, object *CowPages, pageOffset uint64) {
	pq.setQueue(page, object, pageOffset, pq.zeroForkQueue)
}

// MoveAnonymousToAnonymousZeroFork moves a page in the anonymous queue to the
// zero fork queue. A page in another queue does not move.
func (pq *PageQueues) MoveAnonymousToAnonymousZeroFork(page *VmPage) {
	pq.lock.Lock()
	target := pq.zeroForkQueue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	// Already where it belongs when both are the reclaim queues.
	if pq.zeroForkIsReclaimable && pq.anonymousIsReclaimable && queueIsReclaim(page.pageQueue) {
		return
	}
	queue := page.pageQueue
	if pq.anonymousIsReclaimable && !queueIsReclaim(queue) {
		return
	}
	if !pq.anonymousIsReclaimable && queue != pageQueueAnonymous {
		return
	}
	pq.moveToQueueLockedList(page, target)
}

// CompressFailed moves a page in a reclaim queue to the failed reclaim queue,
// so it is not tried again.
func (pq *PageQueues) CompressFailed(page *VmPage) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	if queueIsReclaim(page.pageQueue) {
		pq.moveToQueueLockedList(page, pageQueueFailedReclaim)
	}
}

// ChangeObjectOffset changes a queued page's backlink. Only the page's owner
// may call it, under the object's lock.
func (pq *PageQueues) ChangeObjectOffset(page *VmPage, object *CowPages, pageOffset uint64) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	assert(page.queue != nil, "the page is in a queue list")
	assert(object != nil, "the backlink names an object")
	assert(page.object != nil, "the page has a backlink")
	page.object = object
	page.pageOffset = pageOffset
}

// Remove takes a page out of the queues and clears its backlink.
func (pq *PageQueues) Remove(page *VmPage) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.removeLockedList(page)
}

func (pq *PageQueues) removeLockedList(page *VmPage) {
	oldQueue := page.pageQueue
	page.pageQueue = pageQueueNone
	assert(oldQueue != pageQueueNone, "the page was in a queue")
	pq.pageQueueCounts[oldQueue]--
	page.object = nil
	page.pageOffset = 0
	removeFromQueueList(page)
}

// IsPageReclaimable reports whether a page is in an isolate queue, where
// reclamation takes pages from.
func (pq *PageQueues) IsPageReclaimable(page *VmPage) bool {
	return pq.queueOf(page) == pageQueueReclaimIsolate
}

// queueOf is the queue a page is in. Zircon reads it with a relaxed atomic
// load of page_queue_priv.
func (pq *PageQueues) queueOf(page *VmPage) uint8 {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return page.pageQueue
}

// VmoBacklink is a page with the object and offset it was found at, which
// the caller must check again under the object's lock.
type VmoBacklink struct {
	Cow    *CowPages
	Page   *VmPage
	Offset uint64
}

// peekIsolateList is the first page of the isolate queues, if any.
func (pq *PageQueues) peekIsolateList() (VmoBacklink, bool) {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	for i := range pq.isolateQueues {
		list := &pq.isolateQueues[i]
		if !list.isEmpty() {
			head := list.front()
			assert(head.pageQueue == pageQueueReclaimIsolate, "an isolate page is in the isolate queue")
			assert(head.object != nil, "an isolate page has a backlink")
			return VmoBacklink{Cow: head.object, Page: head, Offset: head.pageOffset}, true
		}
	}
	return VmoBacklink{}, false
}

// PeekIsolate is the first page of the isolate queues. If they are empty it
// isolates pages from the LRU queues up to lowestQueue generations from the
// newest, never from the active queues.
func (pq *PageQueues) PeekIsolate(lowestQueue uint64) (VmoBacklink, bool) {
	// Never take from the active queues.
	lowestQueue = max(lowestQueue, NumActiveQueues)
	for {
		if result, ok := pq.peekIsolateList(); ok {
			return result, true
		}
		pq.lock.Lock()
		// The limit is one larger than the lowest queue, since evicting
		// queue X is done by making X+1 the LRU queue.
		lruLimit := pq.mruGen - (lowestQueue - 1)
		lruTarget := pq.lruGen + 1
		if lruTarget > lruLimit {
			pq.lock.Unlock()
			return pq.peekIsolateList()
		}
		// Zircon isolates at most 16 pages here and leaves the rest to its
		// LRU thread, which the port does not have; it processes the whole
		// generation.
		pq.processLruQueueLocked(lruTarget)
		pq.lock.Unlock()
	}
}

// Counts are the pages in each queue, Zircon's PageQueues::Counts.
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

// QueueCounts are the pages in each queue, the reclaim queues by age.
func (pq *PageQueues) QueueCounts() Counts {
	pq.lock.Lock()
	lru, mru := pq.lruGen, pq.mruGen
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
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
func (pq *PageQueues) GetActiveInactiveCounts() ActiveInactiveCounts {
	pq.lock.Lock()
	mru := pq.mruGenToQueue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
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
// forks too if zeroForks. Pages already queued move over.
func (pq *PageQueues) EnableAnonymousReclaim(zeroForks bool) {
	pq.lock.Lock()
	mruQueue := pq.mruGenToQueue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	pq.anonymousIsReclaimable = true
	pq.zeroForkIsReclaimable = zeroForks
	for !pq.pageQueues[pageQueueAnonymous].isEmpty() {
		pq.moveToQueueLockedList(pq.pageQueues[pageQueueAnonymous].front(), mruQueue)
	}
	for zeroForks && !pq.pageQueues[pageQueueAnonymousZeroFork].isEmpty() {
		pq.moveToQueueLockedList(pq.pageQueues[pageQueueAnonymousZeroFork].front(), mruQueue)
	}
}

// ReclaimIsOnlyPagerBacked reports whether the reclaim queues hold only pages
// a pager backs.
func (pq *PageQueues) ReclaimIsOnlyPagerBacked() bool {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return !pq.anonymousIsReclaimable
}

// debugPageIsSpecificReclaim reports whether a page is in a reclaim queue
// whose object passes validator, and its age.
func (pq *PageQueues) debugPageIsSpecificReclaim(page *VmPage, validator func(*CowPages) bool) (bool, uint64) {
	pq.lock.Lock()
	mru := pq.mruGenToQueue()
	pq.lock.Unlock()
	pq.listLock.Lock()
	q := page.pageQueue
	if q < pageQueueReclaimBase || q > pageQueueReclaimLast {
		pq.listLock.Unlock()
		return false, 0
	}
	age := uint64(queueAge(q, mru))
	cow := page.object
	assert(cow != nil, "a reclaim page has a backlink")
	pq.listLock.Unlock()
	return validator(cow), age
}

func (pq *PageQueues) debugPageIsSpecificQueue(page *VmPage, queue uint8, validator func(*CowPages) bool) bool {
	pq.listLock.Lock()
	if page.pageQueue != queue {
		pq.listLock.Unlock()
		return false
	}
	cow := page.object
	assert(cow != nil, "a queued page has a backlink")
	pq.listLock.Unlock()
	return validator(cow)
}

// DebugPageIsReclaim reports whether a page is in a reclaim queue, and which:
// 0 is the newest.
func (pq *PageQueues) DebugPageIsReclaim(page *VmPage) (bool, uint64) {
	return pq.debugPageIsSpecificReclaim(page, func(*CowPages) bool { return true })
}

// DebugPageIsReclaimIsolate reports whether a page of an object that can
// evict is in the isolate queue.
func (pq *PageQueues) DebugPageIsReclaimIsolate(page *VmPage) bool {
	return pq.debugPageIsSpecificQueue(page, pageQueueReclaimIsolate, (*CowPages).canEvict)
}

// DebugPageIsPagerBackedDirty reports whether a page is in the dirty queue.
func (pq *PageQueues) DebugPageIsPagerBackedDirty(page *VmPage) bool {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return page.pageQueue == pageQueuePagerBackedDirty
}

// DebugPageIsAnonymous reports whether a page is in the anonymous queue.
func (pq *PageQueues) DebugPageIsAnonymous(page *VmPage) bool {
	if pq.ReclaimIsOnlyPagerBacked() {
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		return page.pageQueue == pageQueueAnonymous
	}
	ok, _ := pq.debugPageIsSpecificReclaim(page, func(c *CowPages) bool { return !c.canEvict() })
	return ok
}

// DebugPageIsAnonymousZeroFork reports whether a page is in the zero fork
// queue.
func (pq *PageQueues) DebugPageIsAnonymousZeroFork(page *VmPage) bool {
	if pq.ReclaimIsOnlyPagerBacked() {
		pq.listLock.Lock()
		defer pq.listLock.Unlock()
		return page.pageQueue == pageQueueAnonymousZeroFork
	}
	ok, _ := pq.debugPageIsSpecificReclaim(page, func(c *CowPages) bool { return !c.canEvict() })
	return ok
}

// DebugPageIsAnyAnonymous reports whether a page is in either anonymous queue.
func (pq *PageQueues) DebugPageIsAnyAnonymous(page *VmPage) bool {
	return pq.DebugPageIsAnonymous(page) || pq.DebugPageIsAnonymousZeroFork(page)
}

// DebugPageIsWired reports whether a page is in the wired queue.
func (pq *PageQueues) DebugPageIsWired(page *VmPage) bool {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return page.pageQueue == pageQueueWired
}

// DebugPageIsFailedReclaim reports whether a page is in the failed reclaim
// queue. Zircon reads that queue's count only.
func (pq *PageQueues) DebugPageIsFailedReclaim(page *VmPage) bool {
	pq.listLock.Lock()
	defer pq.listLock.Unlock()
	return page.pageQueue == pageQueueFailedReclaim
}
