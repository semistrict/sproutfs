package checkpoint

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/semistrict/sproutfs/platform"
)

// The page cache's disk is its second tier: a copy of the pages a pull fetched,
// on the host's own disk, keyed by page identity exactly as the memory tier is.
// It holds what the object store holds — each member's and each segment's
// encoded envelope, byte for byte — so a read from it is the same read as one
// from the store, checked by the same envelope, and a copy the disk lost or
// damaged fails that check and is read from the store instead.
//
// Nothing on it is durable. It is truncated when the host starts, it is never
// evidence that a publication landed, and nothing publishes from it: a page
// reaches the store only through a checkpoint, which reads the pager. A newer
// checkpoint's page has a new identity, so the copy of the page it replaced is
// never read for it.
//
// Space is handed out a pull at a time. A pull takes one region, sized from
// what its checkpoint's root records it holds, before it fetches anything: so
// a VM either fits whole or is refused before it starts, and a refused VM reads
// the store as it would without a pull. A page another pull already copied is
// shared rather than copied again, and a region stays while any pull holds a
// page in it.

// ErrNoDisk refuses a pull on a host whose page cache keeps nothing on disk.
var ErrNoDisk = errors.New("checkpoint: the page cache keeps no disk")

// ErrDiskFull refuses a pull whose checkpoint does not fit in what the page
// cache's disk has left.
var ErrDiskFull = errors.New("checkpoint: the page cache's disk is full")

// diskBlock is the unit the disk is handed out in. A region is rounded up to it
// once, not per page, so it costs a pull at most one block; it is the
// filesystem's own block, so a region given back is punched out whole.
const diskBlock = 4 << 10

// pullConcurrency is how many fetches every pull on a host has in flight
// together. A pull is background work: it takes few of the store's requests,
// and none of the page cache's load slots, so a fault never queues behind it.
const pullConcurrency = 2

// blockRun is a run of consecutive blocks of the disk.
type blockRun struct {
	first, count int64
}

// diskRegion is the space one pull took: runs of blocks, filled from the start
// in the order the pull fetched its members. What the pull has written is a
// prefix of it; the rest is given back when the pull ends.
//
// holders counts the pulls holding a page in it, its own among them, and
// readers the reads in flight from it. keys are the entries that name it. A
// region no pull holds loses its entries at once and gives its blocks back
// once its last reader has finished.
type diskRegion struct {
	runs    []blockRun
	written int64
	holders int
	readers int
	keys    []cacheKey
	dropped bool
}

// capacity is how many bytes the region's blocks hold.
func (r *diskRegion) capacity() int64 {
	var blocks int64
	for _, run := range r.runs {
		blocks += run.count
	}
	return blocks * diskBlock
}

// diskFragment is one contiguous stretch of the file.
type diskFragment struct {
	offset, length int64
}

// fragments maps a range of the region's bytes onto the file.
func (r *diskRegion) fragments(offset, length int64) []diskFragment {
	var found []diskFragment
	for _, run := range r.runs {
		size := run.count * diskBlock
		if offset >= size {
			offset -= size
			continue
		}
		take := min(length, size-offset)
		found = append(found, diskFragment{offset: run.first*diskBlock + offset, length: take})
		length -= take
		offset = 0
		if length == 0 {
			break
		}
	}
	return found
}

// diskEntry is where one object's bytes lie within a region.
type diskEntry struct {
	region         *diskRegion
	offset, length int64
}

// cacheDisk is the page cache's disk tier. Its methods are safe for concurrent
// use.
type cacheDisk struct {
	file platform.File
	// blocks is the configured cap, in blocks.
	blocks int64
	// slots is the fetches every pull on this host has in flight together.
	slots chan struct{}

	mu      sync.Mutex
	free    []blockRun // ascending and coalesced
	used    int64      // blocks the regions hold, until they are given back
	entries map[cacheKey]diskEntry
	hits    uint64
	lost    uint64
}

func newCacheDisk(file platform.File, bytes int64) *cacheDisk {
	blocks := bytes / diskBlock
	return &cacheDisk{file: file, blocks: blocks, slots: make(chan struct{}, pullConcurrency),
		free: []blockRun{{first: 0, count: blocks}}, entries: make(map[cacheKey]diskEntry)}
}

