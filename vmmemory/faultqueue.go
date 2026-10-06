package vmmemory

import (
	"sync"
	"time"
)

// faultQueue is one session's faults waiting for its fault workers, and the
// pages those workers are serving. A page is queued once however many of its
// accesses trapped. A page that faults again while it is served is queued
// again and served after, which upgrades a read fault that became a write.
type faultQueue struct {
	mu sync.Mutex
	// limit bounds the pages queued; see ConnectionConfig.QueuePages.
	limit   int
	pending map[uint64]queuedFault
	// serving is every page a worker is serving, and whether that fault
	// changes something: whether it is neither repeated nor a twin.
	serving map[uint64]bool
}

// queuedFault is one page waiting for a worker: whether any of its trapped
// accesses was a store, and when the first of them was read from the UFFD,
// which is what the queue-delay histogram measures against.
type queuedFault struct {
	write bool
	at    time.Time
	// twin marks accesses trapped while a fault that brings their page in was
	// being served: two vCPUs faulting one page at once. That fault wakes
	// them, so serving them again finds the page mapped, but they are not a
	// repeated fault the session is charged for. Only a fault that changes
	// something has a twin, so twins cannot follow each other for free.
	twin bool
}

func newFaultQueue(limit int) *faultQueue {
	return &faultQueue{limit: limit, pending: make(map[uint64]queuedFault), serving: make(map[uint64]bool)}
}

// add queues one access trapped at now. It reports false and queues nothing
// when the page is not queued yet and the queue is full.
func (q *faultQueue) add(page uint64, write bool, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, queued := q.pending[page]
	if !queued {
		if len(q.pending) >= q.limit {
			return false
		}
		// A page that faults again while queued keeps the first reading, so
		// the delay is measured against the access that has waited longest.
		entry.at = now
		changes, busy := q.serving[page]
		entry.twin = busy && changes
	}
	entry.write = entry.write || write
	q.pending[page] = entry
	return true
}

// take gives a worker a queued fault that no worker is serving. It reports
// whether the fault is a repeated fault the session is charged for, which
// repeated decides for every fault that is not a twin.
func (q *faultQueue) take(repeated func(page uint64, write bool) bool) (page uint64, entry queuedFault, repeat, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for page, entry := range q.pending {
		if _, busy := q.serving[page]; busy {
			continue
		}
		delete(q.pending, page)
		repeat = !entry.twin && repeated(page, entry.write)
		q.serving[page] = !entry.twin && !repeat
		return page, entry, repeat, true
	}
	return 0, queuedFault{}, false, false
}

// finish ends a worker's serve of page. It reports whether the page is queued
// again.
func (q *faultQueue) finish(page uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.serving, page)
	_, again := q.pending[page]
	return again
}

// requeue queues a fault a worker gave back, merged with any access to its page
// that trapped since, so the delay is measured against the access that has
// waited longest.
func (q *faultQueue) requeue(page uint64, entry queuedFault) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if existing, ok := q.pending[page]; ok {
		entry.write = entry.write || existing.write
		if existing.at.Before(entry.at) {
			entry.at = existing.at
		}
	}
	q.pending[page] = entry
}
