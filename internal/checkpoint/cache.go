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
// its identity, so that alone identifies them wherever the part holding them
// moves. A segment's identity is the
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

// cacheFlight is one key a load is fetching, and what every caller waiting for
// that key waits on.
type cacheFlight struct {
	done       chan struct{}
	load       *cacheLoad
	waiters    int
	generation uint64
	finished   bool
	entry      *cacheEntry
	err        error
}

// cacheLoad is one fetch the cache is running: the keys it is fetching, a
// flight for each of them, and the one slot of the concurrency budget they
// share. A read of a single object owns one flight; a read of a run of pages
// owns one per page it missed and still one slot, because what such a read
// issues is one request per extent of a part and not one per page.
//
// The load's context is cancelled once the last caller waiting on any of its
// flights has left, so one caller leaving never takes the fetch away from the
// others, whichever key each of them wanted.
type cacheLoad struct {
	cancel  context.CancelFunc
	keys    []cacheKey
	flights []*cacheFlight
	waiters int
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
		flight.load.cancel()
	}
	c.mu.Unlock()
	c.Clear()
	c.unregister()
}

// cacheAdmission is what one attempt at a batch of keys found: the retained
// entry it pinned for each key it already held, the flight it must wait on for
// every other, or the signal to try again once a running load has finished.
type cacheAdmission struct {
	entries []*cacheEntry
	flights []*cacheFlight
	err     error
	wait    <-chan struct{}
}

// fetcher fills one buffer per key of a load. It is given the positions within
// keys that this load owns, and returns their bytes in that order; the cache
// copies what it keeps, so a fetcher may hand back slices of its own buffers.
type fetcher func(ctx context.Context, wanted []int) ([][]byte, error)

func (c *Cache) admit(ctx context.Context, keys []cacheKey, fetch fetcher) cacheAdmission {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return cacheAdmission{err: resource.ErrClosed}
	}
	// What this call would have to fetch is settled before anything is pinned,
	// because a batch that has to wait for a slot must leave the cache exactly
	// as it found it and try again.
	var wanted []int
	for at, key := range keys {
		if c.entries[key] == nil && c.flights[key] == nil {
			wanted = append(wanted, at)
		}
	}
	if len(wanted) > 0 && c.active >= c.limit {
		return cacheAdmission{wait: c.changed}
	}
	found := cacheAdmission{entries: make([]*cacheEntry, len(keys)), flights: make([]*cacheFlight, len(keys))}
	for at, key := range keys {
		if element := c.entries[key]; element != nil {
			c.hits++
			c.lru.MoveToFront(element)
			entry := element.Value.(*cacheEntry)
			entry.readers++
			found.entries[at] = entry
			continue
		}
		if flight := c.flights[key]; flight != nil {
			flight.waiters++
			flight.load.waiters++
			c.coalesced++
			found.flights[at] = flight
		}
	}
	if len(wanted) == 0 {
		return found
	}
	// No individual caller owns a shared fetch. Its cancellation only stops
	// the fetch after the last waiter has left.
	loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	load := &cacheLoad{cancel: cancel, keys: make([]cacheKey, 0, len(wanted)),
		flights: make([]*cacheFlight, 0, len(wanted)), waiters: len(wanted)}
	for _, at := range wanted {
		flight := &cacheFlight{done: make(chan struct{}), load: load, waiters: 1, generation: c.generation}
		c.flights[keys[at]] = flight
		load.keys = append(load.keys, keys[at])
		load.flights = append(load.flights, flight)
		found.flights[at] = flight
		c.misses++
	}
	c.active++
	c.peak = max(c.peak, c.active)
	go c.run(loadCtx, load, wanted, fetch)
	return found
}