// reserve takes a region of at least bytes, or refuses it whole.
func (d *cacheDisk) reserve(bytes int64) (*diskRegion, error) {
	blocks := (bytes + diskBlock - 1) / diskBlock
	d.mu.Lock()
	defer d.mu.Unlock()
	if blocks > d.blocks-d.used {
		return nil, ErrDiskFull
	}
	region := &diskRegion{holders: 1}
	for need := blocks; need > 0; {
		run := &d.free[0]
		take := min(need, run.count)
		region.runs = append(region.runs, blockRun{first: run.first, count: take})
		run.first += take
		run.count -= take
		need -= take
		if run.count == 0 {
			d.free = d.free[1:]
		}
	}
	d.used += blocks
	return region, nil
}

// trim gives back the blocks of a region its pull did not write into, which is
// what it took for pages another pull already held, and everything past where
// a pull that stopped early got to.
func (d *cacheDisk) trim(ctx context.Context, region *diskRegion) {
	d.mu.Lock()
	keep := (region.written + diskBlock - 1) / diskBlock
	var kept, spare []blockRun
	for _, run := range region.runs {
		switch {
		case keep >= run.count:
			kept = append(kept, run)
			keep -= run.count
		case keep > 0:
			kept = append(kept, blockRun{first: run.first, count: keep})
			spare = append(spare, blockRun{first: run.first + keep, count: run.count - keep})
			keep = 0
		default:
			spare = append(spare, run)
		}
	}
	region.runs = kept
	d.mu.Unlock()
	d.giveBack(ctx, spare)
}

