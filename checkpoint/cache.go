package checkpoint

import (
	"cmp"
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// CacheConfig bounds simultaneous cache misses. Retention uses the shared host
// resource budget and yields unused entries to non-cache allocations.
type CacheConfig struct {
	// MaxConcurrentLoads bounds the fetches in flight. Default 16.
	MaxConcurrentLoads int
	// Disk is the file on the host's own disk the cache keeps envelopes in:
	// what a pull copies and what that VM's publications upload. What a file
	// of this deployment holds is read back when the cache is made; any other
	// file is emptied. The file is the caller's to close after the cache. A
	// nil Disk keeps nothing on disk, and every pull is refused.
	Disk platform.File
	// Deployment is the deployment the Disk belongs to, which its header
	// names, and Entropy what a new file's identity is drawn from: nil is the
	// operating system's.
	Deployment CacheDeployment
	Entropy    platform.Entropy
	// Budget is the host's disk limiter: the cache's share of the disk, and
	// which writes it may make. The share is the budget's alone, so a
	// DiskBytes beside one is refused. Without one, the share is DiskBytes
	// and every write is admitted; a zero DiskBytes then keeps nothing on
	// disk.
	Budget    DiskBudget
	DiskBytes int64
	// DiskRegionBytes is the size of one disk region, a multiple of 4 KiB.
	// Default DefaultDiskRegionBytes.
	DiskRegionBytes int64
	// DiskIndexBytes bounds the memory the disk's index may use; past it the
	// disk refuses writes rather than grow. Default DefaultDiskIndexBytes.
	DiskIndexBytes int64
	// DiskSecondChanceReads is how many reads since it was written give an
	// item a second chance before its region is given back. Default 1.
	DiskSecondChanceReads int
	// ClusterPercent is the share of windows, 0 to 100, the cluster cache is
	// turned on for, by a hash of the window. The disk keeps a window inside
	// the share as the stripes the list of caches puts on this cache, under
	// the list's code, and every other window whole, under 1+0, whatever the
	// list says. Zero, the default, keeps every window whole.
	ClusterPercent int
	// Peers is the host's table of peers, which the cache sends the keeps
	// that fill the cluster through and asks for fill rights. Nil reaches no
	// peer: a window inside the share is kept only where this cache holds it.
	Peers *peer.Table
	// Clock is what the rate of keeps and the interval of fill rights are
	// measured by. Nil is the wall clock.
	Clock platform.Clock
	// FillQueueBytes bounds the host's queue of writes to its own disk, which
	// every fill and every keep a peer sends goes through; a fill that finds
	// it full is dropped. Default DefaultFillQueueBytes.
	FillQueueBytes int64
	// FillBytesPerSecond is the rate of keeps this host sends its peers, with
	// a burst of one second of it; a keep past it is dropped. Default
	// DefaultFillBytesPerSecond.
	FillBytesPerSecond int64
	// FillRightInterval is how long a window's fill right, once this cache
	// gave it out, is not given again. Default DefaultFillRightInterval.
	FillRightInterval time.Duration
	// ClusterHedgeFloor is the least a read of the cluster waits for k
	// stripes of a window before it asks the rest of the window's ranks.
	// Default DefaultClusterHedgeFloor.
	ClusterHedgeFloor time.Duration
	// ClusterBound is the least a read of the cluster waits for its stripes
	// before it reads the store as well. Default DefaultClusterBound.
	ClusterBound time.Duration
	// ClusterStripeTimeout is how long one stripe request waits for its
	// answer before it counts as a timeout of the host it asked. Default
	// DefaultClusterStripeTimeout.
	ClusterStripeTimeout time.Duration
	// HeadCheckEvery is how many hits of the disk tier go by between two
	// checks, by a HEAD, that the part behind a hit still exists. Default
	// DefaultHeadCheckEvery; negative checks none.
	HeadCheckEvery int
}

// Cache shares immutable decoded pages among the stores and
// checkpoints of one host. Supply one cache per host rather than one per fork:
// a fork inherits its parent's object keys, so its reads hit the entries the
// parent already loaded. The cache owns no persistent workers and no durable
// state: what its disk keeps across a restart is only a copy of what the store
// holds. A cached object is never evidence that a publication landed.
// Construct it with NewCache; it must not be copied after first use.
type Cache struct {
	mu        sync.Mutex
	resources *resource.Budget
	// disk is the second tier, nil where the host keeps nothing on disk;
	// filler what fills the cluster from it, and reader what reads the
	// cluster through it, nil with it.
	disk       *cacheDisk
	filler     *filler
	reader     *clusterReader
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
	// Disk is the disk tier's, zero where the host keeps nothing on disk,
	// Fill what its fills of the cluster did, and Read what its reads of the
	// cluster did.
	Disk DiskStats
	Fill FillStats
	Read ReadStats
}

// NewCache registers the cache with the host resource owner. Close it when
// the host shuts down to release retention and unregister its evictor. A
// cache with a disk reads back what the disk holds, and fits it to its share,
// before it returns.
func NewCache(ctx context.Context, resources *resource.Budget, config CacheConfig) (*Cache, error) {
	if config.MaxConcurrentLoads == 0 {
		config.MaxConcurrentLoads = 16
	}
	config.DiskRegionBytes = cmp.Or(config.DiskRegionBytes, DefaultDiskRegionBytes)
	config.DiskIndexBytes = cmp.Or(config.DiskIndexBytes, DefaultDiskIndexBytes)
	config.DiskSecondChanceReads = cmp.Or(config.DiskSecondChanceReads, 1)
	config.FillQueueBytes = cmp.Or(config.FillQueueBytes, DefaultFillQueueBytes)
	config.FillBytesPerSecond = cmp.Or(config.FillBytesPerSecond, DefaultFillBytesPerSecond)
	config.FillRightInterval = cmp.Or(config.FillRightInterval, DefaultFillRightInterval)
	config.ClusterHedgeFloor = cmp.Or(config.ClusterHedgeFloor, DefaultClusterHedgeFloor)
	config.ClusterBound = cmp.Or(config.ClusterBound, DefaultClusterBound)
	config.ClusterStripeTimeout = cmp.Or(config.ClusterStripeTimeout, DefaultClusterStripeTimeout)
	config.HeadCheckEvery = cmp.Or(config.HeadCheckEvery, DefaultHeadCheckEvery)
	if config.FillQueueBytes < 0 || config.FillBytesPerSecond < 0 || config.FillRightInterval < 0 ||
		config.ClusterHedgeFloor < 0 || config.ClusterBound < 0 || config.ClusterStripeTimeout <= 0 {
		return nil, ErrInvalidConfig
	}
	if resources == nil || config.MaxConcurrentLoads < 1 || config.MaxConcurrentLoads > 1024 || config.DiskBytes < 0 ||
		config.DiskRegionBytes < minimumDiskRegionBytes || config.DiskRegionBytes > maximumDiskRegionBytes ||
		config.DiskRegionBytes%diskBlock != 0 || config.DiskIndexBytes < 0 || config.DiskSecondChanceReads < 0 ||
		config.DiskSecondChanceReads > wordReadsMax || config.ClusterPercent < 0 || config.ClusterPercent > 100 ||
		config.Budget != nil && config.DiskBytes != 0 ||
		!config.Deployment.storable() ||
		diskHeaderBytes(config.Deployment) > config.DiskRegionBytes {
		return nil, ErrInvalidConfig
	}
	cache := &Cache{resources: resources, limit: config.MaxConcurrentLoads,
		entries: make(map[cacheKey]*list.Element), flights: make(map[cacheKey]*cacheFlight), changed: make(chan struct{})}
	budget := config.Budget
	if budget == nil && config.DiskBytes > 0 {
		budget = fixedShare(config.DiskBytes)
	}
	if config.Disk != nil && budget != nil {
		disk, err := openCacheDisk(ctx, config.Disk, budget, diskSettings{regionBytes: config.DiskRegionBytes,
			indexLimit: config.DiskIndexBytes, threshold: config.DiskSecondChanceReads,
			deployment: config.Deployment, entropy: config.Entropy, clusterPercent: config.ClusterPercent})
		if err != nil {
			return nil, fmt.Errorf("the page cache's disk: %w", err)
		}
		cache.disk = disk
		cache.filler = newFiller(ctx, disk, fillSettings{peers: config.Peers, clock: config.Clock,
			queueBytes: config.FillQueueBytes, bytesPerSecond: config.FillBytesPerSecond,
			rightInterval: config.FillRightInterval})
		cache.reader = newClusterReader(ctx, disk, cache.filler, config.Peers, clusterSettings{
			hedgeFloor: config.ClusterHedgeFloor, bound: config.ClusterBound, stripeTimeout: config.ClusterStripeTimeout,
			probeFirst: DefaultProbeFirst, probeMax: DefaultProbeMax, headEvery: config.HeadCheckEvery})
	}
	cache.unregister = resources.RegisterCache(cache.reclaim)
	return cache, nil
}

// Stats reports the cache's current occupancy and cumulative counters.
func (c *Cache) Stats() CacheStats {
	var disk DiskStats
	var fill FillStats
	var read ReadStats
	if c.disk != nil {
		disk, fill, read = c.disk.stats(), c.filler.statistics(), c.reader.statistics()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{ResidentBytes: c.used, Entries: len(c.entries), ActiveLoads: c.active,
		PeakLoads: c.peak, Hits: c.hits, Misses: c.misses, CoalescedLoads: c.coalesced, Evictions: c.evictions,
		Disk: disk, Fill: fill, Read: read}
}

// FollowCaches has the cache's disk keep and read stripes by the list of
// caches the host holds, which caches returns: for each window inside the
// share CacheConfig.ClusterPercent turns on, the stripes of its envelopes the
// list ranks this cache for, under the list's code. Every other window, and
// every window until it is called, the disk keeps whole. It does nothing for
// a cache that keeps no disk.
func (c *Cache) FollowCaches(caches func() rank.List) {
	if c.disk != nil {
		c.disk.follow(caches)
	}
}

// SettleFills returns once every fill the cache was handed has been written
// or dropped, and every keep and fill right it asked for has been answered,
// the repairs and drops of its reads among them. Nothing waits on a fill;
// this is what a test, or a host about to say what its disk holds, waits on.
// It returns at once for a cache that keeps no disk.
func (c *Cache) SettleFills(ctx context.Context) error {
	if c.disk == nil {
		return nil
	}
	if err := c.reader.settle(ctx); err != nil {
		return err
	}
	return c.filler.settle(ctx)
}

// fill hands envelopes of kind to the cluster, each window inside the
// cluster share to its ranks, and returns at once. It does nothing for a
// cache that keeps no disk, and leaves every window outside the share alone.
func (c *Cache) fill(kind WriteKind, envelopes []envelope) {
	if c == nil || c.disk == nil || len(envelopes) == 0 {
		return
	}
	c.filler.fill(kind, envelopes)
}

// bug reports whether the in-tree bug id is on for the cache's run, as its
// fills see it.
func (c *Cache) bug(id string) bool { return c != nil && c.disk != nil && c.filler.bug(id) }

// fills reports whether the cache fills the cluster with any window: it keeps
// a disk, and the cluster cache is on for some share of windows.
func (c *Cache) fills() bool { return c != nil && c.disk != nil && c.disk.clusterPercent > 0 }

// FitDisk is what the host's disk limiter calls when the cache's share has
// fallen: the disk gives regions back, oldest first and with no second chance,
// until it holds no more than its share less one region. It does nothing for a
// cache that keeps no disk.
func (c *Cache) FitDisk(ctx context.Context) error {
	if c.disk == nil {
		return nil
	}
	return c.disk.fit(ctx)
}

// quiet returns once no load of the cache's own is in flight: no fault, and no
// other read, is waiting on the store. A pull waits here before each fetch, which
// is what puts it behind every fault rather than beside them.
func (c *Cache) quiet(ctx context.Context) error {
	for {
		c.mu.Lock()
		active, changed := c.active, c.changed
		c.mu.Unlock()
		if active == 0 {
			return context.Cause(ctx)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
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
// charges until they release their pins. A disk's open region is closed with
// its table, so the next cache over the file reads it back, and the disk
// takes no more writes. Calling it again does nothing: a host unwinds from
// wherever it failed, and the second call must not close a channel twice or
// unregister the evictor twice.
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
	if c.disk != nil {
		// The reads' requests and probes end first, since a read hands its
		// drops and repairs to the fills. What the fills had not done is
		// dropped: nothing waits on a fill.
		c.reader.close()
		c.filler.close()
		c.disk.shutdown(context.Background())
	}
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
// It also returns the envelopes it read from the store, which the load fills
// the cluster with once its callers have their bytes.
type fetcher func(ctx context.Context, wanted []int) ([][]byte, []envelope, error)

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

// get reads one object, fetching it where the cache does not hold it. load
// returns the object's bytes, and its envelope where the store served it.
func (c *Cache) get(ctx context.Context, key cacheKey,
	load func(context.Context) ([]byte, []envelope, error)) ([]byte, func(), error) {
	data, release, err := c.getAll(ctx, []cacheKey{key}, func(ctx context.Context, _ []int) ([][]byte, []envelope, error) {
		found, served, err := load(ctx)
		if err != nil {
			return nil, nil, err
		}
		return [][]byte{found}, served, nil
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
	fetched, served, err := fetch(ctx, wanted)
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
	if err != nil {
		served = nil
	}
	if c.bug("fill-blocks-read") {
		// The bug fills in front of the read: its callers wait for the fill
		// right, the split and the writes before they have their bytes.
		c.fill(WriteFillRead, served)
		if c.disk != nil {
			_ = c.filler.settle(ctx)
		}
		served = nil
	}
	c.finish(load, entries, err)
	// Behind the read, never in front of it: the callers have their bytes.
	c.fill(WriteFillRead, served)
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
