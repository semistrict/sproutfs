package checkpoint

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
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
	// regions of one part still splits rather than dragging everything between
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
)

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
	if s.cache == nil {
		wanted := make([]int, len(run))
		for at := range wanted {
			wanted[at] = at
		}
		data, err := s.fetchMembers(ctx, geometry, run, wanted)
		if err != nil {
			return err
		}
		fillPages(run, data)
		return nil
	}
	keys := make([]cacheKey, len(run))
	for at, page := range run {
		keys[at] = pageKey(identityOf(volume, page.number, page.at))
	}
	data, release, err := s.cache.getAll(ctx, keys, func(ctx context.Context, wanted []int) ([][]byte, error) {
		return s.fetchMembers(ctx, geometry, run, wanted)
	})
	if err != nil {
		return err
	}
	defer release()
	fillPages(run, data)
	return nil
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

// fetchMembers fetches and decodes the members at the given positions of a run,
// grouped into as few requests as the layout allows. The result holds one
// decoded page per position, in the order the positions were given.
func (s *Store) fetchMembers(ctx context.Context, geometry Geometry, run []pageRead, wanted []int) ([][]byte, error) {
	data := make([][]byte, len(wanted))
	serve := func(ctx context.Context, held readExtent, encoded []byte) error {
		for _, at := range held.members {
			member := run[wanted[at]].at
			page, err := s.codecs.Decode(ctx,
				encoded[member.offset-held.offset:][:member.length], int(geometry.PageSize))
			if err != nil {
				return errors.Join(ErrCorrupt, err)
			}
			if len(page) == 0 || len(page)%SectorSize != 0 {
				return ErrCorrupt
			}
			data[at] = page
		}
		return nil
	}
	if err := s.readExtents(ctx, groupMembers(run, wanted), serve); err != nil {
		return nil, err
	}
	return data, nil
}

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
		encoded, err := s.readRange(ctx, key, held.offset, held.length, maximumReadExtent)
		if err != nil {
			if errors.Is(err, platform.ErrNotFound) || errors.Is(err, platform.ErrInvalidRange) {
				return errors.Join(ErrCorrupt, err)
			}
			return err
		}
		return serve(ctx, held, encoded)
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
