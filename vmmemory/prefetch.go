package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
// Only a fault that follows one of its memory region's recent faults, in its
// own run or the one before, prefetches; one at random reads its page alone
// (followsRecent). A prefetch costs processors as a fault's read does, and a
// guest reading at random gains nothing from it.
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
//     already reading: it waits for that read rather than reading the page a
//     second time (awaitPrefetch). So a guest reading forwards still reads its
//     memory in runs, one read for the faulting page and one for the rest.
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
	// done is closed once every page has landed or been dropped, which is
	// what a fault waiting for one of them waits on.
	done   chan struct{}
	cancel context.CancelCauseFunc
	// reading is set from the split until the pages have landed: the slots
	// are reserved and not yet pages. cancelled marks one an allocation has
	// cancelled already. Both are guarded by Host.mu.
	reading, cancelled bool
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

// splitPrefetch takes every reservation of the window but the faulting page's
// out of the plan, which is left to read the faulting page alone. The pages
// that can land as clean shared pages become a prefetch, which the caller
// starts; every other reservation goes back, and its page is left to its own
// fault. It returns nil where nothing is prefetched.
//
// A stream's fault keeps its whole run, and so does every fault under the
// in-tree bug that puts the run back in front of the faulting page.
func (p *windowPlan) splitPrefetch(ctx context.Context, index uint64) *prefetch {
	if streaming(ctx) || sim.Bug(ctx, "pager-read-the-run-first") {
		return nil
	}
	r := p.memoryRegion
	h := r.host
	sequential := r.followsRecent(p.start)
	var pages []prefetchPage
	var back []fileSlot
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		at := p.reserved[i]
		if page == index || at.slot < 0 {
			continue
		}
		p.reserved[i], p.fresh[i] = fileSlot{slot: -1}, false
		key, named := p.identity(page)
		if named && !key.zero() && at.file == p.fileOf(page) && !p.unpublished(page) {
			pages = append(pages, prefetchPage{page: page, key: key, at: at})
		} else {
			back = append(back, at)
		}
	}
	if len(pages) == 0 && len(back) == 0 {
		return nil
	}
	refused := sim.Buggify(ctx, buggifyPrefetchRefused, 0.2)
	h.mu.Lock()
	defer func() {
		for _, at := range back {
			h.putFree(at)
		}
		h.signal()
		h.mu.Unlock()
	}()
	if len(pages) > 0 && !sequential && !sim.Bug(ctx, "pager-prefetch-every-fault") {
		// A fault that follows none of its memory region's recent faults is
		// read alone: its neighbours are worth reading only to a guest that
		// reads forwards, and a prefetch nothing uses takes processors from
		// the faults that follow.
		h.stats.PrefetchRandom++
		sim.Probe(ctx, ProbePrefetchRandom)
		for _, page := range pages {
			back = append(back, page.at)
		}
		return nil
	}
	if len(pages) > 0 && (refused || h.prefetching >= h.cfg.PrefetchRuns) {
		h.stats.PrefetchRefused++
		sim.Probe(ctx, ProbePrefetchRefused)
		for _, page := range pages {
			back = append(back, page.at)
		}
		return nil
	}
	kept := pages[:0]
	for _, page := range pages {
		// Another prefetch reached this identity between the plan and here:
		// it is that one's to read.
		if h.inflight[page.key] != nil {
			back = append(back, page.at)
			continue
		}
		kept = append(kept, page)
	}
	if len(kept) == 0 {
		return nil
	}
	pf := &prefetch{memoryRegion: r, start: p.start, end: p.end, pages: kept, done: make(chan struct{}),
		reading: true}
	for _, page := range kept {
		h.inflight[page.key] = pf
	}
	h.prefetchNumber++
	// The context is made before the prefetch is registered: an allocation
	// or a detach may cancel it from the moment it is.
	pf.ctx = sim.WithTask(context.WithoutCancel(ctx), fmt.Sprintf("prefetch-%d", h.prefetchNumber))
	pf.ctx, pf.cancel = context.WithCancelCause(checkpoint.WithPrefetch(pf.ctx))
	h.prefetches[pf] = struct{}{}
	h.prefetching++
	h.prefetchRunning++
	r.prefetchRunning++
	h.stats.Prefetches++
	return pf
}

// inFlight is the prefetch reading the faulting page, nil where none is. The
// in-tree bug that reads such a page again reports none.
func (p *windowPlan) inFlight(ctx context.Context, page uint64) *prefetch {
	key, named := p.identity(page)
	if !named || key.zero() || sim.Bug(ctx, "pager-read-in-flight-again") {
		return nil
	}
	h := p.memoryRegion.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[key]
}