// giveBack returns blocks nothing names any more. Punching them out is a
// courtesy to the node's filesystem, not accounting: the cap is the cap
// whether or not the blocks are backed, so a failed or unsupported punch costs
// nothing.
func (d *cacheDisk) giveBack(ctx context.Context, runs []blockRun) {
	if len(runs) == 0 {
		return
	}
	if file, ok := d.file.(platform.SparseFile); ok {
		for _, run := range runs {
			if err := file.PunchHole(ctx, run.first*diskBlock, run.count*diskBlock); err != nil {
				slog.DebugContext(ctx, "checkpoint: punching out the page cache's disk blocks failed",
					"offset", run.first*diskBlock, "bytes", run.count*diskBlock, "error", err)
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, run := range runs {
		d.used -= run.count
		d.free = insertRun(d.free, run)
	}
}

// insertRun adds a run to an ascending, coalesced list of free runs.
func insertRun(free []blockRun, run blockRun) []blockRun {
	at := 0
	for at < len(free) && free[at].first < run.first {
		at++
	}
	free = append(free, blockRun{})
	copy(free[at+1:], free[at:])
	free[at] = run
	merged := free[:0]
	for _, next := range free {
		if n := len(merged); n > 0 && merged[n-1].first+merged[n-1].count == next.first {
			merged[n-1].count += next.count
			continue
		}
		merged = append(merged, next)
	}
	return merged
}

// hold makes a pull a holder of the region an entry lies in, and reports
// whether the disk has the entry at all.
func (d *cacheDisk) hold(held map[*diskRegion]bool, key cacheKey) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, found := d.entries[key]
	if found {
		holdLocked(held, entry.region)
	}
	return found
}

// holdLocked makes a pull a holder of one region, once. The caller holds mu.
func holdLocked(held map[*diskRegion]bool, region *diskRegion) {
	if !held[region] {
		held[region] = true
		region.holders++
	}
}

// write lays one object's bytes into a pull's region after what it has already
// written, and names them by key. A key another pull named first while these
// bytes were being written keeps that pull's copy, which this pull then holds;
// the bytes written here are then slack until the region goes.
func (d *cacheDisk) write(ctx context.Context, region *diskRegion, held map[*diskRegion]bool,
	key cacheKey, data []byte) error {
	d.mu.Lock()
	offset := region.written
	if offset+int64(len(data)) > region.capacity() {
		d.mu.Unlock()
		// The region was sized from what the root records its checkpoint
		// holds, so a member that overruns it is one the root misstated.
		return ErrCorrupt
	}
	fragments := region.fragments(offset, int64(len(data)))
	d.mu.Unlock()
	rest := data
	for _, fragment := range fragments {
		if _, err := d.file.WriteAt(ctx, rest[:fragment.length], fragment.offset); err != nil {
			return err
		}
		rest = rest[fragment.length:]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	region.written += int64(len(data))
	if entry, found := d.entries[key]; found {
		holdLocked(held, entry.region)
		return nil
	}
	d.entries[key] = diskEntry{region: region, offset: offset, length: int64(len(data))}
	region.keys = append(region.keys, key)
	return nil
}

// read returns the bytes the disk holds under key. A read that fails reports
// nothing found, and the entry is forgotten: the store still holds what it
// copied, and a disk that cannot give it back is one to stop asking.
func (d *cacheDisk) read(ctx context.Context, key cacheKey) ([]byte, bool) {
	d.mu.Lock()
	entry, found := d.entries[key]
	if !found {
		d.mu.Unlock()
		return nil, false
	}
	entry.region.readers++
	fragments := entry.region.fragments(entry.offset, entry.length)
	d.mu.Unlock()
	data := make([]byte, entry.length)
	var err error
	rest := data
	for _, fragment := range fragments {
		if _, err = d.file.ReadAt(ctx, rest[:fragment.length], fragment.offset); err != nil {
			break
		}
		rest = rest[fragment.length:]
	}
	d.finishRead(ctx, entry.region)
	if err != nil {
		// A read the caller gave up on says nothing about the disk.
		if context.Cause(ctx) == nil {
			d.lose(ctx, key, err)
		}
		return nil, false
	}
	return data, true
}

// served counts one read of a copy that came back intact.
func (d *cacheDisk) served() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hits++
}

// lose forgets an entry whose bytes the disk could not give back intact.
func (d *cacheDisk) lose(ctx context.Context, key cacheKey, cause error) {
	slog.WarnContext(ctx, "checkpoint: the page cache's disk lost a copy; reading the object store instead",
		"checkpoint", key.Ref.String(), "volume", key.Volume, "page", key.Page, "segment", key.segment,
		"error", cause)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lost++
	delete(d.entries, key)
}

// finishRead ends one read of a region, giving the region's blocks back if it
// was the last thing keeping a region no pull holds.
func (d *cacheDisk) finishRead(ctx context.Context, region *diskRegion) {
	d.mu.Lock()
	region.readers--
	gone := region.dropped && region.readers == 0
	d.mu.Unlock()
	if gone {
		d.giveBack(ctx, region.runs)
	}
}

// release ends a pull's hold on every region it held. A region no pull holds
// any more loses its entries at once, so nothing new reads it, and gives its
// blocks back once the reads already in flight have finished.
func (d *cacheDisk) release(ctx context.Context, held map[*diskRegion]bool) {
	var gone [][]blockRun
	d.mu.Lock()
	for region := range held {
		region.holders--
		if region.holders > 0 {
			continue
		}
		for _, key := range region.keys {
			if d.entries[key].region == region {
				delete(d.entries, key)
			}
		}
		region.keys = nil
		region.dropped = true
		if region.readers == 0 {
			gone = append(gone, region.runs)
		}
	}
	clear(held)
	d.mu.Unlock()
	for _, runs := range gone {
		d.giveBack(ctx, runs)
	}
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
	// UsedBytes is the space the pulls on this host hold, and LimitBytes the
	// cap it is taken from.
	UsedBytes, LimitBytes int64
	// Entries is the pages and segments it holds.
	Entries int
	// Hits counts reads it served, and Lost the copies it could not give back
	// intact, which were read from the object store instead.
	Hits, Lost uint64
}

func (d *cacheDisk) stats() DiskStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DiskStats{UsedBytes: d.used * diskBlock, LimitBytes: d.blocks * diskBlock,
		Entries: len(d.entries), Hits: d.hits, Lost: d.lost}
}
