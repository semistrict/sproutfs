package checkpoint

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/platform/sim"
)

// Pull copies every page one checkpoint holds, and the segments that locate
// them, onto the host's own disk. It is what a VM marked to pull its whole
// memory starts while it runs: a read of a page of that checkpoint that the
// page cache's memory does not hold is served from the disk while the disk
// holds it, and makes no request of the object store.
//
// The copy is fetched in the background, behind every fault. It takes none of
// the cache's load slots and joins none of a fault's fetches, so a fault for a
// page the pull has not reached is read from the store at once, as it would be
// without a pull. Before each fetch the pull waits until no load of the cache is
// in flight, and every pull on the host shares pullConcurrency fetches.
//
// What a pull copies is ordinary disk entries. A pull holds none of them, and
// closing it frees nothing: they leave the disk only as its regions are given
// back under pressure, like everything else on it. A page the disk already
// holds, from another pull or anything else, is not copied again.
//
// A pull also keeps what its VM publishes later. A publication given the pull
// (Publication.Keep) writes the members and segments it uploads to the disk as
// each lands, so a page the VM wrote after the checkpoint it started from, once
// published and then evicted, is read from this host's disk too rather than
// from the store. A write the disk refuses keeps nothing, and the store serves
// those pages.
type Pull struct {
	store *Store
	disk  *cacheDisk
	index *Index
	// bytes is what the checkpoint holds, as its root records it, and pulled
	// how much of it this pull found on the disk, wrote there, or handed to
	// the cluster's fills.
	bytes  int64
	pulled atomic.Int64
	// kept is what later publications added to the disk, or filled the
	// cluster with.
	kept atomic.Int64
	// base is the context the pull began under, without its cancellation.
	base context.Context

	// mu orders keep against Close, so nothing is kept for a pull given up.
	mu     sync.Mutex
	closed bool

	cancel context.CancelFunc
	done   chan struct{}
	err    error
	close  sync.Once
}

// PullStats is how far one pull has come.
type PullStats struct {
	// Bytes is what the checkpoint holds and Pulled how much of it the pull
	// found on the disk or wrote there, or, for a window inside the cluster
	// share, handed to the cluster's fills, which may drop it. Kept is what
	// the VM's later publications added. The disk gives its oldest regions
	// back under pressure, so these say what the pull did, not what the disk
	// still holds.
	Bytes, Pulled, Kept int64
	// Done reports a pull that has stopped fetching: complete when Err is nil,
	// and stopped short by Err otherwise. A pull that stopped short keeps what
	// it copied, and the store serves the rest.
	Done bool
	Err  error
}

// Pull begins copying every page of index onto the page cache's disk and
// returns at once. It refuses, with ErrNoDisk or ErrDiskFull, a host that
// keeps no disk or a checkpoint larger than all the disk's share could hold;
// nothing is fetched then, and reads go to the store as they always do.
func (s *Store) Pull(ctx context.Context, index *Index) (*Pull, error) {
	if s.cache == nil || s.cache.disk == nil {
		return nil, ErrNoDisk
	}
	bytes := index.heldBytes()
	if !s.cache.disk.holds(bytes) {
		return nil, fmt.Errorf("%w: %s holds %d bytes and the disk's share keeps at most %d", ErrDiskFull,
			index.Ref(), bytes, s.cache.disk.capacity())
	}
	base := context.WithoutCancel(ctx)
	running, cancel := context.WithCancel(base)
	p := &Pull{store: s, disk: s.cache.disk, index: index, bytes: bytes, base: base, cancel: cancel,
		done: make(chan struct{})}
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
	stats := PullStats{Bytes: p.bytes, Pulled: p.pulled.Load(), Kept: p.kept.Load()}
	select {
	case <-p.done:
		stats.Done, stats.Err = true, p.err
	default:
	}
	return stats
}

// StopFetching stops copying the checkpoint and waits for the copy in flight,
// and leaves the keeping on: a publication of the VM after it still keeps
// its pages. It is what a VM that stops running here calls before its last
// publication, which Close then follows. Calling it again does nothing.
func (p *Pull) StopFetching() {
	p.cancel()
	<-p.done
}

