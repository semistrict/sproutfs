package checkpoint

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// A read of a range of a volume is a run of pages, and what it costs in
// requests is what the layout allows rather than how many pages it holds. The
// pages of a run are grouped by the part their members are in and by where in
// that part they sit: members that lie next to each other — which is what a
// checkpoint publishing consecutive pages of one volume writes, because it
// writes them in page order — are fetched as one extent, one ranged read, and
// decoded out of that one buffer. A page with no member at all costs nothing.
//
// The cached unit stays the member. A page's bytes are immutable under its
// identity, so two readers of one page share one entry however each of them
// reached it, and compaction moving the member leaves that entry alone. An
// extent has no identity: which members it carries depends on which run asked
// for it and on what else its part holds, so caching extents would give two
// readers of overlapping runs two copies of the pages they share and would make
// a run that is half cached fetch again the half it already has.

const (
	// readThroughBytes is the gap between two members of one part a run reads
	// through rather than splitting its request at. A request costs on the order
	// of ten milliseconds whatever it asks for, and a stream delivers far more
	// than this in that time, so bytes nothing wants are cheaper than a second
	// round trip until the gap grows large. Sixteen 4 KiB members is the bound
	// chosen: it holds a run together across the few pages a later checkpoint
	// rewrote in the middle of it, and a run whose halves sit in different
	// memory regions of one part still splits rather than dragging everything between
	// them along.
	readThroughBytes = 64 << 10
	// maximumReadExtent bounds what one request of a run fetches, which is what
	// one reader holds in memory while it decodes it. A 2 MiB run of 4 KiB pages
	// is about 2 MiB of adjacent members, so this admits one whole with room for
	// their envelopes; a longer run costs one more request per extent of it.
	maximumReadExtent = 4 << 20
	// readExtentConcurrency bounds how many extents of one run this store fetches
	// at a time. A run resolves to about one extent per part it reads from, so
	// this is concurrency over parts and never over pages.
	readExtentConcurrency = 8
	// maximumRunBytes bounds the volume one run covers, which is what bounds the
	// decoded pages one reader holds at once: a read of a larger range is served
	// as several runs, each still one request per extent it finds. It is the
	// largest read-ahead run a pager may be configured with, so no pager's load
	// is ever split.
	maximumRunBytes = 16 << 20
)

// runLimit is where the run holding one byte offset ends: maximumRunBytes on,
// rounded up to a whole page, so that no page is ever split across two runs and
// fetched twice.
func runLimit(geometry Geometry, cursor uint64) uint64 {
	pages := max(1, maximumRunBytes/geometry.PageSize)
	return (geometry.PageOf(cursor) + pages) * geometry.PageSize
}

// pageRead is one page of a run: which page it is, where its member lives,
// which byte of the page the read starts at, and the bytes of the caller's
// buffer it fills.
type pageRead struct {
	number uint64
	at     location
	within uint64
	dst    []byte
}

// readExtent is the bytes one ranged read fetches out of one part: the members
// of a run that lie in it next to each other, or near enough that reading
// through the gap between them costs less than a second request. members are
// positions in the list of the run's pages the fetch was asked for.
type readExtent struct {
	ref     control.Ref
	part    uint32
	offset  uint64
	length  uint64
	members []int
}

// readRun serves every page of a run. The members the cache already holds come
// from it and the rest are fetched in extents, all as one cache operation, so
// every page the run fetched is retained under its own identity and a later
// reader of any of them finds it there.
func (s *Store) readRun(ctx context.Context, geometry Geometry, volume string, run []pageRead) error {
	data, release, err := s.loadPages(ctx, geometry, volume, run)
	if err != nil {
		return err
	}
	defer release()
	fillPages(run, data)
	return nil
}

// loadPages returns the decoded member of every page of a run, one per page in
// the run's order, as readRun fetches them: through the cache, and in extents
// for the ones it does not hold. The cache is keyed by each page's identity —
// the checkpoint the page was first published under, which the index carries
// as the member's origin — rather than by the checkpoint whose part holds it
// now, so a fork hits its parent's entries and compaction moving the bytes
// costs neither a refetch nor a second entry. A member is exactly what was
// published, so one published while the volume was shorter is shorter than the
// page. The caller releases the bytes when it is done with them; dst is not
// used.
func (s *Store) loadPages(ctx context.Context, geometry Geometry, volume string, run []pageRead) ([][]byte, func(), error) {
	keys := make([]cacheKey, len(run))
	for at, page := range run {
		keys[at] = pageKey(identityOf(volume, page.number, page.at))
	}
	if s.cache == nil {
		wanted := make([]int, len(run))
		for at := range wanted {
			wanted[at] = at
		}
		data, _, err := s.fetchMembers(ctx, geometry, run, keys, wanted)
		return data, func() {}, err
	}
	return s.cache.getAll(ctx, keys, func(ctx context.Context, wanted []int) ([][]byte, []envelope, error) {
		return s.fetchMembers(ctx, geometry, run, keys, wanted)
	})
}

