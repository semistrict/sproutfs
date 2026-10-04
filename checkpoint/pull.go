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

	"github.com/semistrict/sproutfs/platform/sim"
)

// Pull fetches every page one checkpoint holds, and the segments that locate
// them, in the background, so that a fault on any of them reads a host's disk
// rather than the object store. It is what a VM marked to pull its whole
// memory starts while it runs. It is a prefetch with no guarantee: what it
// fetched may be evicted later like anything else, and it stops short under
// pressure.
//
// Where a window is depends on the share the cluster cache is turned on for.
//
//   - Inside the share, the pull makes sure the cluster holds the window, and
//     copies nothing whole onto this host's disk. It asks the window's ranks
//     which stripes they hold (presence.go) and reads from the store only the
//     pages the cluster lacks, which it hands to the cluster's fills under
//     their rules: its stripes go to the window's ranks, this host's own disk
//     among them only where the membership ranks it, and a fill that finds the
//     queue full is dropped. A segment, which the pull must read to list the
//     pages, is read through the cluster and from the store only where the
//     cluster lacks it. So a pulled VM restarted on any host reads its pages
//     from the hosts' disks, and the cluster keeps each window once rather
//     than once more on every host that pulled it.
//   - Outside the share, the pull copies the window whole onto this host's own
//     disk, as before the cluster cache: a fault on a page of the checkpoint
//     that the page cache's memory does not hold is served from the disk while
//     the disk holds it, and makes no request of the object store.
//
// Every read a pull makes is background work, behind every fault. Its reads
// are marked as a prefetch (WithPrefetch): its requests of peers go over the
// bulk class under the host's background budget, and a read of the cluster for
// it asks no second request, never reads the store as a hedge, and leaves the
// delay alone. It takes none of the cache's load slots and joins none of a
// fault's fetches, so a fault for a page the pull has not reached is read at
// once, as it would be without a pull. Before each fetch, presence check
// included, the pull waits until no load of the cache is in flight, and every
// pull on the host shares pullConcurrency fetches.
//
// A pull stops short under pressure. When the host's memory budget refuses a
// reservation that no cache can give back room for (resource.Budget.Pressure),
// or the disk limiter shrinks the cache's share (Cache.FitDisk), every pull's
// reads in flight are cancelled and the pull ends with ErrPressure. What it
// did stays; the cluster and the store serve the rest.
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
// those pages. Inside the share every publication fills the cluster itself.
type Pull struct {
	store *Store
	// disk is the host's own disk, nil for a host that keeps none and fills
	// the cluster with every window.
	disk  *cacheDisk
	index *Index
	// bytes is what the checkpoint holds, as its root records it, and pulled
	// how much of it this pull found held, wrote to the disk, or handed to
	// the cluster's fills; held is what it found the disk or the cluster held
	// already, and fetched what it read from the store.
	bytes   int64
	pulled  atomic.Int64
	held    atomic.Int64
	fetched atomic.Int64
	// kept is what later publications added to the disk, or filled the
	// cluster with.
	kept atomic.Int64
	// base is the context the pull began under, without its cancellation.
	base context.Context

	// mu orders keep against Close, so nothing is kept for a pull given up.
	mu     sync.Mutex
	closed bool

	cancel context.CancelCauseFunc
	done   chan struct{}
	err    error
	close  sync.Once
}

// PullStats is how far one pull has come.
type PullStats struct {
	// Bytes is what the checkpoint holds and Pulled how much of it the pull
	// found on the disk or in the cluster, wrote to the disk, or, for a
	// window inside the cluster share, handed to the cluster's fills, which
	// may drop it. Held is what it found held already, and Fetched what it
	// read from the store. Kept is what the VM's later publications added.
	// The disks give their oldest regions back under pressure, so these say
	// what the pull did, not what the disks still hold.
	Bytes, Pulled, Held, Fetched, Kept int64
	// Done reports a pull that has stopped fetching: complete when Err is nil,
	// and stopped short by Err otherwise, ErrPressure among them. A pull that
	// stopped short keeps what it did, and the cluster and the store serve
	// the rest.
	Done bool
	Err  error
}

// ErrPressure stops a pull the host is short of memory or disk for.
var ErrPressure = errors.New("checkpoint: the host is short of memory or disk, and stopped a pull")