// get reads one object, fetching it where the cache does not hold it.
func (c *Cache) get(ctx context.Context, key cacheKey, load func(context.Context) ([]byte, error)) ([]byte, func(), error) {
	data, release, err := c.getAll(ctx, []cacheKey{key}, func(ctx context.Context, _ []int) ([][]byte, error) {
		found, err := load(ctx)
		if err != nil {
			return nil, err
		}
		return [][]byte{found}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return data[0], release, nil
}

// getAll reads several objects at once, fetching the ones the cache does not
// hold through one call of fetch. The keys must be distinct. It is what lets a
// reader of a run of pages fetch the pages it is missing in as few requests as
// the layout allows while every page stays cached under its own identity: what
// two readers share is the page, not the request that happened to carry it.
//
// The returned bytes are pinned until the one release is called, and nothing is
// pinned at all when it reports an error.
func (c *Cache) getAll(ctx context.Context, keys []cacheKey, fetch fetcher) ([][]byte, func(), error) {
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, nil, err
		}
		next := c.admit(ctx, keys, fetch)
		if next.err != nil {
			return nil, nil, next.err
		}
		if next.wait != nil {
			select {
			case <-next.wait:
				continue
			case <-ctx.Done():
				return nil, nil, context.Cause(ctx)
			}
		}
		return c.collect(ctx, keys, next)
	}
}

// collect waits for the flights an admission left and reports the bytes of
// every key with them. A failure releases everything this call pinned and
// leaves every flight it has not taken yet, so a read that fails holds nothing.
func (c *Cache) collect(ctx context.Context, keys []cacheKey, next cacheAdmission) ([][]byte, func(), error) {
	data := make([][]byte, len(keys))
	pinned := make([]*cacheEntry, 0, len(keys))
	for at, entry := range next.entries {
		if entry != nil {
			data[at], pinned = entry.data, append(pinned, entry)
		}
	}
	release := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, entry := range pinned {
			c.releaseLocked(entry)
		}
	}
	fail := func(from int, err error) ([][]byte, func(), error) {
		for at := from; at < len(keys); at++ {
			if flight := next.flights[at]; flight != nil {
				c.leave(keys[at], flight)
			}
		}
		release()
		return nil, nil, err
	}
	for at, flight := range next.flights {
		if flight == nil {
			continue
		}
		select {
		case <-flight.done:
			if err := context.Cause(ctx); err != nil {
				return fail(at, err)
			}
			if flight.err != nil {
				return fail(at+1, flight.err)
			}
			data[at], pinned = flight.entry.data, append(pinned, flight.entry)
		case <-ctx.Done():
			return fail(at, context.Cause(ctx))
		}
	}
	return data, sync.OnceFunc(release), nil
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
	flight.load.waiters--
	if flight.load.waiters == 0 {
		flight.load.cancel()
		// New callers must not join a fetch whose context was canceled. Its
		// occupied slot is released only when the actual loader finishes.
		for at, held := range flight.load.flights {
			if c.flights[flight.load.keys[at]] == held {
				delete(c.flights, flight.load.keys[at])
			}
		}
	}
}

func (c *Cache) run(ctx context.Context, load *cacheLoad, wanted []int, fetch fetcher) {
	defer load.cancel()
	fetched, err := fetch(ctx, wanted)
	if err == nil && len(fetched) != len(load.flights) {
		err = ErrCorrupt
	}
	entries := make([]*cacheEntry, len(load.flights))
	for at := range entries {
		if err != nil {
			break
		}
		// Try to reserve the owned copy, reclaiming unused cache first. If guest
		// pages or pinned readers occupy the allotment, serve this read through
		// transient I/O headroom and do not retain it. Cache capacity must not
		// prevent a required read.
		var lease *resource.Lease
		lease, err = c.resources.TryAcquire(ctx, int64(len(fetched[at]))+cacheEntryCharge)
		if errors.Is(err, resource.ErrCapacity) {
			err = context.Cause(ctx)
		}
		if err != nil {
			break
		}
		owned := make([]byte, len(fetched[at]))
		copy(owned, fetched[at])
		entries[at] = &cacheEntry{key: load.keys[at], data: owned, lease: lease}
	}
	if err != nil {
		// A load fails whole: the keys it had already copied are given back
		// rather than served beside an error nobody can use them with.
		for _, entry := range entries {
			if entry != nil {
				entry.dispose()
			}
		}
		clear(entries)
	}
	c.finish(load, entries, err)
}

func (c *Cache) finish(load *cacheLoad, entries []*cacheEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for at, flight := range load.flights {
		key, entry := load.keys[at], entries[at]
		flight.entry, flight.err, flight.finished = entry, err, true
		if c.flights[key] == flight {
			delete(c.flights, key)
		}
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
	}
	c.active--
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