// fillPages copies each member's bytes into the part of the caller's buffer its
// page fills. A member published when the volume was shorter does not cover the
// whole page; the bytes past its end read as zeroes, which is how a grown
// volume reads.
func fillPages(run []pageRead, data [][]byte) {
	for at, page := range run {
		clear(page.dst)
		if page.within < uint64(len(data[at])) {
			copy(page.dst, data[at][page.within:])
		}
	}
}

// fetchMembers fetches and decodes the members at the given positions of a run.
// A window inside the share the cluster cache is on for is read from the
// cluster: this host's own stripes, then its peers'. Any other window the page
// cache's disk holds whole is read from it. The rest come from the store,
// grouped into as few requests as the layout allows. keys names every page of
// the run. The result holds one decoded page per position, in the order the
// positions were given, and the envelopes the store served, which the cache
// fills the cluster with once the run's callers have their pages.
func (s *Store) fetchMembers(ctx context.Context, geometry Geometry, run []pageRead, keys []cacheKey,
	wanted []int) ([][]byte, []envelope, error) {
	data := make([][]byte, len(wanted))
	// remote is the positions within wanted the store is to serve, and
	// cluster those the cluster is read for first.
	var remote, cluster []int
	for at, position := range wanted {
		key := diskKey{cacheKey: keys[position], span: windowSpan(geometry)}
		if s.readsCluster(key) {
			cluster = append(cluster, at)
			continue
		}
		if page, found := s.fromDisk(ctx, key, int(geometry.PageSize), validPage); found {
			data[at] = page
			s.checkHit(ctx, key, s.memberObject(run[position].at))
			continue
		}
		remote = append(remote, at)
	}
	if len(cluster) > 0 {
		missed, err := s.fromCluster(ctx, geometry, run, keys, wanted, cluster, data)
		if err != nil {
			return nil, nil, err
		}
		remote = append(remote, missed...)
		slices.Sort(remote)
	}
	if len(remote) == 0 {
		return data, nil, context.Cause(ctx)
	}
	positions := make([]int, len(remote))
	for at, position := range remote {
		positions[at] = wanted[position]
	}
	decoded, served, err := s.fromStore(ctx, geometry, run, keys, positions)
	if err != nil {
		return nil, nil, err
	}
	for at, position := range remote {
		data[position] = decoded[at]
	}
	return data, served, nil
}

// fromCluster reads the members at the positions within wanted that cluster
// names from the cluster, into data, and returns the positions it could not
// rebuild, which the store serves. Past the cluster's bound, it may read the
// store for the ones it is still waiting on as well, and take whichever
// answers first.
func (s *Store) fromCluster(ctx context.Context, geometry Geometry, run []pageRead, keys []cacheKey, wanted,
	cluster []int, data [][]byte) ([]int, error) {
	wants := make([]clusterWant, len(cluster))
	for at, position := range cluster {
		wants[at] = clusterWant{key: diskKey{cacheKey: keys[wanted[position]], span: windowSpan(geometry)},
			maximum: int(geometry.PageSize), valid: validPage}
	}
	hedge := func(ctx context.Context, ats []int) ([][]byte, error) {
		positions := make([]int, len(ats))
		for at, want := range ats {
			positions[at] = wanted[cluster[want]]
		}
		decoded, _, err := s.fromStore(ctx, geometry, run, keys, positions)
		return decoded, err
	}
	got, err := s.cache.reader.read(ctx, s.codecs, wants, hedge)
	if err != nil {
		return nil, err
	}
	var missed []int
	for at, position := range cluster {
		if got[at] == nil {
			missed = append(missed, position)
			continue
		}
		data[position] = got[at]
		s.checkHit(ctx, wants[at].key, s.memberObject(run[wanted[position]].at))
	}
	return missed, nil
}

