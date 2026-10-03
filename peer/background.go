package peer

import (
	"context"
	"slices"
	"sync"
)

// Priority orders the bulk work one host asks of its peers. A host's
// background budget admits it in this order, and drops what may be dropped
// rather than queueing it.
type Priority uint8

const (
	// Unpublished is the post-copy stream's pages that no checkpoint holds:
	// they exist only on the source, and the source may not stop serving until
	// they are here. They wait for room; they are never dropped.
	Unpublished Priority = iota
	// Resident is the rest of the post-copy stream, which every checkpoint
	// holds as well. It waits for room too, behind every Unpublished.
	Resident
	// Fill is a keep that fills the cluster's cache from a store read or a
	// publication. One that finds no room is dropped.
	Fill
	// Repair is a keep that rebuilds a stripe no rank holds, the lowest of
	// every write. It is dropped as soon as half the budget is held.
	Repair
)

func (p Priority) String() string {
	return [...]string{"unpublished", "resident", "fill", "repair"}[min(int(p), 3)]
}

type priorityKey struct{}

// WithPriority marks the bulk requests made under ctx as priority. A bulk read
// whose context names none is Resident.
func WithPriority(ctx context.Context, priority Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, priority)
}

// PriorityOf reports the priority of the bulk requests made under ctx.
func PriorityOf(ctx context.Context) Priority {
	priority, ok := ctx.Value(priorityKey{}).(Priority)
	if !ok {
		return Resident
	}
	return priority
}

// Background is one host's budget for bulk work: the bytes its bulk requests
// may hold at all its peers at once. It paces the post-copy stream by its link
// rather than by a fixed rate — a stream gets what the link carries, in turns
// of the budget — and shrinks to a quarter while a guest fault is waiting on
// any peer, so bulk work yields the link to faults instead of counting on TCP to
// share it fairly. A request larger than the whole budget is admitted alone.
type Background struct {
	limit int64

	mu      sync.Mutex
	held    int64
	faults  int
	waiting []*backgroundWaiter
}

type backgroundWaiter struct {
	priority Priority
	bytes    int64
	ready    chan struct{}
	granted  bool
}

// NewBackground is a budget of limit bytes.
func NewBackground(limit int64) *Background { return &Background{limit: max(limit, 1)} }

// effective is the budget now: a quarter of it while a fault waits. Caller
// holds b.mu.
func (b *Background) effective() int64 {
	if b.faults > 0 {
		return max(b.limit/4, 1)
	}
	return b.limit
}

// fits reports room for bytes. Caller holds b.mu.
func (b *Background) fits(bytes, budget int64) bool {
	return b.held == 0 || b.held+bytes <= budget
}

// Acquire waits for room for bytes of work that waits rather than being
// dropped: Unpublished or Resident. It is admitted after every waiter of a
// higher priority and every earlier one of its own.
func (b *Background) Acquire(ctx context.Context, priority Priority, bytes int64) error {
	b.mu.Lock()
	if len(b.waiting) == 0 && b.fits(bytes, b.effective()) {
		b.held += bytes
		b.mu.Unlock()
		return nil
	}
	waiter := &backgroundWaiter{priority: priority, bytes: bytes, ready: make(chan struct{})}
	// Behind every waiter of its own priority or a higher one, ahead of the
	// rest.
	at := len(b.waiting)
	for i, other := range b.waiting {
		if other.priority > priority {
			at = i
			break
		}
	}
	b.waiting = slices.Insert(b.waiting, at, waiter)
	b.grant()
	b.mu.Unlock()
	select {
	case <-waiter.ready:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		defer b.mu.Unlock()
		if waiter.granted {
			b.held -= bytes
			b.grant()
		} else {
			b.waiting = slices.DeleteFunc(b.waiting, func(other *backgroundWaiter) bool { return other == waiter })
			b.grant()
		}
		return context.Cause(ctx)
	}
}

// TryAcquire takes room for bytes of work that is dropped rather than queued,
// Fill or Repair, and reports whether there was any. Work that waits always
// comes first, so there is none while any of it is waiting; a repair has only
// half the budget, so fills keep the rest.
func (b *Background) TryAcquire(priority Priority, bytes int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	budget := b.effective()
	if priority >= Repair {
		budget = max(budget/2, 1)
	}
	if len(b.waiting) > 0 || b.held+bytes > budget {
		return false
	}
	b.held += bytes
	return true
}

// Release gives back what Acquire or TryAcquire took.
func (b *Background) Release(bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= bytes
	b.grant()
}

// faultStarted and faultEnded count the guest faults in flight, which shrink
// the budget while there are any.
func (b *Background) faultStarted() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults++
}

func (b *Background) faultEnded() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults--
	b.grant()
}

// grant admits waiters from the head while they fit. Caller holds b.mu.
func (b *Background) grant() {
	for len(b.waiting) > 0 {
		next := b.waiting[0]
		if !b.fits(next.bytes, b.effective()) {
			return
		}
		b.waiting = b.waiting[1:]
		b.held += next.bytes
		next.granted = true
		close(next.ready)
	}
}

// BackgroundStatus is what the budget holds and who waits on it.
type BackgroundStatus struct {
	Limit, Held int64
	Faults      int
	Waiting     int
}

func (b *Background) Status() BackgroundStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BackgroundStatus{Limit: b.limit, Held: b.held, Faults: b.faults, Waiting: len(b.waiting)}
}
