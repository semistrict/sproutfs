package vmmemory

import (
	"context"
	"sync"

	"github.com/semistrict/sproutfs/platform/sim"
)

// A memory region reads ahead by streams, as Linux's on-demand read-ahead does
// (mm/readahead.c), and not by windows. A fault that goes on none of its
// streams is at random: it reads its page alone and starts a stream there. A
// fault on a page after a stream's latest, no further on than what the stream
// read or prefetched, goes on that stream. A stream's second fault still
// reads its page alone, and only its third prefetches, initialReadAhead
// pages; each fault after that prefetches four times as many as the one
// before, up to the end of its read-ahead window.
//
// A guest that reads forwards so pays a few faults where it begins, and then a
// fault each time it reaches the end of what was prefetched, each further
// ahead. A guest that reads at random pays nothing for read-ahead: a 4 KiB
// pager sees a read of two pages as two faults, the second on the page after
// the first, and a read-ahead that took that for a guest reading forwards
// prefetched a whole window behind every such read. On GCE on 2026-10-09 the
// embedder's PostgreSQL benchmark read 8 KiB at random from a 12 GiB file on a
// 4 KiB pager that way: each fault loaded 44 pages and evicted as many, and
// the guest read 252 times a second against 23,825 on plain Linux.
//
// A memory region's first fault starts a stream that has gone on as far as any
// has, and prefetches the rest of its window: a boot and a restore begin by
// reading forwards.
//
// A pager that prefetches at random (Config.PrefetchAtRandom) prefetches the
// rest of a fault's window behind every fault, as it did before streams:
// its windows are a few large pages each.

// recentStreams is how many of a memory region's latest streams a fault is
// compared with. Eight lets that many threads of a guest each read forwards at
// once.
const recentStreams = 8

// initialReadAhead is how many pages a stream's first prefetch reads, and
// readAheadGrowth how many times as many each prefetch after it does.
const (
	initialReadAhead = 4
	readAheadGrowth  = 4
)

// readAhead is a memory region's latest streams of forward faults. A fault
// that starts a stream takes the place of the stream used least recently of
// those that never went on, and of all of them only where every one has: a
// guest's threads reading at random start a stream a fault, and must not push
// out the one a thread reading forwards has earned. tick counts faults, and
// orders the streams by their latest.
type readAhead struct {
	mu      sync.Mutex
	streams [recentStreams]stream
	count   int
	tick    uint64
}

// stream is one run of forward faults: last is the page of its latest, end
// the page after the last one that fault read or prefetched, faults how many
// faults it has had, and size how many pages it has earned to prefetch, which
// its window's end may cut short.
type stream struct {
	last, end uint64
	faults    int
	size      uint64
	used      uint64
}

// goesOn reports whether a fault on page goes on the stream: it is no further
// back than the stream's latest fault, and no further on than one more
// prefetch past what that fault brought in. Short of end is a page a prefetch
// did not reach, for want of free slots, or one evicted since; past it, a
// guest reading forwards that skipped a few pages. A fault on the latest's own
// page is that fault again, one that planned anew, and goes on it without
// going further.
func (s *stream) goesOn(page uint64) bool {
	return page >= s.last && page <= s.end+s.size
}

// grow counts one more fault on the stream, and the pages it has earned, up
// to limit.
func (s *stream) grow(ctx context.Context, limit uint64) {
	s.faults++
	switch {
	case sim.Bug(ctx, "pager-read-ahead-a-window-at-once"):
		// The bug has a stream earn its whole window at once.
		s.size = limit
	case s.size > 0:
		s.size = min(s.size*readAheadGrowth, limit)
	case s.faults >= 3 || s.faults == 2 && sim.Bug(ctx, "pager-read-ahead-on-a-second-fault"):
		s.size = initialReadAhead
	}
}

// readsAhead reports the pages [first, end) a fault on page reads, its own and
// those it prefetches, all in its window [start, stop), and records the fault
// among its memory region's streams. A fault that reads its page alone
// reports [page, page+1), and one whose stream has earned a whole window
// reads its window whole. What else of its window a fault maps, the pages
// resident there, is not read and is not this: that is its plan's.
func (r *MemoryRegion) readsAhead(ctx context.Context, page, start, stop uint64) (first, end uint64) {
	if r.host.cfg.PrefetchAtRandom && !sim.Bug(ctx, "pager-read-alone-at-random") ||
		sim.Bug(ctx, "pager-prefetch-every-fault") {
		r.streams.record(ctx, page, start, stop, uint64(r.readAheadPages))
		return start, stop
	}
	return r.streams.record(ctx, page, start, stop, uint64(r.readAheadPages))
}

// record records a fault on page, whose window is [start, stop), and reports
// the pages it reads. No stream earns more than limit, a window.
func (ra *readAhead) record(ctx context.Context, page, start, stop, limit uint64) (first, end uint64) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	s := ra.find(page)
	switch {
	case s == nil:
		began := ra.count == 0
		s = ra.place(ctx)
		*s = stream{faults: 1}
		if began {
			// The first fault of a memory region: a stream that has gone on
			// as far as any.
			s.faults, s.size = 3, limit
		}
	case page != s.last:
		s.grow(ctx, limit)
	}
	first, end = page, page+1+min(s.size, stop-page-1)
	if s.size >= limit {
		first, end = start, stop
	}
	ra.tick++
	s.last, s.end, s.used = page, end, ra.tick
	return first, end
}

// place is where a new stream goes: a place no stream has taken yet, or the
// one of the stream used least recently of those that never went on, or of
// all where every one has.
func (ra *readAhead) place(ctx context.Context) *stream {
	if ra.count < recentStreams {
		ra.count++
		return &ra.streams[ra.count-1]
	}
	var least, leastAlone *stream
	for k := range ra.streams {
		s := &ra.streams[k]
		if least == nil || s.used < least.used {
			least = s
		}
		if s.faults == 1 && (leastAlone == nil || s.used < leastAlone.used) {
			leastAlone = s
		}
	}
	if leastAlone != nil && !sim.Bug(ctx, "pager-random-fault-replaces-a-stream") {
		return leastAlone
	}
	return least
}

// find is the stream a fault on page goes on, or nil.
func (ra *readAhead) find(page uint64) *stream {
	for k := range ra.count {
		if s := &ra.streams[k]; s.goesOn(page) {
			return s
		}
	}
	return nil
}
