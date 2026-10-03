package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// The page cache's disk is its second tier: envelopes on the host's own disk,
// keyed by page identity exactly as the memory tier is. It holds what the
// object store holds — each member's and each segment's encoded envelope, byte
// for byte, whole or as the stripes of it the list of caches puts on this
// cache (diskstripes.go) — so a read from it is the same read as one from the
// store, checked by the same envelope. Nothing on it is evidence that a
// publication landed, and nothing publishes from it. A newer checkpoint's page
// has a new identity, so the copy of the page it replaced is never read for it.
//
// The disk is a log of fixed-size disk regions (diskformat.go). One region at a
// time is open. Its space is allocated when it opens, and items are appended to
// it in the order they arrive. A full region is closed: its items are synced,
// its table is written at its end, and it is synced again. The index in memory
// is kept per window (diskindex.go).
//
// Nothing about a VM evicts anything. When the cache needs room, the oldest
// closed region goes. Before it is given back, the items in it read at least
// the threshold since they were written are written again into the open
// region, up to half a region, and the rest go. One region of the share is kept
// free for that second chance alone, so eviction always gives space back. A
// read in flight holds its region, which is given back only once its last
// reader has finished.
//
// The disk survives a restart of the host. Its file's header names the cache
// and its deployment, and its closed regions are read back from their tables
// when it opens (diskrestart.go).

// ErrNoDisk refuses a pull on a host whose page cache keeps nothing on disk.
var ErrNoDisk = errors.New("checkpoint: the page cache keeps no disk")

// ErrDiskFull refuses a pull whose checkpoint does not fit in the page cache's
// disk however much of it were given back.
var ErrDiskFull = errors.New("checkpoint: the page cache's disk is full")

// ErrDiskRefused reports a write the page cache's disk refused: its share has
// no room for a region, the write budget refused it, or its index is at its
// memory bound. Nothing was written.
var ErrDiskRefused = errors.New("checkpoint: the page cache's disk refused a write")

// errNoRoom stops a second chance that would need more than the free region.
var errNoRoom = errors.New("checkpoint: no room for a second chance")

// DefaultDiskRegionBytes is the size of one disk region.
const DefaultDiskRegionBytes = 64 << 20

// DefaultDiskIndexBytes is the memory the disk's index may use.
const DefaultDiskIndexBytes = 64 << 20

// The bounds a region's size is held within: room for any envelope, and an
// offset in the region that fits a table's 32 bits.
const (
	minimumDiskRegionBytes = 64 << 10
	maximumDiskRegionBytes = 1 << 30
)

// diskBlock is the filesystem block a region's size is a multiple of, so a
// region given back is punched out whole.
const diskBlock = 4 << 10

// pullConcurrency is how many fetches every pull on a host has in flight
// together. A pull is background work: it takes few of the store's requests,
// and none of the page cache's load slots, so a fault never queues behind it.
const pullConcurrency = 2

// WriteKind says what a write to the disk is for. A budget that must drop
// writes drops the lowest kind first.
type WriteKind int

const (
	// WriteRepair restores a stripe a window's rank lacks.
	WriteRepair WriteKind = iota
	// WriteSecondChance writes an item again before its region is given back.
	WriteSecondChance
	// WriteFillRead keeps what a read of the store fetched.
	WriteFillRead
	// WriteFillPublication keeps what a publication uploaded, and what a pull
	// copies.
	WriteFillPublication
)

// DiskBudget is what the page cache's disk asks of the host's disk limiter:
// how many bytes it may hold, and whether it may write n bytes of a kind now.
// Its methods must be safe for concurrent use.
type DiskBudget interface {
	Share() int64
	Admit(n int64, kind WriteKind) bool
}

// fixedShare is the budget of a disk given a fixed number of bytes and no
// write budget: every write is admitted.
type fixedShare int64

func (s fixedShare) Share() int64              { return int64(s) }
func (fixedShare) Admit(int64, WriteKind) bool { return true }

