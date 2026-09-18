// Package resource accounts reservations against one host allotment of RAM,
// which is what the pager's resident pages and the page cache's retained objects are
// taken from. Owners reserve before allocating and release only after the bytes
// are no longer retained. Accounting complements, but does not measure, runtime
// overhead or physical usage. Ordinary heap and runtime overhead use the
// deployment's container headroom; large retained allocations use this owner.
//
// Disk is not accounted here. Each concern that writes to the node's disk has a
// fixed cap in its own directory — the pager's spill is bounded by the dirty
// pages its geometry allows, and a starting process wipes what it finds — so
// there is no shared ledger to order or reclaim across.
package resource

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrInvalid  = errors.New("invalid resource reservation")
	ErrCapacity = errors.New("host resource capacity exhausted")
	ErrClosed   = errors.New("resource reservation closed")
)

// Budget is shared by every consumer of one allotment. Create it before
// starting consumers. Do not copy a Budget.
type Budget struct {
	mu         sync.Mutex
	limit      int64
	caches     []*cacheReclaimer
	reclaiming int
	used       int64
	queue      []*waiter
	changed    chan struct{}
}

type waiter struct {
	amount  int64
	granted chan struct{}
}

// New creates the owner of a whole allotment in bytes.
func New(limit int64) (*Budget, error) {
	if limit < 0 {
		return nil, ErrInvalid
	}
	return &Budget{limit: limit, changed: make(chan struct{})}, nil
}

// Stats is an atomic view of reservations, not a heap measurement.
type Stats struct {
	Limit, Used int64
	Waiting     int
}

func (b *Budget) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{Limit: b.limit, Used: b.used, Waiting: len(b.queue)}
}

// Changed closes when capacity or its ownership changes. Nonblocking owners
// capture it before trying admission, then wait on it after a refusal.
func (b *Budget) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func (b *Budget) fitsLocked(amount int64) bool { return amount <= b.limit-b.used }

// Lease owns a reservation. Its owner may grow it before allocation, return
// part after reclamation, and close it once. Closing repeatedly is safe.
// A lease must not be copied. Budget serializes all of its methods.
type Lease struct {
	budget *Budget
	amount int64
	closed bool
}

// TryAcquire reserves without waiting. It does not bypass existing waiters.
// Failure consumes no capacity.
func (b *Budget) TryAcquire(ctx context.Context, amount int64) (*Lease, error) {
	var lease *Lease
	err := b.withCacheReclaim(ctx, amount, func() error {
		var err error
		lease, err = b.tryAcquire(ctx, amount)
		return err
	})
	return lease, err
}

func (b *Budget) tryAcquire(ctx context.Context, amount int64) (*Lease, error) {
	if amount < 0 {
		return nil, ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queue) != 0 || !b.fitsLocked(amount) {
		return nil, ErrCapacity
	}
	b.used += amount
	return &Lease{budget: b, amount: amount}, nil
}

// Acquire waits in FIFO order. Owners must not wait while holding locks a cache
// evictor needs.
func (b *Budget) Acquire(ctx context.Context, amount int64) (*Lease, error) {
	if lease, err := b.TryAcquire(ctx, amount); !errors.Is(err, ErrCapacity) {
		return lease, err
	}
	if amount < 0 {
		return nil, ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if amount > b.limit {
		b.mu.Unlock()
		return nil, ErrCapacity
	}
	lease := &Lease{budget: b, amount: amount}
	if len(b.queue) == 0 && b.fitsLocked(amount) {
		b.used += amount
		b.mu.Unlock()
		return lease, nil
	}
	wait := &waiter{amount: amount, granted: make(chan struct{})}
	b.queue = append(b.queue, wait)
	b.mu.Unlock()
	// Close the race between the first reclaim attempt and queue insertion:
	// cache bytes released by a reader or filled in that gap must not strand
	// this waiter. Once queued, cache retention is suppressed until admission.
	_ = b.withCacheReclaim(ctx, amount, func() error {
		select {
		case <-wait.granted:
			return nil
		default:
			return ErrCapacity
		}
	})
	select {
	case <-wait.granted:
		// A grant crossing cancellation must return its capacity as well.
		if err := context.Cause(ctx); err != nil {
			lease.Close()
			return nil, err
		}
		return lease, nil
	case <-ctx.Done():
		b.mu.Lock()
		queued := false
		for i, item := range b.queue {
			if item == wait {
				copy(b.queue[i:], b.queue[i+1:])
				b.queue[len(b.queue)-1] = nil
				b.queue = b.queue[:len(b.queue)-1]
				queued = true
				break
			}
		}
		if !queued {
			b.used -= amount
		}
		lease.closed, lease.amount = true, 0
		b.grantLocked()
		b.mu.Unlock()
		return nil, context.Cause(ctx)
	}
}

func (b *Budget) grantLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
	for len(b.queue) > 0 {
		wait := b.queue[0]
		if !b.fitsLocked(wait.amount) {
			break
		}
		b.queue[0] = nil
		b.queue = b.queue[1:]
		b.used += wait.amount
		close(wait.granted)
	}
}

func (l *Lease) Bytes() int64 {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	return l.amount
}

// TryGrow reserves additional bytes before the owner grows its allocation.
func (l *Lease) TryGrow(ctx context.Context, extra int64) error {
	return l.budget.withCacheReclaim(ctx, extra, func() error { return l.tryGrow(ctx, extra) })
}

func (l *Lease) tryGrow(ctx context.Context, extra int64) error {
	if extra < 0 {
		return ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if len(b.queue) != 0 || !b.fitsLocked(extra) {
		return ErrCapacity
	}
	b.used += extra
	l.amount += extra
	return nil
}

// Release returns only bytes the owner has actually stopped retaining.
func (l *Lease) Release(amount int64) error {
	if amount < 0 {
		return ErrInvalid
	}
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if amount > l.amount {
		return ErrInvalid
	}
	b.used -= amount
	l.amount -= amount
	b.grantLocked()
	return nil
}

func (l *Lease) Close() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.closed {
		return
	}
	b.used -= l.amount
	l.closed, l.amount = true, 0
	b.grantLocked()
}