var (
	// errMemoryPressure and errDiskPressure are the causes a pull is
	// cancelled with.
	errMemoryPressure = fmt.Errorf("%w: the memory budget refused a reservation no cache could make room for",
		ErrPressure)
	errDiskPressure = fmt.Errorf("%w: the disk limiter shrank the cache", ErrPressure)
)

// The probes a pull marks.
const (
	// ProbePullHeld is a page or segment a pull found the cluster or the
	// disk held, and fetched nothing for.
	ProbePullHeld = "checkpoint/pull-held"
	// ProbePullFetched is an extent or a segment a pull read from the store.
	ProbePullFetched = "checkpoint/pull-fetched"
	// ProbePullPressure is a pull stopped short under pressure.
	ProbePullPressure = "checkpoint/pull-stopped-for-pressure"
)

// buggifyPullPressure presses the host's pulls before one of their fetches,
// as a shortage of memory or disk does.
const buggifyPullPressure = "checkpoint/pull-pressure"

// pullConcurrency is how many fetches every pull on a host has in flight
// together. A pull is background work: it takes few of the store's requests,
// and none of the page cache's load slots, so a fault never queues behind it.
const pullConcurrency = 2

// pulls is every pull a cache is running: the fetch slots they share, and
// what pressure cancels.
type pulls struct {
	slots chan struct{}

	mu      sync.Mutex
	running map[*Pull]struct{}
}

func newPulls() *pulls {
	return &pulls{slots: make(chan struct{}, pullConcurrency), running: make(map[*Pull]struct{})}
}

func (s *pulls) add(p *Pull) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[p] = struct{}{}
}

func (s *pulls) remove(p *Pull) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, p)
}

// press cancels every pull running, with cause.
func (s *pulls) press(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.running {
		if !p.bug("pull-ignores-pressure") {
			p.cancel(cause)
		}
	}
}

// acquire takes one of the fetch slots every pull on the host shares.
func (s *pulls) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *pulls) release() { <-s.slots }

// Pull begins fetching every page of index and returns at once. It refuses,
// with ErrNoDisk, a host that keeps no disk and fills no cluster, and with
// ErrDiskFull a checkpoint larger than all the disk's share could hold where
// some of it would be kept whole on the disk; nothing is fetched then, and
// reads go to the store as they always do. Which windows go whole is known
// only once the segments are read, so with the cluster cache on for part of
// the windows the whole checkpoint is counted.
func (s *Store) Pull(ctx context.Context, index *Index) (*Pull, error) {
	if s.cache == nil || s.cache.disk == nil && !s.cache.fills() {
		return nil, ErrNoDisk
	}
	bytes := index.heldBytes()
	if disk := s.cache.disk; disk != nil && s.cache.cluster.percent < 100 && !disk.holds(bytes) {
		return nil, fmt.Errorf("%w: %s holds %d bytes and the disk's share keeps at most %d", ErrDiskFull,
			index.Ref(), bytes, disk.capacity())
	}
	base := context.WithoutCancel(ctx)
	reads := WithPrefetch(base)
	if sim.Bug(base, "pull-reads-as-a-fault") {
		// The guard leaves the pull's reads unmarked, as a fault's are.
		reads = base
	}
	running, cancel := context.WithCancelCause(reads)
	p := &Pull{store: s, disk: s.cache.disk, index: index, bytes: bytes, base: base, cancel: cancel,
		done: make(chan struct{})}
	s.cache.pulls.add(p)
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
	stats := PullStats{Bytes: p.bytes, Pulled: p.pulled.Load(), Held: p.held.Load(), Fetched: p.fetched.Load(),
		Kept: p.kept.Load()}
	select {
	case <-p.done:
		stats.Done, stats.Err = true, p.err
	default:
	}
	return stats
}

// StopFetching stops fetching the checkpoint and waits for the fetch in
// flight, and leaves the keeping on: a publication of the VM after it still
// keeps its pages. It is what a VM that stops running here calls before its
// last publication, which Close then follows. Calling it again does nothing.
func (p *Pull) StopFetching() {
	p.cancel(nil)
	<-p.done
}