// The probes the disk marks.
const (
	// ProbeDiskSecondChance is an item written again before its region went.
	ProbeDiskSecondChance = "checkpoint/disk-second-chance"
	// ProbeDiskSecondChanceBounded is a second chance stopped at half a region.
	ProbeDiskSecondChanceBounded = "checkpoint/disk-second-chance-bounded"
	// ProbeDiskFreeRegion is a second chance opening the region kept for it.
	ProbeDiskFreeRegion = "checkpoint/disk-free-region"
	// ProbeDiskEvictionWaitsForReader is an evicted region kept for a read in
	// flight.
	ProbeDiskEvictionWaitsForReader = "checkpoint/disk-eviction-waits-for-reader"
	// ProbeDiskKeyMismatch is a read that found another key's item.
	ProbeDiskKeyMismatch = "checkpoint/disk-key-mismatch"
	// ProbeDiskChecksumMismatch is a read that found a damaged item.
	ProbeDiskChecksumMismatch = "checkpoint/disk-checksum-mismatch"
)

// The fault-injection sites of the disk. Each is a fault the disk itself could
// cause, and the cache survives each with misses alone.
const (
	buggifyDiskFailedWrite    = "checkpoint/disk-failed-write"
	buggifyDiskShortWrite     = "checkpoint/disk-short-write"
	buggifyDiskFailedSync     = "checkpoint/disk-failed-sync"
	buggifyDiskTornTable      = "checkpoint/disk-torn-table"
	buggifyDiskFailedPunch    = "checkpoint/disk-failed-punch"
	buggifyDiskFailedAllocate = "checkpoint/disk-failed-allocate"
)

// diskRegion is one region of the log: its place in the file, what has been
// appended to it, the index entries that name it, and the reads in flight
// from it.
type diskRegion struct {
	slot, base int64
	sequence   uint64
	// written is the bytes appended from base, and tableBytes what the
	// table naming them will take. items is what the table will name; it is
	// kept only while the region is open.
	written, tableBytes int64
	items               []tableItem
	entries             []*windowEntry
	readers             int
	// evicted is a region the index no longer names, given back once
	// readers is zero; given is one given back.
	evicted, given bool
}

// fits reports whether an item of size bytes and its table entry fit in the
// rest of the region.
func (r *diskRegion) fits(size, table, regionBytes int64) bool {
	return r.written+size+r.tableBytes+table+diskTrailerSize <= regionBytes
}

// cacheDisk is the page cache's disk tier. Its methods are safe for concurrent
// use.
type cacheDisk struct {
	file        platform.File
	budget      DiskBudget
	regionBytes int64
	indexLimit  int64
	// threshold is the reads since it was written that give an item a second
	// chance.
	threshold int
	// deployment is the one the file must belong to, and entropy what a new
	// file's identity and generation are drawn from.
	deployment CacheDeployment
	entropy    platform.Entropy
	// clusterPercent is the share of windows placed by the list of caches.
	clusterPercent int
	// identity and generation are the file's, set as the disk opens.
	identity   CacheIdentity
	generation uint64
	// slots is the fetches every pull on this host has in flight together,
	// and writer the one writer the log has at a time. Both are channels, so
	// a goroutine waiting on either is durably blocked.
	slots  chan struct{}
	writer chan struct{}

	mu sync.Mutex
	// open is the region items are appended to, and closed the closed
	// regions the index still names, oldest first.
	open   *diskRegion
	closed []*diskRegion
	// held counts the regions on the disk, open, closed and evicted but not
	// yet given back, and pending the evicted ones among them. free is the
	// slots given back, and next the first slot never used.
	held, pending int
	free          []int64
	next          int64
	sequence      uint64
	index         diskIndex
	hits, lost    uint64
	evicted       uint64
	rewritten     uint64
	refused       uint64
	// fromTables, scanned and givenBackOnOpen count what the open did with
	// the regions it found.
	fromTables, scanned, givenBackOnOpen uint64
	// stopped is a disk whose cache has closed: its open region is closed, and
	// it takes no more writes.
	stopped bool
	// caches returns the list of caches the host holds, which says what the
	// disk keeps of each envelope and under which code it reads; nil is a
	// host that follows no list, and keeps each envelope whole.
	caches func() rank.List
}

// diskSettings is how a cache's disk is laid out and bounded, and what its
// file must say of itself.
type diskSettings struct {
	regionBytes, indexLimit int64
	// threshold is the reads since it was written that give an item a second
	// chance.
	threshold  int
	deployment CacheDeployment
	entropy    platform.Entropy
	// clusterPercent is the share of windows the disk places by the list of
	// caches; it keeps the rest whole.
	clusterPercent int
}

