package checkpoint

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/resource"
)

// CacheConfig bounds simultaneous cache misses. Retention uses the shared host
// resource budget and yields unused entries to non-cache allocations.
type CacheConfig struct {
	// MaxConcurrentLoads bounds the fetches in flight. Default 16.
	MaxConcurrentLoads int
}

// Cache shares immutable decoded pages among the stores and
// checkpoints of one host. Supply one cache per host rather than one per fork:
// a fork inherits its parent's object keys, so its reads hit the entries the
// parent already loaded. The cache owns no persistent workers and no durable
// state, and a cached object is never evidence that a publication landed.
// Construct it with NewCache; it must not be copied after first use.
type Cache struct {
	mu         sync.Mutex
	resources  *resource.Budget
	unregister func()
	closed     bool
	limit      int
	used       int64
	entries    map[cacheKey]*list.Element
	lru        list.List
	flights    map[cacheKey]*cacheFlight
	active     int
	changed    chan struct{}
	generation uint64
	hits       uint64
	misses     uint64
	coalesced  uint64
	evictions  uint64
	peak       int
}

// cacheKey names what a cached copy holds. A page's bytes are immutable under
// its lineage identity and are never named by content, so that alone identifies
// them wherever the part holding them moves. A segment's identity is the
// checkpoint that wrote it, its volume and its number, so that is what it is
// keyed by: where in that checkpoint's index object it sits is only how it is
// fetched, and two roots addressing the same segment share the one copy.
type cacheKey struct {
	control.Identity
	// segment says the Identity names one segment of a volume's page table,
	// whose Ref is the checkpoint that wrote it and whose Page is its number,
	// rather than a page of that volume's contents.
	segment bool
}

// pageKey is the key one page's decoded bytes are cached under.
func pageKey(identity control.Identity) cacheKey { return cacheKey{Identity: identity} }

// segmentCacheKey is the key one segment's decoded bytes are cached under: the
// segment's identity, so a root that inherited the entry shares the copy.
func segmentCacheKey(volume string, number uint64, ref control.Ref) cacheKey {
	return cacheKey{Identity: control.Identity{Ref: ref, Volume: volume, Page: number},
		segment: true}
}

const cacheEntryCharge = int64(512)

type cacheEntry struct {
	key      cacheKey
	data     []byte // Immutable, borrowed only while readers holds a pin.
	readers  int
	retained bool
	lease    *resource.Lease // Nil for an unretained transient read.
}

func (entry *cacheEntry) dispose() {
	entry.data = nil
	if entry.lease != nil {
		entry.lease.Close()
	}
}

type cacheFlight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	waiters    int
	generation uint64
	finished   bool
	entry      *cacheEntry
	err        error
}

// CacheStats reports current occupancy and cumulative accounting. Hits, misses
// and eviction counters are monotonic for the cache's lifetime.
type CacheStats struct {
	// ResidentBytes includes retained objects and their bookkeeping charge.
	// Active readers detached by Clear remain charged to the host budget.
	ResidentBytes int64
	// Entries is the number of retained objects.
	Entries int
	// ActiveLoads and PeakLoads are the fetches in flight now and the most
	// that have ever been in flight at once.
	ActiveLoads int
	PeakLoads   int
	// Hits, Misses and CoalescedLoads count reads served from a retained
	// object, reads that started a fetch, and reads that joined one.
	Hits           uint64
	Misses         uint64
	CoalescedLoads uint64
	// Evictions counts entries dropped by local or shared pressure and clearing.
	Evictions uint64
}

// NewCache registers the cache with the host resource owner. Close it when
// the host shuts down to release retention and unregister its evictor.
func NewCache(resources *resource.Budget, config CacheConfig) (*Cache, error) {
	if config.MaxConcurrentLoads == 0 {
		config.MaxConcurrentLoads = 16
	}
	if resources == nil || config.MaxConcurrentLoads < 1 || config.MaxConcurrentLoads > 1024 {
		return nil, ErrInvalidConfig
	}
	cache := &Cache{resources: resources, limit: config.MaxConcurrentLoads,
		entries: make(map[cacheKey]*list.Element), flights: make(map[cacheKey]*cacheFlight), changed: make(chan struct{})}
	cache.unregister = resources.RegisterCache(cache.reclaim)
	return cache, nil
}

// Stats reports the cache's current occupancy and cumulative counters.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{ResidentBytes: c.used, Entries: len(c.entries), ActiveLoads: c.active,
		PeakLoads: c.peak, Hits: c.hits, Misses: c.misses, CoalescedLoads: c.coalesced, Evictions: c.evictions}
}

// Clear discards retained entries. Already-running loads can still satisfy
// their callers but cannot repopulate the cache after this call.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for c.lru.Len() != 0 {
		c.drop(c.lru.Back())
	}
}

// Close stops retention and releases unused bytes. Active readers keep their
// charges until they release their pins. Calling it again does nothing: a host
// unwinds from wherever it failed, and the second call must not close a channel
// twice or unregister the evictor twice.
func (c *Cache) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.changed)
	c.changed = make(chan struct{})
	for _, flight := range c.flights {
		flight.cancel()
	}
	c.mu.Unlock()
	c.Clear()
	c.unregister()
}