// fromStore fetches the members at positions of a run from the store, grouped
// into as few requests as the layout allows, and returns one decoded page per
// position, in their order, and the envelopes the store served.
func (s *Store) fromStore(ctx context.Context, geometry Geometry, run []pageRead, keys []cacheKey,
	positions []int) ([][]byte, []envelope, error) {
	data := make([][]byte, len(positions))
	served := make([]envelope, len(positions))
	serve := func(ctx context.Context, held readExtent, encoded []byte) error {
		for _, at := range held.members {
			member := run[positions[at]].at
			sealed := encoded[member.offset-held.offset:][:member.length]
			page, err := s.codecs.Decode(ctx, sealed, int(geometry.PageSize))
			if err != nil {
				return errors.Join(ErrCorrupt, err)
			}
			if !validPage(page) {
				return ErrCorrupt
			}
			data[at] = page
			served[at] = envelope{key: diskKey{cacheKey: keys[positions[at]], span: windowSpan(geometry)},
				data: sealed}
		}
		return nil
	}
	if err := s.readExtents(ctx, groupMembers(run, positions), serve); err != nil {
		return nil, nil, err
	}
	return data, served, nil
}

// readsCluster reports whether key is read from the cluster: the cache keeps
// a disk that follows a list of caches, and the cluster cache is on for key's
// window.
func (s *Store) readsCluster(key diskKey) bool {
	return s.cache != nil && s.cache.reader != nil && s.cache.reader.on(key)
}

// memberObject names the part a member lies in, for a check that it is still
// there.
func (s *Store) memberObject(at location) func() (platform.ObjectKey, error) {
	return func() (platform.ObjectKey, error) { return s.partKey(at.ref, at.part) }
}

// checkHit samples one hit of the disk tier, local or cluster, and has the
// object it was served for checked to still exist (clusterReader.checkHit).
func (s *Store) checkHit(ctx context.Context, key diskKey, object func() (platform.ObjectKey, error)) {
	if s.cache != nil && s.cache.reader != nil {
		s.cache.reader.checkHit(ctx, key, s.objects, object)
	}
}

// validPage reports whether a decoded member is a page a volume can hold: whole
// sectors, and at least one of them.
func validPage(page []byte) bool { return len(page) != 0 && len(page)%SectorSize == 0 }

// groupMembers groups the members a run needs into the extents that fetch them:
// one per part, split wherever the gap between two of them is larger than
// reading through it is worth, or where the extent has grown past what one
// request carries.
func groupMembers(run []pageRead, wanted []int) []readExtent {
	order := make([]int, len(wanted))
	for at := range order {
		order[at] = at
	}
	slices.SortFunc(order, func(a, b int) int {
		first, second := run[wanted[a]].at, run[wanted[b]].at
		if found := compareRefs(first.ref, second.ref); found != 0 {
			return found
		}
		if found := cmp.Compare(first.part, second.part); found != 0 {
			return found
		}
		return cmp.Compare(first.offset, second.offset)
	})
	var extents []readExtent
	for _, at := range order {
		member := run[wanted[at]].at
		if count := len(extents); count > 0 {
			held := &extents[count-1]
			end := max(held.offset+held.length, member.offset+member.length)
			if held.ref == member.ref && held.part == member.part &&
				int64(member.offset)-int64(held.offset+held.length) <= readThroughBytes &&
				end-held.offset <= maximumReadExtent {
				held.length = end - held.offset
				held.members = append(held.members, at)
				continue
			}
		}
		extents = append(extents, readExtent{ref: member.ref, part: member.part,
			offset: member.offset, length: member.length, members: []int{at}})
	}
	return extents
}

// readExtents fetches every extent of a run, a few at a time, and hands each
// one's bytes to serve. Independent parts are therefore read at once rather
// than one after another. The first failure cancels the rest, and what the
// caller is told is that failure rather than the cancellation it caused.
func (s *Store) readExtents(ctx context.Context, extents []readExtent,
	serve func(context.Context, readExtent, []byte) error) error {
	read := func(ctx context.Context, held readExtent) error {
		key, err := s.partKey(held.ref, held.part)
		if err != nil {
			return err
		}
		return s.readObject(ctx, key, func(ctx context.Context, from *tier) error {
			encoded, err := from.readRange(ctx, key, held.offset, held.length, maximumReadExtent)
			if err != nil {
				return missingIsCorrupt(err)
			}
			return serve(ctx, held, encoded)
		})
	}
	if len(extents) == 1 {
		return read(ctx, extents[0])
	}
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var wait sync.WaitGroup
	var once sync.Once
	var failure error
	for range min(len(extents), readExtentConcurrency) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for running.Err() == nil {
				at := int(next.Add(1)) - 1
				if at >= len(extents) {
					return
				}
				if err := read(running, extents[at]); err != nil {
					once.Do(func() { failure = err; cancel() })
					return
				}
			}
		}()
	}
	wait.Wait()
	if failure != nil {
		return failure
	}
	return context.Cause(ctx)
}