// openCacheDisk opens the page cache's disk over file: what the file holds is
// read back, or the file is made anew, and the disk is fitted to its share
// before it serves anything.
func openCacheDisk(ctx context.Context, file platform.File, budget DiskBudget, settings diskSettings) (*cacheDisk, error) {
	d := &cacheDisk{file: file, budget: budget, regionBytes: settings.regionBytes, indexLimit: settings.indexLimit,
		threshold: settings.threshold, deployment: settings.deployment, entropy: platform.EntropyOr(settings.entropy),
		clusterPercent: settings.clusterPercent,
		slots:          make(chan struct{}, pullConcurrency), writer: make(chan struct{}, 1), index: newDiskIndex()}
	if err := d.readBack(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// base is where the region in slot begins. The file's first region-sized span
// is its header's.
func (d *cacheDisk) base(slot int64) int64 { return (slot + 1) * d.regionBytes }

// share is the bytes the disk may hold now, in whole regions.
func (d *cacheDisk) share() int64 { return max(d.budget.Share(), 0) }

// capacity is the most a fill can keep on the disk at once: its share less the
// region kept free, before headers and tables.
func (d *cacheDisk) capacity() int64 {
	return max(d.share()/d.regionBytes-1, 0) * (d.regionBytes - diskTrailerSize)
}

// holds reports whether bytes of fills fit on the disk at once.
func (d *cacheDisk) holds(bytes int64) bool { return bytes <= d.capacity() }

// lockWriter takes the log's one writer.
func (d *cacheDisk) lockWriter(ctx context.Context) error {
	select {
	case d.writer <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (d *cacheDisk) unlockWriter() { <-d.writer }

// refuse counts and reports a write the disk will not take.
func (d *cacheDisk) refuse(format string, args ...any) error {
	d.mu.Lock()
	d.refused++
	d.mu.Unlock()
	return fmt.Errorf("%w: "+format, append([]any{ErrDiskRefused}, args...)...)
}

// append lays stripes of key's envelope, as one item each, next to each other
// at the end of the open region, in one write, and names them in the index,
// opening, closing and evicting regions to make room. It reports whether the
// items are on the disk. The caller holds the writer. A fill is of stripes
// the index does not hold. A second chance names a stripe a second time, in
// the open region, until its victim leaves the index a moment later.
func (d *cacheDisk) append(ctx context.Context, key diskKey, stripes []stripe.Stripe, kind WriteKind) (bool, error) {
	size, table := itemsBytes(key, stripes)
	region, err := d.room(ctx, size, table, kind)
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	offset := region.base + region.written
	region.written += size
	d.mu.Unlock()
	items := make([]byte, 0, size)
	for _, s := range stripes {
		items = appendItem(items, key, s)
	}
	if err := d.writeItems(ctx, items, offset); err != nil {
		// The space stays in the region unused, and no table names it.
		slog.WarnContext(ctx, "checkpoint: writing to the page cache's disk failed; the store serves the page",
			"checkpoint", key.Ref.String(), "volume", key.Volume, "page", key.Page, "segment", key.segment,
			"error", err)
		return false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range stripes {
		region.tableBytes += tableEntryBytes(key)
		region.items = append(region.items, tableItem{key: key, code: codeOf(s),
			offset: uint32(offset - region.base), length: uint32(len(s.Bytes))})
		d.index.insert(key, codeOf(s), region, offset, int64(len(s.Bytes)))
		offset += itemHeaderBytes(key) + int64(len(s.Bytes))
	}
	return true, nil
}

// itemsBytes is what stripes of key's envelope take in a region as items, and
// what they add to its table.
func itemsBytes(key diskKey, stripes []stripe.Stripe) (size, table int64) {
	for _, s := range stripes {
		size += itemHeaderBytes(key) + int64(len(s.Bytes))
		table += tableEntryBytes(key)
	}
	return size, table
}

// writeItems writes items where they go, in one write.
func (d *cacheDisk) writeItems(ctx context.Context, item []byte, offset int64) error {
	if sim.Buggify(ctx, buggifyDiskFailedWrite, 0.05) {
		return platform.ErrInjectedFault
	}
	if sim.Buggify(ctx, buggifyDiskShortWrite, 0.05) {
		if _, err := d.file.WriteAt(ctx, item[:len(item)/2], offset); err != nil {
			return err
		}
		return platform.ErrInjectedFault
	}
	_, err := d.file.WriteAt(ctx, item, offset)
	return err
}

// room returns the open region with room for an item of size bytes and a
// table entry of table, closing a full one, opening one, and evicting to make
// room. A second chance never evicts: it opens no more than the free region,
// and stops with errNoRoom where that is not enough.
func (d *cacheDisk) room(ctx context.Context, size, table int64, kind WriteKind) (*diskRegion, error) {
	for {
		d.mu.Lock()
		open := d.open
		if open != nil && open.fits(size, table, d.regionBytes) {
			d.mu.Unlock()
			return open, nil
		}
		d.mu.Unlock()
		if open != nil {
			d.close(ctx, open)
		}
		if d.mayOpen(ctx, kind) {
			if err := d.openRegion(ctx, kind); err != nil {
				return nil, err
			}
			continue
		}
		if kind == WriteSecondChance {
			return nil, errNoRoom
		}
		share := d.share()
		d.mu.Lock()
		var victim *diskRegion
		if len(d.closed) > 0 {
			victim = d.closed[0]
		}
		over := int64(d.held)*d.regionBytes > share
		d.mu.Unlock()
		if victim == nil {
			return nil, d.refuse("the share of %d bytes holds no region to give back", share)
		}
		d.evict(ctx, victim, !over)
	}
}

// mayOpen reports whether the share has room for one more region of a kind.
// Every region but one may be filled; the last is the second chance's.
func (d *cacheDisk) mayOpen(ctx context.Context, kind WriteKind) bool {
	limit := d.share() / d.regionBytes
	if kind != WriteSecondChance && !sim.Bug(ctx, "diskcache-no-free-region") {
		limit--
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return int64(d.held) < limit
}

// openRegion opens a region in the lowest slot free and allocates its space.
// A slot used before has its trailer cleared, so a table a punch that failed
// left there is never read back as the new region's.
func (d *cacheDisk) openRegion(ctx context.Context, kind WriteKind) error {
	regions := d.share() / d.regionBytes
	d.mu.Lock()
	var slot int64
	reused := len(d.free) > 0
	if reused {
		slot, d.free = d.free[0], d.free[1:]
	} else {
		slot = d.next
		d.next++
	}
	d.held++
	d.sequence++
	region := &diskRegion{slot: slot, base: d.base(slot), sequence: d.sequence}
	free := int64(d.held) == regions
	d.mu.Unlock()
	err := d.allocate(ctx, region)
	if err == nil && reused {
		_, err = d.file.WriteAt(ctx, make([]byte, diskTrailerSize), region.base+d.regionBytes-diskTrailerSize)
	}
	if err != nil {
		d.mu.Lock()
		d.held--
		d.free = insertSlot(d.free, slot)
		d.mu.Unlock()
		return d.refuse("opening a region failed: %v", err)
	}
	if kind == WriteSecondChance && free {
		sim.Probe(ctx, ProbeDiskFreeRegion)
	}
	d.mu.Lock()
	d.open = region
	d.mu.Unlock()
	return nil
}

// allocate reserves a region's space where the file can, so no write into it
// fails half way through for want of space.
func (d *cacheDisk) allocate(ctx context.Context, region *diskRegion) error {
	file, ok := d.file.(platform.AllocatingFile)
	if !ok {
		return nil
	}
	if sim.Buggify(ctx, buggifyDiskFailedAllocate, 0.25) {
		return platform.ErrInjectedFault
	}
	err := file.Allocate(ctx, region.base, d.regionBytes)
	if errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}

// insertSlot adds a slot to an ascending list of free slots.
func insertSlot(free []int64, slot int64) []int64 {
	at, _ := slices.BinarySearch(free, slot)
	return slices.Insert(free, at, slot)
}

// close ends a region's appends. Its items are synced, then its table is
// written at its end, then it is synced again: buffered writes reach the disk
// in any order, and the first sync keeps the table from ever naming an item
// that is not there. A sync that fails leaves the region without a table,
// which costs nothing until the cache is read back after a restart.
func (d *cacheDisk) close(ctx context.Context, region *diskRegion) {
	ctx = context.WithoutCancel(ctx)
	d.mu.Lock()
	if d.open == region {
		d.open = nil
	}
	items := region.items
	region.items = nil
	d.closed = append(d.closed, region)
	d.mu.Unlock()
	if !sim.Bug(ctx, "diskcache-table-before-sync") {
		if err := d.sync(ctx); err != nil {
			slog.WarnContext(ctx, "checkpoint: syncing a disk region failed; it is closed without a table",
				"region", region.sequence, "error", err)
			return
		}
	}
	table := encodeTable(region.sequence, d.generation, items)
	at := region.base + d.regionBytes - int64(len(table))
	written := table
	if sim.Buggify(ctx, buggifyDiskTornTable, 0.25) {
		// The table's second half, with its trailer, reaches the disk and
		// its first does not: a restart finds the table torn.
		written = table[len(table)/2:]
		at += int64(len(table) / 2)
	}
	if _, err := d.file.WriteAt(ctx, written, at); err != nil {
		slog.WarnContext(ctx, "checkpoint: writing a disk region's table failed", "region", region.sequence,
			"error", err)
		return
	}
	if err := d.sync(ctx); err != nil {
		slog.WarnContext(ctx, "checkpoint: syncing a disk region's table failed", "region", region.sequence,
			"error", err)
	}
}

func (d *cacheDisk) sync(ctx context.Context) error {
	if sim.Buggify(ctx, buggifyDiskFailedSync, 0.25) {
		return platform.ErrInjectedFault
	}
	return d.file.Sync(ctx)
}

// rescue is one item of a victim region a second chance writes again.
type rescue struct {
	location diskLocation
	page     uint16
	code     diskCode
}

// evict gives the oldest closed region back. With secondChance, the items in
// it read at least the threshold since they were written are first written
// again into the open region, in the order they lie, up to half a region; the
// rest go. A region's entries are in the order their first items lie, and an
// entry's items lie next to each other, so visiting them in turn visits the
// items in order. The caller holds the writer.
func (d *cacheDisk) evict(ctx context.Context, victim *diskRegion, secondChance bool) {
	ctx = context.WithoutCancel(ctx)
	d.mu.Lock()
	var rescues []rescue
	if secondChance {
		for _, entry := range victim.entries {
			entry.each(func(page uint16, index uint8, location diskLocation) {
				if location.word.reads() >= d.threshold {
					rescues = append(rescues, rescue{location: location, page: page,
						code: diskCode{stripe: index, k: entry.k, m: entry.m}})
				}
			})
		}
		victim.readers++
	}
	d.mu.Unlock()
	if secondChance {
		d.secondChance(ctx, victim, rescues)
		d.finishRead(ctx, victim)
	}
	d.mu.Lock()
	d.closed = slices.DeleteFunc(d.closed, func(region *diskRegion) bool { return region == victim })
	d.index.dropRegion(victim)
	victim.evicted = true
	d.pending++
	gone := victim.readers == 0
	if !gone {
		sim.Probe(ctx, ProbeDiskEvictionWaitsForReader)
	}
	d.mu.Unlock()
	if gone || sim.Bug(ctx, "diskcache-evict-under-reader") {
		d.giveBack(ctx, victim)
	}
}

// secondChance writes rescues again into the open region, stopping at half a
// region, at the first refusal of the write budget, or where the free region
// is not enough.
func (d *cacheDisk) secondChance(ctx context.Context, victim *diskRegion, rescues []rescue) {
	var written int64
	for _, item := range rescues {
		size := item.location.size()
		if written+size > d.regionBytes/2 && !sim.Bug(ctx, "diskcache-unbounded-second-chance") {
			sim.Probe(ctx, ProbeDiskSecondChanceBounded)
			return
		}
		if !d.budget.Admit(size, WriteSecondChance) {
			return
		}
		buffer := make([]byte, size)
		if err := readFull(ctx, d.file, buffer, item.location.offset); err != nil {
			continue
		}
		parsed, err := parseItem(buffer, false)
		if err != nil {
			continue
		}
		if hash, page := parsed.key.window(); hash != item.location.entry.hash || page != item.page ||
			parsed.code != item.code {
			continue
		}
		stored, err := d.append(ctx, parsed.key, []stripe.Stripe{parsed.stripe()}, WriteSecondChance)
		if err != nil {
			return
		}
		written += size
		if !stored {
			continue
		}
		sim.Probe(ctx, ProbeDiskSecondChance)
		d.mu.Lock()
		d.rewritten++
		d.mu.Unlock()
	}
}

// giveBack returns an evicted region's space, once. Punching it out returns
// its blocks to the node's filesystem; a punch that fails costs the
// filesystem the blocks until the slot is used again, and nothing else.
func (d *cacheDisk) giveBack(ctx context.Context, region *diskRegion) {
	d.mu.Lock()
	if region.given {
		d.mu.Unlock()
		return
	}
	region.given = true
	d.mu.Unlock()
	if err := d.punch(ctx, region); err != nil {
		slog.WarnContext(ctx, "checkpoint: punching out a disk region failed", "region", region.sequence,
			"error", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held--
	d.pending--
	d.evicted++
	d.free = insertSlot(d.free, region.slot)
}

func (d *cacheDisk) punch(ctx context.Context, region *diskRegion) error {
	file, ok := d.file.(platform.SparseFile)
	if !ok {
		return nil
	}
	if sim.Buggify(ctx, buggifyDiskFailedPunch, 0.25) {
		return platform.ErrInjectedFault
	}
	return file.PunchHole(ctx, region.base, d.regionBytes)
}

// fit gives regions back, oldest first and with no second chance, until the
// disk holds no more than its share less one region, so a share that wavers
// does not evict and refill a region at a time. It closes the open region if
// that is what is left.
func (d *cacheDisk) fit(ctx context.Context) error {
	if err := d.lockWriter(ctx); err != nil {
		return err
	}
	defer d.unlockWriter()
	for {
		share := d.share()
		d.mu.Lock()
		kept := int64(d.held-d.pending) * d.regionBytes
		var victim *diskRegion
		if len(d.closed) > 0 {
			victim = d.closed[0]
		}
		open := d.open
		d.mu.Unlock()
		if kept <= share-d.regionBytes || victim == nil && open == nil {
			return nil
		}
		if victim == nil {
			d.close(ctx, open)
			continue
		}
		d.evict(ctx, victim, false)
	}
}

// shutdown closes the open region, so that a restart reads its items back from
// its table rather than giving it back, and refuses every write after it.
func (d *cacheDisk) shutdown(ctx context.Context) {
	if err := d.lockWriter(ctx); err != nil {
		slog.WarnContext(ctx, "checkpoint: the page cache's disk was left with its region open", "error", err)
		return
	}
	defer d.unlockWriter()
	d.mu.Lock()
	open := d.open
	d.stopped = true
	d.mu.Unlock()
	if open != nil {
		d.close(ctx, open)
	}
}

// diskReadOutcome is what one read of the disk found.
type diskReadOutcome int

const (
	diskHit diskReadOutcome = iota
	// diskAbsent is a key the index does not hold.
	diskAbsent
	// diskFailed is a read the disk did not complete.
	diskFailed
	// diskKeyMismatch is an item that names another key.
	diskKeyMismatch
	// diskDamaged is an item that fails its checksum.
	diskDamaged
	// diskWrongStripe is stripes that pass their checks and rebuild no
	// envelope that passes its own.
	diskWrongStripe
)

// readItem reads back the item location names, which the index holds as the
// stripe of key's envelope that code says, and with the region it lies in
// held for the read. Anything but a hit is a miss, and an item the disk could
// not give back intact is forgotten: the store still holds what was copied,
// and a copy that failed once is not asked for again.
func (d *cacheDisk) readItem(ctx context.Context, key diskKey, code diskCode,
	location diskLocation) (stripe.Stripe, diskReadOutcome) {
	buffer := make([]byte, location.size())
	err := readFull(ctx, d.file, buffer, location.offset)
	d.finishRead(ctx, location.entry.region)
	if err != nil {
		// A read the caller gave up on says nothing about the disk.
		if context.Cause(ctx) == nil {
			d.forget(ctx, location, key, err)
		}
		return stripe.Stripe{}, diskFailed
	}
	found, ok := itemKey(buffer)
	if ok && !sim.Bug(ctx, "diskcache-skip-key-check") {
		if named := (diskCode{stripe: buffer[6], k: buffer[7], m: buffer[8]}); found != key || named != code {
			sim.Probe(ctx, ProbeDiskKeyMismatch)
			d.forget(ctx, location, key, fmt.Errorf("%w: stripe %d of %d+%d of %s/%s/%d", errItemKey,
				named.stripe, named.k, named.m, found.Ref, found.Volume, found.Page))
			return stripe.Stripe{}, diskKeyMismatch
		}
	}
	parsed, err := parseItem(buffer, sim.Bug(ctx, "diskcache-skip-checksum"))
	if err != nil {
		sim.Probe(ctx, ProbeDiskChecksumMismatch)
		d.forget(ctx, location, key, err)
		return stripe.Stripe{}, diskDamaged
	}
	read := parsed.stripe()
	if len(read.Bytes) > 0 && sim.Buggify(ctx, buggifyDiskWrongStripe, 0.05) {
		// A stripe whose own checksum holds and whose bytes are not the
		// envelope's, as a peer that answers with a wrong stripe gives one.
		read.Bytes[len(read.Bytes)/2] ^= 0x40
	}
	return read, diskHit
}

// forget drops one item from the index.
func (d *cacheDisk) forget(ctx context.Context, location diskLocation, key diskKey, cause error) {
	slog.WarnContext(ctx, "checkpoint: the page cache's disk lost a copy; reading the object store instead",
		"checkpoint", key.Ref.String(), "volume", key.Volume, "page", key.Page, "segment", key.segment,
		"error", cause)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lost++
	d.index.forget(location)
}

// finishRead ends one read of a region, giving the region back if it was
// evicted and this was its last reader.
func (d *cacheDisk) finishRead(ctx context.Context, region *diskRegion) {
	d.mu.Lock()
	region.readers--
	gone := region.evicted && region.readers == 0
	d.mu.Unlock()
	if gone {
		d.giveBack(context.WithoutCancel(ctx), region)
	}
}

// forgetAll drops every entry of the index. It is the in-tree bug that has a
// pull free what it copied when it closes.
func (d *cacheDisk) forgetAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.index.clear()
}

// acquire takes one of the fetch slots every pull on this host shares.
func (d *cacheDisk) acquire(ctx context.Context) error {
	select {
	case d.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (d *cacheDisk) releaseSlot() { <-d.slots }

// DiskStats is what the page cache's disk holds and has served.
type DiskStats struct {
	// UsedBytes is the space its regions hold, open, closed and waiting for a
	// reader, and LimitBytes its share.
	UsedBytes, LimitBytes int64
	// Regions is how many regions it holds.
	Regions int
	// Entries is the stripes of pages and segments it holds, a whole
	// envelope being one, and IndexBytes what its index costs in memory.
	Entries    int
	IndexBytes int64
	// Hits counts reads it served, and Lost the copies it could not give back
	// intact, which were read from the object store instead.
	Hits, Lost uint64
	// Evicted counts regions given back, Rewritten the items a second chance
	// wrote again, and Refused the writes it refused.
	Evicted, Rewritten, Refused uint64
	// Identity names the disk's file, which keeps it across restarts.
	Identity CacheIdentity
	// FromTables, Scanned and GivenBackOnOpen are what the disk did with the
	// regions it found when it opened: read back from their tables, read back
	// by scanning their items, and given back.
	FromTables, Scanned, GivenBackOnOpen uint64
}

func (d *cacheDisk) stats() DiskStats {
	share := d.share()
	d.mu.Lock()
	defer d.mu.Unlock()
	return DiskStats{UsedBytes: int64(d.held) * d.regionBytes, LimitBytes: share, Regions: d.held,
		Entries: d.index.live, IndexBytes: d.index.used, Hits: d.hits, Lost: d.lost, Evicted: d.evicted,
		Rewritten: d.rewritten, Refused: d.refused, Identity: d.identity, FromTables: d.fromTables,
		Scanned: d.scanned, GivenBackOnOpen: d.givenBackOnOpen}
}