type cacheAdmission struct {
	entry  *cacheEntry
	err    error
	flight *cacheFlight
	wait   <-chan struct{}
}

func (c *Cache) admit(ctx context.Context, key cacheKey, load func(context.Context) ([]byte, error)) cacheAdmission {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return cacheAdmission{err: resource.ErrClosed}
	}
	if element := c.entries[key]; element != nil {
		c.hits++
		c.lru.MoveToFront(element)
		entry := element.Value.(*cacheEntry)
		entry.readers++
		return cacheAdmission{entry: entry}
	}
	if flight := c.flights[key]; flight != nil {
		flight.waiters++
		c.coalesced++
		return cacheAdmission{flight: flight}
	}
	if c.active >= c.limit {
		return cacheAdmission{wait: c.changed}
	}
	// No individual caller owns a shared fetch. Its cancellation only stops
	// the fetch after the last waiter has left.
	loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	flight := &cacheFlight{done: make(chan struct{}), cancel: cancel, waiters: 1, generation: c.generation}
	c.flights[key] = flight
	c.active++
	c.peak = max(c.peak, c.active)
	c.misses++
	go c.run(loadCtx, key, flight, load)
	return cacheAdmission{flight: flight}
}

func (c *Cache) get(ctx context.Context, key cacheKey, load func(context.Context) ([]byte, error)) ([]byte, func(), error) {
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, nil, err
		}
		next := c.admit(ctx, key, load)
		if next.err != nil {
			return nil, nil, next.err
		}
		if next.entry != nil {
			return next.entry.data, sync.OnceFunc(func() { c.release(next.entry) }), nil
		}
		if next.wait != nil {
			select {
			case <-next.wait:
				continue
			case <-ctx.Done():
				return nil, nil, context.Cause(ctx)
			}
		}
		select {
		case <-next.flight.done:
			if err := context.Cause(ctx); err != nil {
				c.leave(key, next.flight)
				return nil, nil, err
			}
			if next.flight.err != nil {
				return nil, nil, next.flight.err
			}
			entry := next.flight.entry
			return entry.data, sync.OnceFunc(func() { c.release(entry) }), nil
		case <-ctx.Done():
			c.leave(key, next.flight)
			return nil, nil, context.Cause(ctx)
		}
	}
}

func (c *Cache) leave(key cacheKey, flight *cacheFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if flight.finished {
		if flight.entry != nil {
			c.releaseLocked(flight.entry)
		}
		return
	}
	flight.waiters--
	if flight.waiters == 0 {
		flight.cancel()
		// New callers must not join a fetch whose context was canceled. Its
		// occupied slot is released only when the actual loader finishes.
		if c.flights[key] == flight {
			delete(c.flights, key)
		}
	}
}

func (c *Cache) run(ctx context.Context, key cacheKey, flight *cacheFlight, load func(context.Context) ([]byte, error)) {
	defer flight.cancel()
	data, err := load(ctx)
	var entry *cacheEntry
	if err == nil {
		// Try to reserve the owned copy, reclaiming unused cache first. If guest
		// pages or pinned readers occupy the allotment, serve this read through
		// transient I/O headroom and do not retain it. Cache capacity must not
		// prevent a required read.
		var lease *resource.Lease
		lease, err = c.resources.TryAcquire(ctx, int64(len(data))+cacheEntryCharge)
		if errors.Is(err, resource.ErrCapacity) {
			err = context.Cause(ctx)
		}
		if err == nil {
			owned := make([]byte, len(data))
			copy(owned, data)
			entry = &cacheEntry{key: key, data: owned, lease: lease}
		}
	}
	c.finish(key, flight, entry, err)
}

func (c *Cache) finish(key cacheKey, flight *cacheFlight, entry *cacheEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flight.entry, flight.err, flight.finished = entry, err, true
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
	c.active--
	if entry != nil {
		entry.readers = flight.waiters
		if entry.lease != nil && flight.waiters > 0 && flight.generation == c.generation && !c.closed && len(entry.data) > 0 {
			charge := entry.lease.Bytes()
			entry.retained = true
			c.entries[key] = c.lru.PushFront(entry)
			c.used += charge
		}
		if entry.readers == 0 {
			entry.dispose()
		}
	}
	close(flight.done)
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Cache) release(entry *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(entry)
}

func (c *Cache) releaseLocked(entry *cacheEntry) {
	entry.readers--
	if entry.readers == 0 && entry.retained && !c.resources.CacheRetentionAllowed() {
		c.drop(c.entries[entry.key])
		return
	}
	if entry.readers == 0 && !entry.retained {
		entry.dispose()
	}
}

// Caller holds mu. A detached entry remains charged while a reader pins it.
func (c *Cache) drop(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
	c.used -= entry.lease.Bytes()
	c.evictions++
	entry.retained = false
	if entry.readers == 0 {
		entry.dispose()
	}
}

func (c *Cache) reclaim(ctx context.Context, requested int64) (bool, error) {
	if requested == 0 {
		return false, nil
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for element := c.lru.Back(); element != nil; element = element.Prev() {
		if element.Value.(*cacheEntry).readers == 0 {
			c.drop(element)
			return true, nil
		}
	}
	return false, nil
}