// awaitPrefetch waits, with the memory region given up as a backing read
// gives it up, for the prefetch reading the faulting page to land or drop it.
// The fault then plans its window again from the top.
func (r *MemoryRegion) awaitPrefetch(ctx context.Context, pf *prefetch) error {
	h := r.host
	h.mu.Lock()
	h.stats.PrefetchWaits++
	h.mu.Unlock()
	sim.Probe(ctx, ProbePrefetchWaited)
	return r.withoutMemoryRegion(ctx, func() error {
		select {
		case <-pf.done:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		// Every fault waiting on this prefetch is released at once. In a
		// controlled run they go on one at a time, in the order it chooses.
		return sim.Admit(ctx, "vmmemory/prefetch-wait")
	})
}

// begin runs the prefetch on a goroutine of its own. It outlives the fault
// that split it off, so it keeps only the values of that fault's context; an
// allocation short of slots cancels it, and so does the memory region's
// detach.
func (pf *prefetch) begin() { go pf.run(pf.ctx) }

func (pf *prefetch) run(ctx context.Context) {
	r := pf.memoryRegion
	h := r.host
	defer func() {
		h.mu.Lock()
		r.prefetchRunning--
		h.prefetchRunning--
		h.signal()
		h.mu.Unlock()
		pf.cancel(nil)
	}()
	landed := pf.land(ctx)
	h.mu.Lock()
	for _, page := range pf.pages {
		if h.inflight[page.key] == pf {
			delete(h.inflight, page.key)
		}
	}
	delete(h.prefetches, pf)
	h.prefetching--
	h.signal()
	h.mu.Unlock()
	close(pf.done)
	if len(landed) > 0 {
		r.mapPrefetched(ctx, pf, landed)
	}
}

// land reads the prefetch's pages with one backing read and publishes each
// as a clean idle page under its identity. It reports the pages that landed;
// every slot it did not fill goes back.
func (pf *prefetch) land(ctx context.Context) []prefetchPage {
	r := pf.memoryRegion
	h := r.host
	ps := h.pageSize
	// The pages are in order, so the read covers the first to the last of
	// them, leaving out everything between them that is not theirs.
	first, last := pf.pages[0].page, pf.pages[len(pf.pages)-1].page+1
	wanted := make([]bool, last-first)
	for _, page := range pf.pages {
		wanted[page.page-first] = true
	}
	buffer := h.takeWindow(last - first)
	defer h.putWindow(buffer)
	data := *buffer
	// In a controlled run the prefetch begins its read when the run chooses,
	// not beside whatever the fault that started it does next.
	err := sim.Admit(ctx, "vmmemory/prefetch-start")
	if err == nil {
		err = sim.BuggifyDelay(ctx, buggifyPrefetchSlow, 0.5, 50*time.Millisecond)
	}
	var unpublished []bool
	if err == nil {
		unpublished, err = r.readRun(ctx, first, wanted, data, &h.prefetchLatency)
	}
	if err == nil && sim.Buggify(ctx, buggifyPrefetchFailed, 0.1) {
		err = errPrefetchFailed
	}
	if err == nil {
		// Its pages land when a controlled run chooses, not beside the faults
		// that are taking slots at the same moment.
		err = sim.Admit(ctx, "vmmemory/prefetch-land")
	}
	h.mu.Lock()
	pf.reading = false
	h.mu.Unlock()
	if err != nil {
		if context.Cause(ctx) == nil {
			slog.DebugContext(ctx, "vmmemory: a prefetch's read failed; its pages are left to their faults",
				"pages", len(pf.pages), "error", err)
		}
		pf.drop(ctx, pf.pages)
		return nil
	}
	h.mu.Lock()
	h.stats.Loads++
	h.stats.LoadedPages += uint64(len(pf.pages))
	h.mu.Unlock()
	var landed []prefetchPage
	for at, page := range pf.pages {
		if offset := page.page - first; offset < uint64(len(unpublished)) && unpublished[offset] {
			// A migration's source holds this page after all: its bytes are
			// the guest's own, not the identity's, and only a fault may take
			// them, as this memory region's dirty state.
			sim.Probe(ctx, ProbePrefetchHeld)
			pf.drop(ctx, pf.pages[at:at+1])
			continue
		}
		offset := page.page - first
		pg, err := h.create(ctx, page.at, data[offset*ps:(offset+1)*ps], page.key, false, r.kind)
		if err != nil {
			// create gave the slot back.
			h.mu.Lock()
			h.stats.PrefetchDropped++
			h.mu.Unlock()
			continue
		}
		h.mu.Lock()
		existing := h.clean[page.key]
		if existing == nil {
			h.clean[page.key] = pg
			h.cleanVersion++
			h.idleLocked(pg)
			h.stats.PrefetchedPages++
		}
		h.mu.Unlock()
		if existing != nil {
			// Another load made this identity resident first, and that page
			// is the one every memory region maps.
			sim.Probe(ctx, ProbePrefetchDuplicate)
			err := h.release(ctx, pg)
			h.unlock(pg)
			h.mu.Lock()
			h.stats.PrefetchDropped++
			h.mu.Unlock()
			if err != nil {
				slog.WarnContext(ctx, "vmmemory: giving back a prefetched page failed", "error", err)
			}
			continue
		}
		h.unlock(pg)
		landed = append(landed, page)
	}
	if len(landed) > 0 {
		sim.Probe(ctx, ProbePrefetchLanded)
	}
	return landed
}

// drop gives the slots of pages that did not land back.
func (pf *prefetch) drop(ctx context.Context, pages []prefetchPage) {
	h := pf.memoryRegion.host
	for _, page := range pages {
		if err := h.abandonSlots(ctx, page.at, 1, nil); err != nil {
			slog.WarnContext(ctx, "vmmemory: giving back a prefetch's slot failed", "error", err)
		}
	}
	h.mu.Lock()
	h.stats.PrefetchDropped += uint64(len(pages))
	h.mu.Unlock()
}

// mapPrefetched maps the pages a prefetch landed into the memory region whose
// fault asked for them, read-only and under their identities, as a fault's
// read-ahead used to map them. It takes the window's locks as a fault does,
// and maps only a page the guest still has nothing at, whose identity is still
// the one that landed. A memory region being detached is left alone: its
// pages stay idle.
func (r *MemoryRegion) mapPrefetched(ctx context.Context, pf *prefetch, landed []prefetchPage) {
	if !r.live.TryRLock() {
		return
	}
	defer r.live.RUnlock()
	stripe := r.stripe(pf.start)
	if err := stripe.Lock(ctx); err != nil {
		return
	}
	defer stripe.Unlock()
	if err := sim.Admit(ctx, "vmmemory/prefetch-map"); err != nil {
		return
	}
	if err := r.lockPageAccess(ctx, pf.start, false); err != nil {
		return
	}
	defer r.mu.RUnlock()
	plan, err := r.plan(ctx, pf.start, pf.end, pf.end)
	if err != nil {
		return
	}
	defer plan.unlock()
	for _, page := range landed {
		plan.bindLanded(ctx, page)
	}
	mapped := uint64(0)
	for _, pg := range plan.pages {
		if pg != nil {
			mapped++
		}
	}
	if mapped == 0 {
		return
	}
	if _, err := plan.install(ctx); err != nil {
		if !errors.Is(err, ErrMappingRefused) {
			slog.WarnContext(ctx, "vmmemory: mapping a prefetch's pages failed", "pages", mapped, "error", err)
		}
		return
	}
	h := r.host
	h.mu.Lock()
	h.stats.PrefetchMapped += mapped
	h.mu.Unlock()
	sim.Probe(ctx, ProbePrefetchMapped)
}

// bindLanded binds one page a prefetch landed to its resident, where the
// guest has nothing at that page yet and the page's identity is still the one
// that landed, and the resident is free and in a file this memory region may
// map. It never waits and never copies.
func (p *windowPlan) bindLanded(ctx context.Context, page prefetchPage) {
	i := page.page - p.start
	if p.pages[i] != nil || p.zeros[i] || !p.eligible(page.page) || p.memoryRegion.mapped(page.page) {
		return
	}
	if key, named := p.identity(page.page); !named || key != page.key {
		return
	}
	r := p.memoryRegion
	h := r.host
	h.mu.Lock()
	pg := h.clean[page.key]
	h.mu.Unlock()
	if pg == nil || p.locked[pg] || !pg.mu.TryLock() {
		return
	}
	h.mu.Lock()
	valid := h.clean[page.key] == pg && r.mapsLocked(pg.file)
	h.mu.Unlock()
	if !valid {
		h.unlock(pg)
		return
	}
	if found := h.probe.stable(ctx, h, pg, "bindLanded"); found != "" {
		panic(found)
	}
	h.bind(r.binding(page.page), pg)
	p.pages[i] = pg
	p.locked[pg] = true
	p.fresh[i] = true
}

// cancelPrefetchesLocked cancels every prefetch still reading, whose slots an
// allocation that would otherwise evict a page a guest maps takes instead. It
// reports whether any is still reading, cancelled now or before: the
// allocation waits for their slots to come back. Caller holds h.mu.
func (h *Host) cancelPrefetchesLocked(ctx context.Context) bool {
	reading := false
	for pf := range h.prefetches {
		if !pf.reading {
			continue
		}
		reading = true
		if !pf.cancelled {
			pf.cancelled = true
			pf.cancel(errMemoryPressure)
			h.stats.PrefetchCancelled++
			sim.Probe(ctx, ProbePrefetchCancelled)
		}
	}
	return reading
}

// cancelPrefetches cancels this memory region's prefetches and waits until
// none of them runs, which a detach does before it takes anything away.
func (r *MemoryRegion) cancelPrefetches(ctx context.Context) error {
	h := r.host
	for {
		h.mu.Lock()
		for pf := range h.prefetches {
			if pf.memoryRegion == r {
				pf.cancel(errDetaching)
			}
		}
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

// SettlePrefetches returns once none of this memory region's prefetches is
// running: each has landed or dropped its pages and mapped what it could.
// Nothing waits on a prefetch; this is what a test of a guest that reads only
// once its neighbours are in waits on.
func (r *MemoryRegion) SettlePrefetches(ctx context.Context) error {
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
// Only such a fault prefetches the rest of its run. On GCE on 2026-10-04 a
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