// Close stops the pull and its keeping. What it copied stays on the disk.
// Calling it again does nothing.
func (p *Pull) Close() {
	p.close.Do(func() {
		p.cancel(nil)
		<-p.done
		p.mu.Lock()
		defer p.mu.Unlock()
		p.closed = true
		if p.disk != nil && sim.Bug(p.base, "diskcache-pull-frees-on-close") {
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
		if p.disk == nil {
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

// run fetches the checkpoint one segment at a time, in volume and segment
// order, until it is done or cancelled.
func (p *Pull) run(ctx context.Context) {
	defer close(p.done)
	defer p.store.cache.pulls.remove(p)
	if !p.bug("pull-ignores-pressure") {
		pressure := p.store.cache.resources.Pressure()
		go func() {
			select {
			case <-pressure:
				p.cancel(errMemoryPressure)
			case <-p.done:
			}
		}()
	}
	for _, name := range p.index.names {
		table := p.index.volumes[name]
		for _, number := range slices.Sorted(maps.Keys(table.segments)) {
			err := p.segment(ctx, name, number, table.segments[number])
			if err == nil {
				continue
			}
			cause := context.Cause(ctx)
			switch {
			case errors.Is(cause, ErrPressure):
				err = cause
				sim.Probe(p.base, ProbePullPressure)
				slog.WarnContext(ctx, "checkpoint: a pull stopped short under pressure; the cluster and the store serve the rest",
					"checkpoint", p.index.Ref().String(), "error", err)
			case cause == nil:
				slog.WarnContext(ctx, "checkpoint: a pull stopped short; the store serves what it did not reach",
					"checkpoint", p.index.Ref().String(), "volume", name, "segment", number, "error", err)
			}
			p.err = err
			return
		}
	}
}

// bug reports whether the in-tree bug id is on for the pull's run.
func (p *Pull) bug(id string) bool { return sim.Bug(p.base, id) }

// inShare reports whether key's window is inside the share the cluster cache
// is on for, which the pull fills the cluster with rather than copying whole.
func (p *Pull) inShare(key diskKey) bool { return p.store.readsCluster(key) }

// found counts bytes the pull found held and fetched nothing for.
func (p *Pull) found(bytes uint64) {
	p.held.Add(int64(bytes))
	p.pulled.Add(int64(bytes))
	sim.Probe(p.base, ProbePullHeld)
}

// segment fetches one segment and every member it locates that the disk, or
// the cluster inside the share, does not already hold.
func (p *Pull) segment(ctx context.Context, volume string, number uint64, entry segmentEntry) error {
	key := segmentDiskKey(volume, number, entry.at.ref)
	data, err := p.segmentBytes(ctx, key, entry.at)
	if err != nil {
		return err
	}
	located, err := decodeTable(ctx, data)
	if err != nil {
		return err
	}
	if err := p.index.checkTable(volume, located); err != nil {
		return err
	}
	geometry := p.index.volumes[volume].geometry
	first := number * geometry.SegmentPages
	var run []pageRead
	var keys []diskKey
	// shared is the positions in run of the pages inside the share, which
	// the cluster is asked about.
	var shared []int
	for relative, at := range located.all {
		number := first + uint64(relative)
		page := pageDiskKey(identityOf(volume, number, at), geometry)
		switch {
		case p.inShare(page):
			shared = append(shared, len(run))
		case p.disk == nil:
			// A host that keeps no disk has nowhere to keep a window outside
			// the share: the store serves it.
			continue
		case p.disk.has(ctx, page):
			p.found(at.length)
			continue
		}
		run = append(run, pageRead{number: number, at: at})
		keys = append(keys, page)
	}
	if len(shared) > 0 {
		if run, keys, err = p.lacking(ctx, run, keys, shared); err != nil {
			return err
		}
	}
	wanted := make([]int, len(run))
	for at := range wanted {
		wanted[at] = at
	}
	for _, extent := range groupMembers(run, wanted) {
		var held []byte
		err := p.fetch(ctx, func(ctx context.Context) error {
			key, err := p.store.partKey(extent.ref, extent.part)
			if err != nil {
				return err
			}
			return p.store.readObject(ctx, key, func(ctx context.Context, from *tier) error {
				var err error
				held, err = from.readRange(ctx, key, extent.offset, extent.length, maximumReadExtent)
				return err
			})
		})
		if err != nil {
			return err
		}
		p.fetched.Add(int64(len(held)))
		sim.Probe(p.base, ProbePullFetched)
		for _, at := range extent.members {
			member := run[at].at
			if err := p.write(ctx, keys[at], held[member.offset-extent.offset:][:member.length]); err != nil {
				return err
			}
		}
	}
	return nil
}

// lacking asks the cluster about the pages of run at the positions shared,
// counts those it holds, and returns run and its keys without them: what is
// left to read from the store.
func (p *Pull) lacking(ctx context.Context, run []pageRead, keys []diskKey, shared []int) ([]pageRead, []diskKey,
	error) {
	asked := make([]diskKey, len(shared))
	for at, position := range shared {
		asked[at] = keys[position]
	}
	var held []bool
	err := p.fetch(ctx, func(ctx context.Context) error {
		held = p.store.cache.reader.holds(ctx, asked)
		return context.Cause(ctx)
	})
	if err != nil {
		return nil, nil, err
	}
	if p.bug("pull-reads-the-store-first") {
		// The guard reads every page from the store, as the pull did before
		// it asked the cluster.
		clear(held)
	}
	skip := make(map[int]bool)
	for at, position := range shared {
		if held[at] {
			skip[position] = true
			p.found(run[position].at.length)
		}
	}
	var left []pageRead
	var leftKeys []diskKey
	for position := range run {
		if !skip[position] {
			left, leftKeys = append(left, run[position]), append(leftKeys, keys[position])
		}
	}
	return left, leftKeys, nil
}

// segmentBytes is one segment, decoded. Inside the share it is read through
// the cluster, and from the store only where the cluster lacks it, which this
// pull then fills the cluster with. Outside it, it comes from the disk's copy
// where the disk holds one, and from the store otherwise, which this pull
// then copies.
func (p *Pull) segmentBytes(ctx context.Context, key diskKey, at segmentAddress) ([]byte, error) {
	if p.inShare(key) {
		var data []byte
		var refill []envelope
		err := p.fetch(ctx, func(ctx context.Context) error {
			var hedge storeHedge
			if p.bug("pull-hedges-to-the-store") {
				// The guard has a pull's read of the cluster read the store
				// as well past its bound, as a fault's does.
				hedge = func(ctx context.Context, _ []int) ([][]byte, error) {
					data, _, err := p.store.readSegment(ctx, at)
					return [][]byte{data}, err
				}
			}
			got, rebuilt, err := p.store.cache.reader.read(ctx, p.store.codecs,
				[]clusterWant{{key: key, maximum: maximumSegmentSize, valid: anySegment}}, hedge)
			if err != nil {
				return err
			}
			data, refill = got[0], rebuilt
			return nil
		})
		if err != nil {
			return nil, err
		}
		if data != nil {
			p.found(at.length)
			p.store.cache.fill(WriteFillPublication, refill)
			return data, nil
		}
	} else if p.disk != nil {
		if data, _, found := p.disk.decoded(ctx, key, p.store.codecs, maximumSegmentSize, accept); found {
			p.found(at.length)
			return data, nil
		}
	}
	var data, encoded []byte
	err := p.fetch(ctx, func(ctx context.Context) error {
		var err error
		data, encoded, err = p.store.readSegment(ctx, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	p.fetched.Add(int64(len(encoded)))
	sim.Probe(p.base, ProbePullFetched)
	if !p.inShare(key) && p.disk == nil {
		return data, nil
	}
	return data, p.write(ctx, key, encoded)
}

// accept takes any bytes an envelope decodes to.
func accept([]byte) bool { return true }

// fetch runs one piece of the pull's work behind every fault: once no load of
// the cache is in flight, and within the fetches every pull on the host
// shares.
func (p *Pull) fetch(ctx context.Context, work func(context.Context) error) error {
	cache := p.store.cache
	if err := cache.quiet(ctx); err != nil {
		return err
	}
	if err := cache.pulls.acquire(ctx); err != nil {
		return err
	}
	defer cache.pulls.release()
	if sim.Buggify(p.base, buggifyPullPressure, 0.02) {
		cache.pulls.press(errMemoryPressure)
	}
	if p.bug("pull-takes-a-fault-slot") {
		// The guard runs the work in a slot of the loads something waits
		// on, as a pull read through the cache unmarked would.
		release, err := cache.holdLoadSlot(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return work(ctx)
}

// write keeps one envelope. A window inside the cluster share is handed to
// the cluster as a fill, which the pull does not wait for: its stripes go to
// its ranks, this host's among them where it is ranked, or are dropped, and
// the store serves what was dropped. Any other is copied whole to the disk. A
// write the disk refuses stops the pull; one the disk failed is the disk's to
// log, and the pull goes on.
func (p *Pull) write(ctx context.Context, key diskKey, data []byte) error {
	if p.inShare(key) {
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