// Close stops the pull and its keeping. What it copied stays on the disk.
// Calling it again does nothing.
func (p *Pull) Close() {
	p.close.Do(func() {
		p.cancel()
		<-p.done
		p.mu.Lock()
		defer p.mu.Unlock()
		p.closed = true
		if sim.Bug(p.base, "diskcache-pull-frees-on-close") {
			p.disk.forgetAll()
		}
	})
}

// envelope is one object's bytes as the store holds them, and the key the disk
// names them by.
type envelope struct {
	key  diskKey
	data []byte
}

// keep writes envelopes a publication of this pull's VM has just uploaded to
// the disk, skipping any the disk already holds. It is best effort: a write
// the disk refuses leaves the store serving the rest, and the publication goes
// on regardless. A window inside the cluster share is the publication's own
// fill, which puts it on its ranks, this host's among them.
func (p *Pull) keep(ctx context.Context, envelopes []envelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	for _, e := range envelopes {
		if p.store.cache.filler.inShare(e.key) {
			p.kept.Add(int64(len(e.data)))
			continue
		}
		if p.disk.has(ctx, e.key) {
			continue
		}
		if err := p.disk.write(ctx, e.key, e.data, WriteFillPublication); err != nil {
			slog.WarnContext(ctx, "checkpoint: the disk keeps no more of a pulled VM's publication; the store serves it",
				"checkpoint", p.index.Ref().String(), "error", err)
			return
		}
		if p.disk.has(ctx, e.key) {
			p.kept.Add(int64(len(e.data)))
		}
	}
}

// run copies the checkpoint one segment at a time, in volume and segment
// order.
func (p *Pull) run(ctx context.Context) {
	defer close(p.done)
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
	key := segmentDiskKey(volume, number, entry.at.ref)
	data, err := p.segmentBytes(ctx, key, entry.at)
	if err != nil {
		return err
	}
	located, err := p.index.decodeSegment(volume, data)
	if err != nil {
		return err
	}
	geometry := p.index.volumes[volume].geometry
	first := number * geometry.SegmentPages
	var run []pageRead
	var keys []diskKey
	for _, relative := range slices.Sorted(maps.Keys(located.pages)) {
		at, number := located.pages[relative], first+uint64(relative)
		page := pageDiskKey(identityOf(volume, number, at), geometry)
		if p.disk.has(ctx, page) {
			p.pulled.Add(int64(at.length))
			continue
		}
		run = append(run, pageRead{number: number, at: at})
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
			var held []byte
			err = p.store.readObject(ctx, key, func(ctx context.Context, from *tier) error {
				var err error
				held, err = from.readRange(ctx, key, extent.offset, extent.length, maximumReadExtent)
				return err
			})
			return held, err
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

// segmentBytes is one segment, decoded: from the disk's copy where it holds
// one, and from the store's otherwise, which this pull then copies.
func (p *Pull) segmentBytes(ctx context.Context, key diskKey, at segmentAddress) ([]byte, error) {
	if data, found := p.disk.decoded(ctx, key, p.store.codecs, maximumSegmentSize, accept); found {
		p.pulled.Add(int64(at.length))
		return data, nil
	}
	var data []byte
	encoded, err := p.fetch(ctx, func(ctx context.Context) ([]byte, error) {
		decoded, encoded, err := p.store.readSegment(ctx, at)
		data = decoded
		return encoded, err
	})
	if err != nil {
		return nil, err
	}
	return data, p.write(ctx, key, encoded)
}

// accept takes any bytes an envelope decodes to.
func accept([]byte) bool { return true }

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

// write copies one envelope to the disk. A write the disk refuses stops the
// pull; one the disk failed is the disk's to log, and the pull goes on. A
// window inside the cluster share is handed to the cluster as a fill instead,
// which the pull does not wait for: its stripes go to its ranks, this host's
// among them, or are dropped, and the store serves what was dropped.
func (p *Pull) write(ctx context.Context, key diskKey, data []byte) error {
	if p.store.cache.filler.inShare(key) {
		p.store.cache.fill(WriteFillPublication, []envelope{{key: key, data: data}})
		p.pulled.Add(int64(len(data)))
		return nil
	}
	if err := p.disk.write(ctx, key, data, WriteFillPublication); err != nil {
		return err
	}
	if p.disk.has(ctx, key) {
		p.pulled.Add(int64(len(data)))
	}
	return nil
}
