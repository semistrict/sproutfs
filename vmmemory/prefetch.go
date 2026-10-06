package vmmemory

import (
	"context"
	"errors"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A fault reads its own page first. The rest of its read-ahead run is a
// prefetch: one read on a goroutine of its own, started beside the fault's
// read, of the run's pages that need bytes. The fault installs its page, with
// whatever of the window was already resident, and wakes the guest as soon as
// that one page is in; it never waits for the prefetch. A guest that follows
// pointers knows its next address only once this page is in, so a run read
// before the page cost every hop of such a chain the whole run: on GCE on
// 2026-10-03 a 4 KiB page from the cluster took 0.65 ms and its 8 MiB run
// 39 ms (docs/measurements/gce-dependent-reads-2026-10-03.md).
//
// A fault that follows one of its memory region's recent faults, in its own
// run or the one before, prefetches (followsRecent). One at random prefetches
// only in a pager of large pages (Config.PrefetchAtRandom,
// PrefetchesAtRandom), and otherwise reads its page alone. A prefetch costs
// processors a page as a fault's read does: a run of 4 KiB pages is 2,047 of
// them to prefetch, and one of 2 MiB pages three.
//
// A prefetch's pages land as clean pages under their identities, idle and in
// the sharing index, as a page every memory region has stopped mapping is. The
// prefetch then maps them read-only into the memory region whose fault asked
// for them, once that window's locks are free, so a guest reading forwards
// takes no fault on them: a store trap on a page it does not map would make a
// private copy of it. A page that cannot land like that is not prefetched, and
// is left to its own fault: a page whose bytes go in the memory region's own
// file, a page another host still holds, and a page with no identity.
//
// The rules that keep it from ever costing a fault:
//
//   - A fault never waits for a prefetch, except for a page that prefetch is
//     already reading: its READ request waits on the prefetch's rather than
//     reading the page a second time (awaitPrefetch, pagerequests.go). So a
//     guest reading forwards still reads its memory in runs, one read for the
//     faulting page and one for the rest.
//   - A prefetch takes only slots that are free, giving up idle pages for them
//     as read-ahead always has, and never evicts. Its landed pages are idle
//     until something maps them, so they are the first memory an allocation
//     gives up. An allocation that would otherwise evict a page a guest maps
//     cancels the prefetches still reading and takes their slots
//     (cancelPrefetchesLocked).
//   - At most Config.PrefetchRuns prefetches are reading at once. Past that a
//     fault reads its page and nothing else, and its neighbours fault for
//     themselves.
//   - Its reads are marked (checkpoint.WithPrefetch): they go over the peers'
//     bulk class, take the page cache's prefetch slots, and never hedge.
//
// A post-copy stream's faults read their whole runs at once (WithStream):
// nothing waits on them, and a stream of pages only another host holds is
// what a run is for.

// ProbePrefetchLanded marks a prefetch whose pages landed, ProbePrefetchMapped
// one that mapped them into the memory region that asked, ProbePrefetchWaited a
// fault that waited for a prefetch already reading its page,
// ProbePrefetchRefused a run left unread because the prefetches in flight were
// at their bound, ProbePrefetchCancelled a prefetch an allocation cancelled
// for its slots, ProbePrefetchDuplicate a landed page whose identity another
// load had made resident first, and ProbePrefetchHeld a page a migration's
// source turned out still to hold, which a prefetch drops, and
// ProbePrefetchRandom a run left unread because its fault followed none of
// its memory region's recent faults.
const (
	ProbePrefetchLanded    = "vmmemory/prefetch-landed"
	ProbePrefetchMapped    = "vmmemory/prefetch-mapped"
	ProbePrefetchWaited    = "vmmemory/prefetch-waited"
	ProbePrefetchRefused   = "vmmemory/prefetch-refused"
	ProbePrefetchCancelled = "vmmemory/prefetch-cancelled-for-pressure"
	ProbePrefetchDuplicate = "vmmemory/prefetch-duplicate"
	ProbePrefetchHeld      = "vmmemory/prefetch-held-by-source"
	ProbePrefetchRandom    = "vmmemory/prefetch-random"
)

// PrefetchProbes is every probe a prefetch marks.
func PrefetchProbes() []string {
	return []string{ProbePrefetchLanded, ProbePrefetchMapped, ProbePrefetchWaited, ProbePrefetchRefused,
		ProbePrefetchCancelled, ProbePrefetchDuplicate, ProbePrefetchHeld, ProbePrefetchRandom}
}

// The fault-injection sites of a prefetch. buggifyPrefetchSlow holds its read
// back, so faults meet it in flight; buggifyPrefetchRefused leaves a run
// unread as if the prefetches in flight were at their bound; and
// buggifyPrefetchFailed fails its read after the backing answered.
const (
	buggifyPrefetchSlow    = "vmmemory/prefetch-slow"
	buggifyPrefetchRefused = "vmmemory/prefetch-refused"
	buggifyPrefetchFailed  = "vmmemory/prefetch-failed"
)

// PrefetchSites is every fault-injection site of a prefetch.
func PrefetchSites() []string {
	return []string{buggifyPrefetchSlow, buggifyPrefetchRefused, buggifyPrefetchFailed}
}

// errPrefetchFailed is the read failure buggifyPrefetchFailed injects, and
// errMemoryPressure the cause a prefetch is cancelled with when an allocation
// needs its slots.
var (
	errPrefetchFailed = errors.New("vmmemory: injected failure of a prefetch's read")
	errMemoryPressure = errors.New("vmmemory: an allocation needed the slots of a prefetch")
	errDetaching      = errors.New("vmmemory: the memory region a prefetch was for is detaching")
)

// prefetch is the rest of one fault's run, read behind the fault.
type prefetch struct {
	memoryRegion *MemoryRegion
	// start and end are the window, and pages the pages it reads, in page
	// order, each with its identity and the free slot reserved for it.
	start, end uint64
	pages      []prefetchPage
	cancel   context.CancelCauseFunc
	// reading is set from the split until the read has ended, and holding
	// until every slot is settled: given back, or holding a page that landed,
	// idle. Until then a slot is neither free nor a page, and an allocation
	// short of one waits for it rather than evict (cancelPrefetchesLocked).
	// cancelled marks one an allocation has cancelled already. All are guarded
	// by Host.mu.
	reading, holding, cancelled bool
	// finished is set once the read has ended (finish). Guarded by Host.mu.
	finished bool
	// ctx is what the prefetch runs under: the values of the context of the
	// fault that split it off, a task of its own in a controlled run, and
	// the prefetch mark; cancel ends it.
	ctx context.Context
}

type prefetchPage struct {
	page uint64
	key  pageKey
	at   fileSlot
}

type streamKey struct{}

// WithStream marks the faults made under ctx as a post-copy stream's: they
// bring pages in behind a running guest that waits on none of them, so each
// reads its whole run at once rather than its page first.
func WithStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamKey{}, true)
}

