package resource

import (
	"context"
	"errors"
)

// CacheEvictor drops one unused cache entry, returning true only after
// releasing the bytes it retained. Pinned or dirty state is not disposable
// cache. Callbacks run without the budget lock and must not allocate through
// Budget. Return false when nothing is evictable.
type CacheEvictor func(context.Context, int64) (bool, error)

type cacheReclaimer struct{ evict CacheEvictor }

// RegisterCache makes disposable cache subordinate to non-cache reservations.
// The returned unregister function is safe to call repeatedly. Stop cache work
// and release its charges before unregistering it.
func (b *Budget) RegisterCache(evict CacheEvictor) func() {
	entry := &cacheReclaimer{evict: evict}
	b.mu.Lock()
	b.caches = append(b.caches, entry)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, cached := range b.caches {
			if cached == entry {
				b.caches = append(b.caches[:i], b.caches[i+1:]...)
				return
			}
		}
	}
}

// TryAcquireCache reserves optional cache retention. It cannot displace a
// non-cache waiter or refill space being reclaimed for non-cache admission.
// Cache owners may discard their own unused entries before trying this call.
func (b *Budget) TryAcquireCache(ctx context.Context, amount int64) (*Lease, error) {
	if amount < 0 {
		return nil, ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reclaiming != 0 || len(b.queue) != 0 || !b.fitsLocked(amount) {
		return nil, ErrCapacity
	}
	b.used += amount
	return &Lease{budget: b, amount: amount}, nil
}

func (b *Budget) withCacheReclaim(ctx context.Context, requested int64, attempt func() error) error {
	err := attempt()
	if !errors.Is(err, ErrCapacity) {
		return err
	}
	b.mu.Lock()
	b.reclaiming++
	caches := append([]*cacheReclaimer(nil), b.caches...)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.reclaiming--
		b.mu.Unlock()
	}()
	var evictionErr error
	for {
		progress := false
		for _, cache := range caches {
			if err := context.Cause(ctx); err != nil {
				return err
			}
			evicted, err := cache.evict(ctx, requested)
			if evictionErr == nil {
				evictionErr = err
			}
			if !evicted {
				continue
			}
			progress = true
			if err := attempt(); !errors.Is(err, ErrCapacity) {
				return err
			}
		}
		if !progress {
			// An eviction racing another reclaimer may already have made room.
			if err := attempt(); !errors.Is(err, ErrCapacity) {
				return err
			}
			return errors.Join(ErrCapacity, evictionErr)
		}
	}
}

// CacheRetentionAllowed prevents a finishing reader from retaining optional
// bytes while non-cache work is waiting for those same bytes.
func (b *Budget) CacheRetentionAllowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reclaiming == 0 && len(b.queue) == 0
}
