package checkpoint

import (
	"bytes"
	"cmp"
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// CacheConfig bounds simultaneous cache misses. Retention uses the shared host
// resource budget and yields unused entries to non-cache allocations.
type CacheConfig struct {
	// MaxConcurrentLoads bounds the fetches in flight for reads something
	// waits on. Default 16.
	MaxConcurrentLoads int
	// MaxConcurrentPrefetches bounds the fetches in flight for prefetches
	// (WithPrefetch). They have slots of their own, so a fault never waits
	// for a slot behind one. Default MaxConcurrentLoads.
	MaxConcurrentPrefetches int
	// Disk is the file on the host's own disk the cache keeps envelopes in:
	// what a pull copies and what that VM's publications upload. What a file
	// of this deployment holds is read back when the cache is made; any other
	// file is emptied. The file is the caller's to close after the cache. A
	// nil Disk keeps nothing on disk, and every pull is refused.
	Disk platform.File
	// Shards has the cache serve the shards the membership assigns this host,
	// each added as its network disk is attached (AddShard) and removed as
	// it is released (RemoveShard), beside its own Disk or without one. A
	// cache with shards fills and reads the cluster whatever disks it keeps
	// at the moment, none among them.
	Shards bool
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
	// the share as the stripes the membership puts on this disk, under the
	// membership's code, and every other window whole, under 1+0, whatever
	// the membership says. Zero, the default, keeps every window whole.
	ClusterPercent int
	// Peers is the host's table of peers, which the cache sends the keeps
	// that fill the cluster through and asks for fill rights. Nil reaches no
	// peer: a window inside the share is kept only where this cache holds it.
	Peers *peer.Table
	// Clock is what the rate of keeps and the interval of fill rights are
	// measured by, and, with no Peers, the waits of reads of the cluster:
	// with a table of peers those are on the table's clock, which its
	// requests are timed by. Nil is the wall clock.
	Clock platform.Clock
	// FillQueueBytes bounds the host's queue of writes to its own disk, which
	// every fill and every keep a peer sends goes through. A read's fill that
	// finds it full is dropped. A publication's waits while it holds three
	// quarters of the bound or more, so the publication goes at the pace its
	// fills do. Default DefaultFillQueueBytes.
	FillQueueBytes int64
	// FillBytesPerSecond is the rate of keeps this host sends its peers, with
	// a burst of one second of it. A read's keep past it is dropped, and a
	// publication's waits for it, leaving a quarter of the burst to reads.
	// Default DefaultFillBytesPerSecond.
	FillBytesPerSecond int64
	// FillWaitBound is the longest one wait of a publication's fills lasts:
	// for room in the queue, or a keep's for the rate, the background budget
	// or a busy holder. A publication that waited it out once waits no more,
	// and each of its fills that finds no room after is dropped. Default
	// DefaultFillWaitBound.
	FillWaitBound time.Duration
	// FillRightInterval is how long a window's fill right, once this cache
	// gave it out, is not given again. Default DefaultFillRightInterval.
	FillRightInterval time.Duration
	// FillKeepsInFlight bounds the publications' keeps and writes this host
	// has on their way at once, to all its peers and its own disks together.
	// Each holder has at most two of them, one on the wire and one behind it.
	// Default DefaultFillKeepsInFlight.
	FillKeepsInFlight int
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
	// disk is the host's own disk, the second tier, nil where the host keeps
	// nothing on its own disk. cluster is every disk the cache keeps for the
	// cluster, its own and the shards it serves, and how it places them;
	// filler what fills the cluster, and reader what reads it, nil for a
	// cache that keeps no disk and serves no shard. shard is how a shard's
	// disk is laid out.
	disk    *cacheDisk
	cluster *cluster
	shard   diskSettings
	filler  *filler
	reader  *clusterReader
	// pulls is the pulls the cache runs, which pressure cancels.
	pulls      *pulls
	unregister func()
	closed     bool
	limit      int
	// prefetchLimit bounds the prefetch loads in flight, and prefetches is
	// how many are; active counts them too.
	prefetchLimit int
	prefetches    int
	used          int64
	entries       map[cacheKey]*list.Element
	lru           list.List
	flights       map[cacheKey]*cacheFlight
	active        int
	changed       chan struct{}
	generation    uint64
	// pages is what reads of pages did, and tables what lookups of segments'
	// page tables did and what the cache holds of them.
	pages  cacheReads
	tables TableStats
	// prefetchLoads counts the loads prefetches started.
	prefetchLoads uint64
	evictions     uint64
	peak          int
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

// cacheEntry is one key's copy: a page's decoded member, or a segment's page
// table, decoded once (pageTable).
type cacheEntry struct {
	key      cacheKey
	data     []byte     // Immutable, borrowed only while readers holds a pin.
	table    *pageTable // Immutable, and held the same way.
	readers  int
	retained bool
	lease    *resource.Lease // Nil for an unretained transient read.
}

// held reports whether the entry holds anything worth keeping.
func (entry *cacheEntry) held() bool { return len(entry.data) > 0 || entry.table != nil }

func (entry *cacheEntry) dispose() {
	entry.data, entry.table = nil, nil
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
	// prefetch marks a load a prefetch started, which holds a prefetch slot.
	prefetch bool
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
	// that have ever been in flight at once, prefetches among them.
	ActiveLoads int
	PeakLoads   int
	// ActivePrefetches is the fetches in flight now that prefetches started,
	// and PrefetchLoads how many prefetches have started.
	ActivePrefetches int
	PrefetchLoads    uint64
	// Hits, Misses and CoalescedLoads count reads of pages served from a
	// retained page, reads that started a fetch, and reads that joined one.
	// Lookups of page tables are counted in Tables.
	Hits           uint64
	Misses         uint64
	CoalescedLoads uint64
	// Evictions counts entries dropped by local or shared pressure and
	// clearing, page tables among them.
	Evictions uint64
	// Tables is what the cache holds of segments' page tables, which
	// ResidentBytes and Entries count too, and what looking them up did.
	Tables TableStats
	// Disk is the disk tier's, zero where the host keeps nothing on its own
	// disk, Shards each shard's it serves now, Fill what its fills of the
	// cluster did, and Read what its reads of the cluster did.
	Disk   DiskStats
	Shards []DiskStats
	Fill   FillStats
	Read   ReadStats
}

// cacheReads is what reads of one kind of entry did: served from a retained
// entry, started a fetch, or joined one.
type cacheReads struct{ hits, misses, coalesced uint64 }

// TableStats is what a cache's memory tier holds of segments' page tables,
// each decoded once for every index that reads it (pageTable), and what
// looking them up did.
type TableStats struct {
	// Entries and Bytes are the tables it holds now and what they are
	// charged.
	Entries int
	Bytes   int64
	// Hits counts lookups a held table served, Loads the lookups that fetched
	// the segment and decoded it, Coalesced the lookups that joined a load in
	// flight, and Kept the tables a publication left as it wrote them.
	Hits, Loads, Coalesced, Kept uint64
}

// NewCache registers the cache with the host resource owner. Close it when
// the host shuts down to release retention and unregister its evictor. A
// cache with a disk reads back what the disk holds, and fits it to its share,
// before it returns.
func NewCache(ctx context.Context, resources *resource.Budget, config CacheConfig) (*Cache, error) {
	if config.MaxConcurrentLoads == 0 {
		config.MaxConcurrentLoads = 16
	}
	config.MaxConcurrentPrefetches = cmp.Or(config.MaxConcurrentPrefetches, config.MaxConcurrentLoads)
	config.DiskRegionBytes = cmp.Or(config.DiskRegionBytes, DefaultDiskRegionBytes)
	config.DiskIndexBytes = cmp.Or(config.DiskIndexBytes, DefaultDiskIndexBytes)
	config.DiskSecondChanceReads = cmp.Or(config.DiskSecondChanceReads, 1)
	config.FillQueueBytes = cmp.Or(config.FillQueueBytes, DefaultFillQueueBytes)
	config.FillBytesPerSecond = cmp.Or(config.FillBytesPerSecond, DefaultFillBytesPerSecond)
	config.FillRightInterval = cmp.Or(config.FillRightInterval, DefaultFillRightInterval)
	config.FillWaitBound = cmp.Or(config.FillWaitBound, DefaultFillWaitBound)
	config.FillKeepsInFlight = cmp.Or(config.FillKeepsInFlight, DefaultFillKeepsInFlight)
	config.ClusterHedgeFloor = cmp.Or(config.ClusterHedgeFloor, DefaultClusterHedgeFloor)
	config.ClusterBound = cmp.Or(config.ClusterBound, DefaultClusterBound)
	config.ClusterStripeTimeout = cmp.Or(config.ClusterStripeTimeout, DefaultClusterStripeTimeout)
	config.HeadCheckEvery = cmp.Or(config.HeadCheckEvery, DefaultHeadCheckEvery)
	if config.FillQueueBytes < 0 || config.FillBytesPerSecond < 0 || config.FillRightInterval < 0 ||
		config.FillWaitBound < 0 || config.FillKeepsInFlight < 0 || config.ClusterHedgeFloor < 0 || config.ClusterBound < 0 ||
		config.ClusterStripeTimeout <= 0 {
		return nil, ErrInvalidConfig
	}
	if resources == nil || config.MaxConcurrentLoads < 1 || config.MaxConcurrentLoads > 1024 ||
		config.MaxConcurrentPrefetches < 1 || config.MaxConcurrentPrefetches > 1024 || config.DiskBytes < 0 ||
		config.DiskRegionBytes < minimumDiskRegionBytes || config.DiskRegionBytes > maximumDiskRegionBytes ||
		config.DiskRegionBytes%diskBlock != 0 || config.DiskIndexBytes < 0 || config.DiskSecondChanceReads < 0 ||
		config.DiskSecondChanceReads > wordReadsMax || config.ClusterPercent < 0 || config.ClusterPercent > 100 ||
		config.Budget != nil && config.DiskBytes != 0 ||
		!config.Deployment.storable() ||
		diskHeaderBytes(config.Deployment) > config.DiskRegionBytes {
		return nil, ErrInvalidConfig
	}
	shared := newCluster(config.ClusterPercent)
	settings := diskSettings{regionBytes: config.DiskRegionBytes, indexLimit: config.DiskIndexBytes,
		threshold: config.DiskSecondChanceReads, deployment: config.Deployment, entropy: config.Entropy,
		cluster: shared}
	cache := &Cache{resources: resources, limit: config.MaxConcurrentLoads, prefetchLimit: config.MaxConcurrentPrefetches,
		cluster: shared, shard: settings, pulls: newPulls(),
		entries: make(map[cacheKey]*list.Element), flights: make(map[cacheKey]*cacheFlight), changed: make(chan struct{})}
	budget := config.Budget
	if budget == nil && config.DiskBytes > 0 {
		budget = fixedShare(config.DiskBytes)
	}
	if config.Disk != nil && budget != nil {
		disk, err := openCacheDisk(ctx, config.Disk, budget, settings)
		if err != nil {
			return nil, fmt.Errorf("the page cache's disk: %w", err)
		}
		cache.disk = disk
		shared.own = disk
	}
	if cache.disk != nil || config.Shards {
		cache.filler = newFiller(ctx, shared, fillSettings{peers: config.Peers, clock: config.Clock,
			queueBytes: config.FillQueueBytes, bytesPerSecond: config.FillBytesPerSecond,
			rightInterval: config.FillRightInterval, waitBound: config.FillWaitBound,
			keepsInFlight: config.FillKeepsInFlight})
		cache.reader = newClusterReader(ctx, shared, cache.filler, config.Peers, config.Clock, clusterSettings{
			hedgeFloor: config.ClusterHedgeFloor, bound: config.ClusterBound, stripeTimeout: config.ClusterStripeTimeout,
			probeFirst: DefaultProbeFirst, probeMax: DefaultProbeMax, headEvery: config.HeadCheckEvery})
	}
	cache.unregister = resources.RegisterCache(cache.reclaim)
	return cache, nil
}

// Stats reports the cache's current occupancy and cumulative counters.
func (c *Cache) Stats() CacheStats {
	var disk DiskStats
	var shards []DiskStats
	var fill FillStats
	var read ReadStats
	if c.disk != nil {
		disk = c.disk.stats()
	}
	for _, held := range c.cluster.all() {
		if held != c.disk {
			shards = append(shards, held.stats())
		}
	}
	if c.filler != nil {
		fill, read = c.filler.statistics(), c.reader.statistics()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{ResidentBytes: c.used, Entries: len(c.entries), ActiveLoads: c.active,
		PeakLoads: c.peak, ActivePrefetches: c.prefetches, PrefetchLoads: c.prefetchLoads,
		Hits: c.pages.hits, Misses: c.pages.misses, CoalescedLoads: c.pages.coalesced, Evictions: c.evictions,
		Tables: c.tables, Disk: disk, Shards: shards, Fill: fill, Read: read}
}

// FollowMembership has the cache's disks keep and read stripes by the
// membership source holds, as the host of member: for each window inside the
// share CacheConfig.ClusterPercent turns on, the stripes of its envelopes the
// membership ranks each disk for, under the membership's code, while the
// membership has member serve the disk. Every other window, and every window
// until it is called, the host's own disk keeps whole.
func (c *Cache) FollowMembership(source membership.Source, member rank.Identity) {
	c.cluster.follow(source, member)
}

// AddShard has the cache serve a shard, a network disk the membership
// assigned this host under shard.Lease: its header is checked, its lease
// taken, and what it holds read back before it returns, and the cache keeps
// and serves it from then on, while the membership has this host serve it.
// A shard leased under a newer assignment is refused with ErrFenced, and
// nothing on it is changed.
func (c *Cache) AddShard(ctx context.Context, shard ShardConfig) error {
	if c.filler == nil {
		return fmt.Errorf("%w: the cache was not made to serve shards", ErrInvalidConfig)
	}
	if shard.Device == nil || shard.Identity.IsZero() || shard.Budget == nil || shard.Lease.Assigned == 0 ||
		shard.Lease.Member.IsZero() {
		return fmt.Errorf("%w: a shard needs a device, an identity, a budget and a lease", ErrInvalidConfig)
	}
	if c.cluster.keeps(shard.Identity) {
		return fmt.Errorf("%w: the cache keeps disk %s already", ErrInvalidConfig, shard.Identity)
	}
	settings := c.shard
	lease := shard.Lease
	settings.identity, settings.lease = shard.Identity, &lease
	disk, err := openCacheDisk(ctx, shard.Device, shard.Budget, settings)
	if err != nil {
		return fmt.Errorf("shard %s: %w", shard.Identity, err)
	}
	if err := c.cluster.add(disk); err != nil {
		disk.shutdown(ctx)
		return err
	}
	return nil
}

// RemoveShard stops the cache serving a shard: no request takes it from here
// on, and once the last that did has finished its open region is closed with
// its table, so the next member to open it reads it back. The device is the
// caller's to close after.
func (c *Cache) RemoveShard(ctx context.Context, identity rank.Identity) error {
	disk, err := c.cluster.remove(ctx, identity)
	if err != nil {
		return err
	}
	disk.shutdown(ctx)
	return nil
}

// Keeps reports whether the cache keeps the disk of identity now: its own, or
// a shard it serves.
func (c *Cache) Keeps(identity rank.Identity) bool { return c.cluster.keeps(identity) }

// CheckShard reads a shard's lease back, and reports ErrFenced where another
// member took it, or the error of a device that no longer reads: either way
// the shard is to be removed.
func (c *Cache) CheckShard(ctx context.Context, identity rank.Identity) error {
	disk, release, kept := c.cluster.hold(identity)
	if !kept {
		return fmt.Errorf("%w: %s", ErrNotKept, identity)
	}
	defer release()
	return disk.checkLease(ctx)
}

// Fenced reports whether a shard the cache serves found its lease taken by
// another member's: the shard writes nothing more, and its member should
// remove it.
func (c *Cache) Fenced(identity rank.Identity) bool {
	disk, release, kept := c.cluster.hold(identity)
	if !kept {
		return false
	}
	defer release()
	disk.mu.Lock()
	defer disk.mu.Unlock()
	return disk.fenced
}

// SettleFills returns once every fill the cache was handed has been written
// or dropped, and every keep and fill right it asked for has been answered,
// the repairs and drops of its reads among them. Nothing waits on a fill;
// this is what a test, or a host about to say what its disk holds, waits on.
// It returns at once for a cache that fills nothing.
func (c *Cache) SettleFills(ctx context.Context) error {
	if c.filler == nil {
		return nil
	}
	if err := c.reader.settle(ctx); err != nil {
		return err
	}
	return c.filler.settle(ctx)
}

// SettleReads returns once nothing the cache's reads of the cluster started
// still runs: every request has been answered or has timed out, every read of
// the store past a bound has returned, and every read whose caller went on
// without it has ended. A read that the store answered first, or whose caller
// gave up on it, leaves its requests running behind it, loading the holders'
// disks and the network; this is what a run that measures one batch of reads
// after another waits on between them. It returns at once for a cache that
// reads no cluster.
func (c *Cache) SettleReads(ctx context.Context) error {
	if c.reader == nil {
		return nil
	}
	return c.reader.idle(ctx)
}

// fill hands envelopes of kind to the cluster, each window inside the
// cluster share to its ranks, and returns at once. It does nothing for a
// cache that fills nothing, and leaves every window outside the share alone.
func (c *Cache) fill(kind WriteKind, envelopes []envelope) {
	if c == nil || c.filler == nil || len(envelopes) == 0 {
		return
	}
	c.filler.fill(kind, envelopes)
}

// publish hands what a publication made durable to the cluster, each window
// inside the cluster share to its ranks, and returns once each window is
// handed over or dropped: a window waits under ctx for room in the queue, as
// the publication's pace allows. It does nothing for a cache that fills
// nothing.
func (c *Cache) publish(ctx context.Context, pace *fillPace, envelopes []envelope) {
	if c == nil || c.filler == nil || len(envelopes) == 0 {
		return
	}
	c.filler.publish(ctx, pace, envelopes)
}

// bug reports whether the in-tree bug id is on for the cache's run, as its
// fills see it.
func (c *Cache) bug(id string) bool { return c != nil && c.filler != nil && c.filler.bug(id) }

// fills reports whether the cache fills the cluster with any window: it keeps
// a disk or serves shards, and the cluster cache is on for some share of
// windows.
func (c *Cache) fills() bool { return c != nil && c.filler != nil && c.cluster.percent > 0 }

// FitDisk is what the host's disk limiter calls when the cache's share has
// fallen: the disk gives regions back, oldest first and with no second chance,
// until it holds no more than its share less one region. It does nothing for a
// cache that keeps no disk.
//
// The disk is short of room, so every pull's reads in flight are cancelled
// first and each pull stops short with ErrPressure: a pull would fill the disk
// again as it gives regions back.
func (c *Cache) FitDisk(ctx context.Context) error {
	if c.disk == nil {
		return nil
	}
	c.pulls.press(errDiskPressure)
	return c.disk.fit(ctx)
}

// holdLoadSlot takes one of the slots of the loads something waits on, as a
// fault's load does, until the release it returns is called. Only the guard
// pull-takes-a-fault-slot takes one outside a load.
func (c *Cache) holdLoadSlot(ctx context.Context) (func(), error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, resource.ErrClosed
		}
		if c.active-c.prefetches < c.limit {
			c.active++
			c.peak = max(c.peak, c.active)
			c.mu.Unlock()
			return sync.OnceFunc(func() {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.active--
				close(c.changed)
				c.changed = make(chan struct{})
			}), nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
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
	if c.filler != nil {
		// The reads' requests and probes end first, since a read hands its
		// drops and repairs to the fills. What the fills had not done is
		// dropped: nothing waits on a fill.
		c.reader.close()
		c.filler.close()
	}
	for _, disk := range c.cluster.all() {
		disk.shutdown(context.Background())
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
// keys that this load owns, and returns their bytes in that order.
type fetcher func(ctx context.Context, wanted []int) (fetched, error)

// fetched is what one fetch returns: the bytes of each key it was asked for,
// in order, which of them are its own to give, none where owned is nil, and
// the envelopes it read from the store, which the load fills the cluster with
// once its callers have their bytes. Bytes a fetch owns are in a buffer
// nothing else holds or will change, little longer than they are, and the
// cache keeps them as they are. Any others, a slice of a buffer the fetch
// shares among keys or uses again, the cache copies.
type fetched struct {
	data   [][]byte
	owned  []bool
	served []envelope
}

// owns reports whether the bytes at are the fetch's own to give.
func (f fetched) owns(at int) bool { return at < len(f.owned) && f.owned[at] }

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
	// A prefetch's load takes a slot of the prefetches', and every other load
	// one of the rest, so a fault never waits for a slot a prefetch holds.
	prefetch := Prefetching(ctx)
	if len(wanted) > 0 && (prefetch && c.prefetches >= c.prefetchLimit ||
		!prefetch && c.active-c.prefetches >= c.limit) {
		return cacheAdmission{wait: c.changed}
	}
	found := cacheAdmission{entries: make([]*cacheEntry, len(keys)), flights: make([]*cacheFlight, len(keys))}
	for at, key := range keys {
		if element := c.entries[key]; element != nil {
			if key.segment {
				c.tables.Hits++
			} else {
				c.pages.hits++
			}
			c.lru.MoveToFront(element)
			entry := element.Value.(*cacheEntry)
			entry.readers++
			found.entries[at] = entry
			continue
		}
		if flight := c.flights[key]; flight != nil {
			flight.waiters++
			flight.load.waiters++
			if key.segment {
				c.tables.Coalesced++
			} else {
				c.pages.coalesced++
			}
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
		flights: make([]*cacheFlight, 0, len(wanted)), waiters: len(wanted), prefetch: prefetch}
	for _, at := range wanted {
		flight := &cacheFlight{done: make(chan struct{}), load: load, waiters: 1, generation: c.generation}
		c.flights[keys[at]] = flight
		load.keys = append(load.keys, keys[at])
		load.flights = append(load.flights, flight)
		found.flights[at] = flight
		if keys[at].segment {
			c.tables.Loads++
		} else {
			c.pages.misses++
		}
	}
	c.active++
	c.peak = max(c.peak, c.active)
	if prefetch {
		c.prefetches++
		c.prefetchLoads++
	}
	go c.run(loadCtx, load, wanted, fetch)
	return found
}

// get reads one object, fetching it where the cache does not hold it. load
// returns the object's bytes, and its envelope where the store served it.
func (c *Cache) get(ctx context.Context, key cacheKey,
	load func(context.Context) ([]byte, []envelope, error)) ([]byte, func(), error) {
	entries, release, err := c.getEntries(ctx, []cacheKey{key}, single(load))
	if err != nil {
		return nil, nil, err
	}
	return entries[0].data, release, nil
}

// table reads one segment's page table under its key, fetching the segment
// and decoding it where the cache does not hold it decoded. load returns the
// segment's bytes, and its envelope where the store served it. Every reader
// of a segment shares the one decode, as readers of a page share its fetch.
func (c *Cache) table(ctx context.Context, key cacheKey,
	load func(context.Context) ([]byte, []envelope, error)) (*pageTable, func(), error) {
	if !key.segment {
		return nil, nil, fmt.Errorf("%w: %v is not a segment", ErrInvalidConfig, key)
	}
	entries, release, err := c.getEntries(ctx, []cacheKey{key}, single(load))
	if err != nil {
		return nil, nil, err
	}
	return entries[0].table, release, nil
}

// keepTable retains a segment's page table that this host has without a load:
// one a publication just wrote, which the readers of the index it published
// look pages up in next. It is charged and evicted like any other entry. A
// key the cache holds or is loading already is left as it is, since a segment
// is the same table under its identity however it got here, and a table
// there is no room for is not kept.
func (c *Cache) keepTable(ctx context.Context, key cacheKey, table *pageTable) {
	lease, err := c.resources.TryAcquire(ctx, table.charge()+cacheEntryCharge)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.entries[key] != nil || c.flights[key] != nil {
		lease.Close()
		return
	}
	c.retainLocked(&cacheEntry{key: key, table: table, lease: lease})
	c.tables.Kept++
}

// retainLocked keeps an entry, most recently used, charged at its lease.
// Caller holds mu.
func (c *Cache) retainLocked(entry *cacheEntry) {
	entry.retained = true
	c.entries[entry.key] = c.lru.PushFront(entry)
	c.used += entry.lease.Bytes()
	if entry.table != nil {
		c.tables.Entries++
		c.tables.Bytes += entry.lease.Bytes()
	}
}

// single is a fetcher of one key, whose bytes the cache copies.
func single(load func(context.Context) ([]byte, []envelope, error)) fetcher {
	return func(ctx context.Context, _ []int) (fetched, error) {
		found, served, err := load(ctx)
		if err != nil {
			return fetched{}, err
		}
		return fetched{data: [][]byte{found}, served: served}, nil
	}
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
	entries, release, err := c.getEntries(ctx, keys, fetch)
	if err != nil {
		return nil, nil, err
	}
	data := make([][]byte, len(entries))
	for at, entry := range entries {
		data[at] = entry.data
	}
	return data, release, nil
}

// getEntries is getAll's entries, pinned until the one release is called: a
// page's holds its bytes and a segment's its page table.
func (c *Cache) getEntries(ctx context.Context, keys []cacheKey, fetch fetcher) ([]*cacheEntry, func(), error) {
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

// collect waits for the flights an admission left and reports the entry of
// every key with them. A failure releases everything this call pinned and
// leaves every flight it has not taken yet, so a read that fails holds nothing.
func (c *Cache) collect(ctx context.Context, keys []cacheKey, next cacheAdmission) ([]*cacheEntry, func(), error) {
	entries := make([]*cacheEntry, len(keys))
	pinned := make([]*cacheEntry, 0, len(keys))
	for at, entry := range next.entries {
		if entry != nil {
			entries[at], pinned = entry, append(pinned, entry)
		}
	}
	release := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, entry := range pinned {
			c.releaseLocked(entry)
		}
	}
	fail := func(from int, err error) ([]*cacheEntry, func(), error) {
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
			// Every caller of a load is released at once when it ends, and
			// each goes on to decide what it reads next, from this host's
			// disk or from the store. In a controlled run they go on one at a
			// time, in the order the run chooses.
			if err := sim.Admit(ctx, "checkpoint/cache/flight"); err != nil {
				return fail(at, err)
			}
			if err := context.Cause(ctx); err != nil {
				return fail(at, err)
			}
			if flight.err != nil {
				return fail(at+1, flight.err)
			}
			entries[at], pinned = flight.entry, append(pinned, flight.entry)
		case <-ctx.Done():
			return fail(at, context.Cause(ctx))
		}
	}
	return entries, sync.OnceFunc(release), nil
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
	got, err := fetch(ctx, wanted)
	if err == nil && len(got.data) != len(load.flights) {
		err = ErrCorrupt
	}
	served := got.served
	entries := make([]*cacheEntry, len(load.flights))
	for at := range entries {
		if err != nil {
			break
		}
		// What is kept of a segment is its page table, decoded here once for
		// every reader waiting on the load and every one after it; of a page,
		// its bytes.
		entry := &cacheEntry{key: load.keys[at]}
		charge := int64(len(got.data[at]))
		if entry.key.segment {
			if entry.table, err = decodeTable(ctx, got.data[at]); err != nil {
				break
			}
			charge = entry.table.charge()
		}
		// Try to reserve the entry, reclaiming unused cache first. If guest
		// pages or pinned readers occupy the allotment, serve this read through
		// transient I/O headroom and do not retain it. Cache capacity must not
		// prevent a required read.
		entry.lease, err = c.resources.TryAcquire(ctx, charge+cacheEntryCharge)
		if errors.Is(err, resource.ErrCapacity) {
			entry.lease, err = nil, context.Cause(ctx)
		}
		if err != nil {
			break
		}
		if !entry.key.segment {
			// A page's entry keeps the bytes a fetch owns as they are, and a
			// copy of any others, made with no zeroing first. Either way it
			// holds no more of their buffer than they are.
			kept := got.data[at]
			if !got.owns(at) {
				kept = bytes.Clone(kept)
			}
			entry.data = slices.Clip(kept)
		}
		entries[at] = entry
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
			if entry.lease != nil && flight.waiters > 0 && flight.generation == c.generation && !c.closed && entry.held() {
				c.retainLocked(entry)
			}
			if entry.readers == 0 {
				entry.dispose()
			}
		}
		close(flight.done)
	}
	c.active--
	if load.prefetch {
		c.prefetches--
	}
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
	if entry.table != nil {
		c.tables.Entries--
		c.tables.Bytes -= entry.lease.Bytes()
	}
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