func streaming(ctx context.Context) bool {
	stream, _ := ctx.Value(streamKey{}).(bool)
	return stream
}

// prefetchSettleSeam runs once a prefetch's read is over and before its slots
// are settled: given back where the read failed, or filled with the pages it
// landed. A test holds a prefetch there to put an allocation in that moment.
var prefetchSettleSeam func()

// SettlePrefetches returns once none of this memory region's prefetches is
// running: each has landed or dropped its pages and mapped what it could.
// Nothing waits on a prefetch; this is what a test of a guest that reads only
// once its neighbours are in waits on.
func (r *MemoryRegion) SettlePrefetches(ctx context.Context) error {
	z := r.zircon

	return z.settlePrefetches(ctx)
}

// settlePrefetchesCounted waits for this memory region's count of running
// prefetches to reach zero, which both cores keep.
func (r *MemoryRegion) settlePrefetchesCounted(ctx context.Context) error {
	h := r.host
	for {
		h.mu.Lock()
		running, changed := r.prefetchRunning, h.changed
		h.mu.Unlock()
		if running == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// SettlePrefetches returns once no prefetch is running: every one has landed
// or dropped its pages and mapped what it could. Nothing waits on a prefetch;
// this is what a test, or a benchmark about to read what is resident, waits
// on.
func (h *Host) SettlePrefetches(ctx context.Context) error {
	z := h.zircon

	return z.settlePrefetches(ctx)
}

// settlePrefetchesCounted waits for the host's count of running prefetches
// to reach zero, which both cores keep.
func (h *Host) settlePrefetchesCounted(ctx context.Context) error {
	for {
		h.mu.Lock()
		running, changed := h.prefetchRunning, h.changed
		h.mu.Unlock()
		if running == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// PrefetchesAtRandom reports whether a pager of pages of pageSize should
// prefetch behind a fault at random too (Config.PrefetchAtRandom): one of
// 2 MiB pages or larger should, and one of smaller pages should not.
//
// Whether a prefetch at random pays depends on whether the guest goes on to
// touch the rest of the run, which no fault can tell. What tips it is the
// prefetch's cost, which is a page's processor time for each page. At 4 KiB a
// run is 2,048 pages: on GCE on 2026-10-04 a chain of 4 KiB faults from the
// cluster that prefetched behind every fault took 24 ms a hop against 3.2 ms
// read alone, because each prefetch was about 100 ms of processor
// (docs/measurements/gce-fault-first-2026-10-04.md). At 2 MiB a run is four
// pages, the same chain took 10.4 ms a hop against 7.1 ms, and a guest of
// 2 MiB pages is few enough pages that one reading at random touches most of
// its runs: Valkey restored with a 4 GiB heap, whose 20,000 dependent GETs
// fault in about 2,800 of its 4,096 pages, ran them in 11.7 s from the
// cluster and 32.5 s from the store prefetching behind every fault, against
// 22.5 s and 99.8 s reading its pages alone, and 22.4 s and 55.6 s reading
// each run before its page (docs/measurements/gce-real-app-restore-2026-10-04.md).
func PrefetchesAtRandom(pageSize uint64) bool {
	return pageSize >= checkpoint.PageSize2MiB
}

// prefetches reports whether a fault in the window that begins at start
// prefetches the rest of its window behind its page: one that follows a
// recent fault of its memory region, and in a pager that prefetches at random
// any fault. It records the window among the memory region's recent faults.
func (r *MemoryRegion) prefetches(ctx context.Context, start uint64) bool {
	follows := r.followsRecent(start)
	random := r.host.cfg.PrefetchAtRandom && !sim.Bug(ctx, "pager-read-alone-at-random")
	return follows || random || sim.Bug(ctx, "pager-prefetch-every-fault")
}

// recentFaults is how many of a memory region's latest faulting windows a
// fault is compared with to tell a guest reading forwards from one reading at
// random. Eight lets that many threads of a guest each read forwards at once.
const recentFaults = 8

// faultHistory is the windows of a memory region's latest faults that read
// its backing.
type faultHistory struct {
	mu      sync.Mutex
	windows [recentFaults]uint64
	next    int
	count   int
}

// followsRecent reports whether a fault in the window that begins at start
// follows one of this memory region's recent faults, one in the same window
// or in the window before, and records the window. A memory region's first
// fault counts as following: a boot and a restore begin by reading forwards.
//
// Only such a fault plans the rest of its window and prefetches it; every
// other plans its page alone (planFault), unless its pager prefetches at
// random (prefetches). On GCE on 2026-10-04 a
// chain of dependent 4 KiB faults read from the cluster took 24 ms a hop when
// every fault prefetched its run, against 0.67 ms for a page alone: each
// 2,047-page prefetch is about 100 ms of processor, and the prefetches of the
// hops before took the processors the next hop's own read needed.
func (r *MemoryRegion) followsRecent(start uint64) bool {
	window := start / uint64(r.readAheadPages)
	history := &r.history
	history.mu.Lock()
	defer history.mu.Unlock()
	follows := history.count == 0
	for _, recent := range history.windows[:history.count] {
		if recent == window || recent+1 == window {
			follows = true
		}
	}
	history.windows[history.next] = window
	history.next = (history.next + 1) % recentFaults
	history.count = min(history.count+1, recentFaults)
	return follows
}
