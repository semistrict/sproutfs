package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// Pull is a copy of every page one checkpoint holds, and of the segments that
// locate them, on the host's own disk. It is what a VM marked to pull its whole
// memory holds while it runs: once the copy is complete, a read of any page of
// that checkpoint that the page cache's memory does not hold is served from the
// disk and makes no request of the object store.
//
// The copy is fetched in the background, behind every fault. It takes none of
// the cache's load slots and joins none of a fault's fetches, so a fault for a
// page the pull has not reached is read from the store at once, as it would be
// without a pull. Before each fetch the pull waits until no load of the cache is
// in flight, and every pull on the host shares pullConcurrency fetches.
//
// Close gives the copy up. A page another pull on the host also holds stays
// for that pull.
type Pull struct {
	store *Store
	disk  *cacheDisk
	index *Index
	// region is the space the pull took, and held every region it holds a page
	// in, its own among them. held is guarded by the disk's lock.
	region *diskRegion
	held   map[*diskRegion]bool
	// bytes is what the checkpoint holds, as its root records it, and pulled
	// how much of it is on the disk so far.
	bytes  int64
	pulled atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
	err    error
	close  sync.Once
}

// PullStats is how far one pull has come.
type PullStats struct {
	// Bytes is what the checkpoint holds and Pulled how much of it is on the
	// disk: the members and segments this pull copied and the ones it found
	// another pull had already copied.
	Bytes, Pulled int64
	// Done reports a pull that has stopped fetching: complete when Err is nil,
	// and stopped short by Err otherwise. A pull that stopped short keeps what
	// it copied, and the store serves the rest.
	Done bool
	Err  error
}

// Pull begins copying every page of index onto the page cache's disk and
// returns at once. It refuses, with ErrNoDisk or ErrDiskFull, a host that
// keeps nothing on disk or a checkpoint that does not fit in what the disk has
// left; nothing is copied then, and reads go to the store as they always do.
func (s *Store) Pull(ctx context.Context, index *Index) (*Pull, error) {
	if s.cache == nil || s.cache.disk == nil {
		return nil, ErrNoDisk
	}
	bytes := index.heldBytes()
	region, err := s.cache.disk.reserve(bytes)
	if err != nil {
		stats := s.cache.disk.stats()
		return nil, fmt.Errorf("%w: %s holds %d bytes and %d of %d are free", err, index.Ref(), bytes,
			stats.LimitBytes-stats.UsedBytes, stats.LimitBytes)
	}
	running, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Pull{store: s, disk: s.cache.disk, index: index, region: region,
		held: map[*diskRegion]bool{region: true}, bytes: bytes, cancel: cancel, done: make(chan struct{})}
	go p.run(running)
	return p, nil
}

// heldBytes is what the checkpoint holds in the store, as its root records it:
// every member its segments locate and every segment that locates them.
func (i *Index) heldBytes() int64 {
	var bytes uint64
	for _, table := range i.volumes {
		for _, entry := range table.segments {
			bytes += entry.at.length
			for _, use := range entry.reads {
				bytes += use.bytes
			}
		}
	}
	return int64(bytes)
}

// Wait returns once the pull has stopped fetching, with what stopped it short.
func (p *Pull) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Stats reports how far the pull has come.
func (p *Pull) Stats() PullStats {
	stats := PullStats{Bytes: p.bytes, Pulled: p.pulled.Load()}
	select {
	case <-p.done:
		stats.Done, stats.Err = true, p.err
	default:
	}
	return stats
}

// Close stops the pull and gives its copy up. Calling it again does nothing.
func (p *Pull) Close() {
	p.close.Do(func() {
		p.cancel()
		<-p.done
		p.disk.release(context.Background(), p.held)
	})
}

// run copies the checkpoint one segment at a time, in volume and segment
// order, and gives back whatever of its region it did not write.
func (p *Pull) run(ctx context.Context) {
	defer close(p.done)
	defer p.disk.trim(context.WithoutCancel(ctx), p.region)
	for _, name := range p.index.names {
		table := p.index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(table.segments)) {
			if err := p.segment(ctx, name, number, table.segments[number]); err != nil {
				if context.Cause(ctx) == nil {
					slog.WarnContext(ctx, "checkpoint: a pull stopped short; the store serves what it did not reach",
						"checkpoint", p.index.Ref().String(), "volume", name, "segment", number, "error", err)
				}
				p.err = err
				return
			}
		}
	}
}

// segment copies one segment and every member it locates that the disk does not
// already hold.
func (p *Pull) segment(ctx context.Context, volume string, number uint64, entry segmentEntry) error {
	key := segmentCacheKey(volume, number, entry.at.ref)
	encoded, err := p.segmentBytes(ctx, key, entry.at)
	if err != nil {
		return err
	}
	data, err := p.store.codecs.Decode(ctx, encoded, maximumSegmentSize)
	if err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	located, err := p.index.decodeSegment(volume, data)
	if err != nil {
		return err
	}
	geometry := p.index.volumes[volume].geometry
	first := number * geometry.SegmentPages
	var run []pageRead
	var keys []cacheKey
	for _, relative := range slices.Sorted(maps.Keys(located.pages)) {
		at := located.pages[relative]
		page := pageKey(identityOf(volume, first+uint64(relative), at))
		if p.hold(page) {
			p.pulled.Add(int64(at.length))
			continue
		}
		run = append(run, pageRead{number: first + uint64(relative), at: at})
		keys = append(keys, page)
	}
	wanted := make([]int, len(run))
	for at := range wanted {
		wanted[at] = at
	}
	for _, extent := range groupMembers(run, wanted) {
		held, err := p.fetch(ctx, func(ctx context.Context) ([]byte, error) {
			key, err := p.store.partKey(extent.ref, extent.part)
			if err != nil {
				return nil, err
			}
			return p.store.readRange(ctx, key, extent.offset, extent.length, maximumReadExtent)
		})
		if err != nil {
			return err
		}
		for _, at := range extent.members {
			member := run[at].at
			if err := p.write(ctx, keys[at], held[member.offset-extent.offset:][:member.length]); err != nil {
				return err
			}
		}
	}
	return nil
}

// segmentBytes is one segment's envelope: the disk's copy where another pull
// already made one, and the store's otherwise, which this pull then copies.
func (p *Pull) segmentBytes(ctx context.Context, key cacheKey, at segmentAddress) ([]byte, error) {
	if p.hold(key) {
		if encoded, found := p.disk.read(ctx, key); found {
			p.pulled.Add(int64(at.length))
			return encoded, nil
		}
	}
	encoded, err := p.fetch(ctx, func(ctx context.Context) ([]byte, error) { return p.store.readSegment(ctx, at) })
	if err != nil {
		return nil, err
	}
	return encoded, p.write(ctx, key, encoded)
}

// fetch runs one request of the store behind every fault: once no load of the
// cache is in flight, and within the fetches every pull on the host shares.
func (p *Pull) fetch(ctx context.Context, read func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := p.store.cache.quiet(ctx); err != nil {
		return nil, err
	}
	if err := p.disk.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.disk.releaseSlot()
	return read(ctx)
}

// hold makes this pull a holder of a copy another pull made, and reports
// whether there is one.
func (p *Pull) hold(key cacheKey) bool { return p.disk.hold(p.held, key) }

// write copies one envelope into this pull's region.
func (p *Pull) write(ctx context.Context, key cacheKey, data []byte) error {
	if err := p.disk.write(ctx, p.region, p.held, key, data); err != nil {
		return err
	}
	p.pulled.Add(int64(len(data)))
	return nil
}
