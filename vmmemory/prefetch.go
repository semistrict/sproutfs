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
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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

// prefetchSendSeam runs as a fault splits a prefetch off, before it takes
// h.mu to look for reads under way and send its own. A test starts a read of
// the same root there.
var prefetchSendSeam func(start uint64)

// prefetchUnlockSeam runs as a prefetch that landed gives each of its pages
// up, once that page is given up and before the next is. A test puts a fault
// there, which finds the pages after it still held.
var prefetchUnlockSeam func(page uint64)

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

// prefetch is the rest of one fault's run, read behind the fault, as READ
// requests on the identity roots of its pages that it answers by supplying
// them, which is Zircon's PrefetchRange with a pager that reads ahead. Which
// faults prefetch, the bound on prefetches in flight, the bulk class of their
// reads and mapping what landed into the region that asked are the pager's.
type prefetch struct {
	region *MemoryRegion
	// start and end are the window, and pages the pages it reads, in page
	// order, each with its identity and the free slot reserved for it.
	start, end uint64
	pages      []prefetchPage
	// requests are the READ requests it sent, one for each run of its pages in
	// one root, which it answers when it finishes: every fault waiting on one
	// of its pages waits on one of them. Guarded by Host.mu.
	requests []prefetchRequest
	cancel   context.CancelCauseFunc
	// reading is set from the split until the read has ended, and holding
	// until every slot is settled: given back, or holding a page that landed,
	// idle. Until then a slot is neither free nor a page, and an allocation
	// short of one waits for it rather than evict (cancelPrefetchesLocked).
	// cancelled marks one an allocation has cancelled already, and finished
	// one whose read has ended (finish). All are guarded by Host.mu.
	reading, holding, cancelled, finished bool
	// ctx is what the prefetch runs under: the values of the context of the
	// fault that split it off, a task of its own in a controlled run, and the
	// prefetch mark; cancel ends it.
	ctx context.Context
}

// prefetchRequest is one READ request a prefetch sent, and the range it
// asks for, which a supply may have resolved before the prefetch answers it.
type prefetchRequest struct {
	root           *identityRoot
	request        *zirconvm.PageRequest
	offset, length uint64
}

// readingIn answers, under the host lock, whether a prefetch is reading a page
// of the window [start, end): it asks each identity root of the window once,
// where its pages begin, for the requests outstanding in the window.
type readingIn struct {
	host       *Host
	start, end uint64
	asked      bool
	root       rootKey
	ranges     []zirconvm.RequestRange
}

func (h *Host) readingIn(start, end uint64) readingIn {
	return readingIn{host: h, start: start, end: end}
}

// of reports whether a read is under way of the page key names, which is in
// the window: a request of its root is outstanding over it. What it reports
// may be out of date by the time the caller acts on it.
func (in *readingIn) of(key pageKey) bool {
	in.host.mu.Lock()
	defer in.host.mu.Unlock()
	return in.ofLocked(key)
}

// ofLocked is of with h.mu held.
func (in *readingIn) ofLocked(key pageKey) bool {
	h := in.host
	ps := h.pageSize
	if root := rootOf(key); !in.asked || root != in.root {
		in.asked, in.root, in.ranges = true, root, in.ranges[:0]
		if found := h.roots[root]; found != nil {
			in.ranges = found.reads.source.AppendOutstanding(in.ranges, zirconvm.ReadRequest, in.start*ps, in.end*ps)
		}
	}
	offset := key.id.Page * ps
	for _, r := range in.ranges {
		if offset >= r.Offset && offset-r.Offset < r.Len {
			return true
		}
	}
	return false
}

// splitPrefetch takes every reservation of the window but the faulting page's
// out of the plan, which is left to read the faulting page alone. The pages
// that can land as clean shared pages, which into names the file of
// (planRest), become a prefetch, which the caller starts; every other
// reservation goes back, and its page is left to its own fault. It returns nil
// where nothing is prefetched.
func (p *plan) splitPrefetch(ctx context.Context, index uint64, into []*arenaFile) *prefetch {
	r := p.region
	h := r.host
	var pages []prefetchPage
	var back []fileSlot
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		at := p.reserved[i]
		if page == index || at.slot < 0 {
			continue
		}
		p.reserved[i], p.fresh[i] = fileSlot{slot: -1}, false
		if key, _ := p.identity(page); into[i] == at.file {
			pages = append(pages, prefetchPage{page: page, key: key, at: at})
		} else {
			back = append(back, at)
		}
	}
	if len(pages) == 0 && len(back) == 0 {
		return nil
	}
	refused := sim.Buggify(ctx, buggifyPrefetchRefused, 0.2)
	reading := r.host.readingIn(p.start, p.end)
	stale := sim.Bug(ctx, "pager-filter-a-prefetch-unlocked")
	if stale {
		// The bug reads the reads under way before the seam, with h.mu not
		// held, and sends against that copy after it.
		for _, page := range pages {
			reading.of(page.key)
		}
	}
	if prefetchSendSeam != nil {
		prefetchSendSeam(p.start)
	}
	// In a controlled run another task may go on here, between the plan and
	// the send: a fault of another region of the same root, say.
	if err := sim.Admit(ctx, "vmmemory/prefetch-send"); err != nil {
		for _, page := range pages {
			back = append(back, page.at)
		}
		h.mu.Lock()
		for _, at := range back {
			h.putFree(at)
		}
		h.signal()
		h.mu.Unlock()
		return nil
	}
	h.mu.Lock()
	defer func() {
		for _, at := range back {
			h.putFree(at)
		}
		h.signal()
		h.mu.Unlock()
	}()
	if len(pages) > 0 && (refused || h.prefetching >= h.cfg.PrefetchRuns) {
		h.stats.PrefetchRefused++
		sim.Probe(ctx, ProbePrefetchRefused)
		for _, page := range pages {
			back = append(back, page.at)
		}
		return nil
	}
	// The reads under way are read under the same hold of h.mu that sends
	// the prefetch's: every READ of a root is sent under it, so none can
	// start between the look and the send and cover a page twice. A prefetch
	// of another region forked from the same root is one.
	if !stale {
		reading.asked = false
	}
	kept := pages[:0]
	for _, page := range pages {
		// A read reached this identity between the plan and here: it is
		// that read's to bring in.
		if reading.ofLocked(page.key) {
			back = append(back, page.at)
			continue
		}
		kept = append(kept, page)
	}
	if len(kept) == 0 {
		return nil
	}
	pf := &prefetch{region: r, start: p.start, end: p.end, pages: kept, reading: true, holding: true}
	pf.sendLocked()
	pf.ctx = sim.WithTask(context.WithoutCancel(ctx), fmt.Sprintf("prefetch-%d", p.start))
	pf.ctx, pf.cancel = context.WithCancelCause(checkpoint.WithPrefetch(pf.ctx))
	r.host.prefetches[pf] = struct{}{}
	h.prefetching++
	h.prefetchRunning++
	r.prefetchRunning++
	h.stats.Prefetches++
	return pf
}

// sendLocked sends the prefetch's READ requests, one to the root of each run
// of its pages in one root. Caller holds h.mu.
func (pf *prefetch) sendLocked() {
	h := pf.region.host
	ps := h.pageSize
	for at := 0; at < len(pf.pages); {
		first := pf.pages[at]
		key := rootOf(first.key)
		run := 1
		for at+run < len(pf.pages) && pf.pages[at+run].page == first.page+uint64(run) &&
			rootOf(pf.pages[at+run].key) == key {
			run++
		}
		root := h.rootLocked(key)
		request := h.newRequest()
		_ = root.reads.source.GetPages(first.key.id.Page*ps, uint64(run)*ps, request)
		if !root.reads.proxy.Holds(request) || zirconvm.RequestLen(request) != uint64(run)*ps {
			panic("vmmemory: a prefetch's request met another")
		}
		pf.requests = append(pf.requests, prefetchRequest{root: root, request: request,
			offset: first.key.id.Page * ps, length: uint64(run) * ps})
		at += run
	}
}

// answerLocked resolves the prefetch's requests: supplied, or failed where
// err says its read failed. Either wakes every read waiting on one. A page a
// prefetch did not land is then its fault's to read. Caller holds h.mu.
func (pf *prefetch) answerLocked(err error) {
	h := pf.region.host
	for _, sent := range pf.requests {
		if err != nil {
			sent.root.reads.source.OnPagesFailed(sent.offset, sent.length, zirconvm.ErrIO)
		} else {
			sent.root.reads.source.OnPagesSupplied(sent.offset, sent.length)
		}
		h.requests.Put(sent.request)
	}
	pf.requests = nil
}

// waiter is a READ request waiting on a prefetch's.
type waiter struct {
	host    *Host
	request *zirconvm.PageRequest
}

// waiter is a READ request waiting on the prefetch's first, nil once the
// prefetch has finished.
func (pf *prefetch) waiter() *waiter {
	h := pf.region.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(pf.requests) == 0 {
		return nil
	}
	sent := pf.requests[0]
	request := h.newRequest()
	_ = sent.root.reads.source.GetPages(pf.pages[0].key.id.Page*h.pageSize, h.pageSize, request)
	if sent.root.reads.proxy.Holds(request) {
		panic("vmmemory: a fault's request to wait on a prefetch was sent")
	}
	return &waiter{host: h, request: request}
}

// wait waits for the prefetch's request to be answered and gives the
// waiting request back.
func (w *waiter) wait(ctx context.Context) error {
	status := w.request.Wait(ctx)
	w.host.requests.Put(w.request)
	if status != nil && !zirconvm.IsValidInternalFailureCode(status) {
		return context.Cause(ctx)
	}
	return nil
}

// begin runs the prefetch on a goroutine of its own.
func (pf *prefetch) begin() { go pf.run(pf.ctx) }

func (pf *prefetch) run(ctx context.Context) {
	r := pf.region
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
	pf.finish(nil)
	if len(landed) > 0 {
		r.mapPrefetched(ctx, pf, landed)
	}
}

// finish is prefetch.finish.
func (pf *prefetch) finish(err error) {
	h := pf.region.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if pf.finished {
		return
	}
	pf.finished = true
	pf.answerLocked(err)
	h.prefetching--
	h.signal()
}

// settle is prefetch.settle.
func (pf *prefetch) settle() {
	h := pf.region.host
	h.mu.Lock()
	pf.holding = false
	delete(pf.region.host.prefetches, pf)
	h.signal()
	h.mu.Unlock()
}

// land reads the prefetch's pages with one backing read and supplies each run
// of them to its root, where they are idle until a region maps them. It
// reports the pages that landed; every slot it did not fill goes back.
func (pf *prefetch) land(ctx context.Context) []prefetchPage {
	defer pf.settle()
	r := pf.region
	h := r.host
	ps := h.pageSize
	first, last := pf.pages[0].page, pf.pages[len(pf.pages)-1].page+1
	wanted := make([]bool, last-first)
	for _, page := range pf.pages {
		wanted[page.page-first] = true
	}
	buffer := h.takeWindow(last - first)
	defer h.putWindow(buffer)
	data := *buffer
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
		pf.finish(err)
		if prefetchSettleSeam != nil {
			prefetchSettleSeam()
		}
		pf.drop(ctx, pf.pages)
		return nil
	}
	if prefetchSettleSeam != nil {
		prefetchSettleSeam()
	}
	h.mu.Lock()
	h.stats.Loads++
	h.stats.LoadedPages += uint64(len(pf.pages))
	h.mu.Unlock()
	// Each page's frame is made in its slot, and every run of consecutive
	// pages of one root that has one is one supply. A page whose slot could
	// not be filled is left to its fault: newFrame gave its slot back.
	frames := make([]*zirconvm.VmPage, len(pf.pages))
	for at, page := range pf.pages {
		if offset := page.page - first; offset < uint64(len(unpublished)) && unpublished[offset] {
			// A migration's source holds this page after all: its bytes are
			// the guest's own, not the identity's, and only a fault may take
			// them, as the region's dirty state.
			sim.Probe(ctx, ProbePrefetchHeld)
			pf.drop(ctx, pf.pages[at:at+1])
			continue
		}
		offset := (page.page - first) * ps
		frame, err := r.host.newFrame(ctx, page.at, data[offset:offset+ps], r.kind)
		if err != nil {
			h.mu.Lock()
			h.stats.PrefetchDropped++
			h.mu.Unlock()
			continue
		}
		frames[at] = frame
	}
	var landed []prefetchPage
	for at := 0; at < len(pf.pages); {
		if frames[at] == nil {
			at++
			continue
		}
		page := pf.pages[at]
		root := rootOf(page.key)
		run := 1
		for at+run < len(pf.pages) && frames[at+run] != nil && pf.pages[at+run].page == page.page+uint64(run) &&
			rootOf(pf.pages[at+run].key) == root {
			run++
		}
		landed = append(landed, pf.landRun(ctx, pf.pages[at:at+run], frames[at:at+run])...)
		at += run
	}
	if len(landed) > 0 {
		sim.Probe(ctx, ProbePrefetchLanded)
	}
	return landed
}

// landRun supplies one run of the prefetch's pages, consecutive and of one
// root, each in its frame. A page another read supplied first stays, and the
// prefetch's copy goes back.
func (pf *prefetch) landRun(ctx context.Context, pages []prefetchPage, frames []*zirconvm.VmPage) []prefetchPage {
	r := pf.region
	h := r.host
	ps := h.pageSize
	root := r.host.root(rootOf(pages[0].key))
	if err := r.host.supply(ctx, root.object, pages[0].key.id.Page, frames); err != nil {
		slog.WarnContext(ctx, "vmmemory: supplying a prefetch's pages failed", "pages", len(frames), "error", err)
		for _, frame := range frames {
			frameOf(frame).mu.Unlock()
		}
		return nil
	}
	var landed []prefetchPage
	lock := root.pages.Lock()
	lock.Lock()
	h.mu.Lock()
	for k, frame := range frames {
		found := root.pages.PageLocked(pages[k].key.id.Page * ps)
		if found != frame {
			// Another read made this identity resident first, and that page
			// is the one every region maps; the supply gave this one back.
			sim.Probe(ctx, ProbePrefetchDuplicate)
			h.stats.PrefetchDropped++
			continue
		}
		// It lands idle, as a page every region has stopped mapping is.
		r.host.adoptLocked(found)
		r.host.node.PageQueues().MoveToReclaimDontNeed(found)
		h.stats.PrefetchedPages++
		landed = append(landed, pages[k])
	}
	h.mu.Unlock()
	lock.Unlock()
	// The frames were held from their making; a landed one is idle in its
	// root now, and one the supply gave back is gone.
	for k, frame := range frames {
		frameOf(frame).mu.Unlock()
		if prefetchUnlockSeam != nil {
			prefetchUnlockSeam(pages[k].page)
		}
	}
	return landed
}

// drop gives the slots of pages that did not land back, as prefetch.drop
// does.
func (pf *prefetch) drop(ctx context.Context, pages []prefetchPage) {
	if len(pages) == 0 {
		return
	}
	h := pf.region.host
	punched := make([]bool, len(pages))
	var failed error
	for at, page := range pages {
		err := page.at.file.Release(context.WithoutCancel(ctx), page.at.slot)
		punched[at] = err == nil
		failed = errors.Join(failed, err)
	}
	h.mu.Lock()
	for at, page := range pages {
		if punched[at] {
			h.putFree(page.at)
		}
	}
	if failed != nil {
		h.err = errors.Join(h.err, failed)
	}
	h.stats.PrefetchDropped += uint64(len(pages))
	h.signal()
	h.mu.Unlock()
	if failed != nil {
		slog.WarnContext(ctx, "vmmemory: giving back a prefetch's slots failed", "error", failed)
	}
}

// mapPrefetched maps the pages a prefetch landed into the region whose fault
// asked for them, as MemoryRegion.mapPrefetched does.
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
	mapped := uint64(0)
	for _, page := range landed {
		if plan.bindLanded(page) {
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

// bindLanded takes one page a prefetch landed into the plan, where the region
// has nothing at that page yet and its identity is still the one that
// landed, and its root still holds it.
func (p *plan) bindLanded(page prefetchPage) bool {
	i := page.page - p.start
	if p.pages[i] != nil || p.zeros[i] || !p.eligible(page.page) || p.region.mapped(page.page) {
		return false
	}
	if key, named := p.identity(page.page); !named || key != page.key {
		return false
	}
	r := p.region
	root := r.host.root(rootOf(page.key))
	lock := root.pages.Lock()
	lock.Lock()
	defer lock.Unlock()
	found := root.pages.PageLocked(page.key.id.Page * r.host.pageSize)
	if found == nil || !r.reachable(found) || !p.hold(found) {
		return false
	}
	r.host.node.PageQueues().MarkAccessed(found)
	r.bind(page.page, found)
	p.pages[i], p.fresh[i] = found, true
	return true
}

// cancelPrefetchesLocked cancels every prefetch still reading, whose slots an
// allocation that would otherwise evict a page a guest maps takes instead. It
// reports whether any prefetch still holds slots, reading, cancelled, or
// giving them back or landing its pages in them: the allocation waits for
// those slots to come back free or as idle pages. Caller holds h.mu.
func (h *Host) cancelPrefetchesLocked(ctx context.Context) bool {
	holding := false
	for pf := range h.prefetches {
		if !pf.holding {
			continue
		}
		holding = true
		if pf.reading && !pf.cancelled {
			pf.cancelled = true
			pf.cancel(errMemoryPressure)
			h.stats.PrefetchCancelled++
			sim.Probe(ctx, ProbePrefetchCancelled)
		}
	}
	return holding
}

// cancelPrefetches cancels this region's prefetches and waits until none of
// them runs, which a detach does before it takes anything away.
func (r *MemoryRegion) cancelPrefetches(ctx context.Context) error {
	h := r.host
	for {
		h.mu.Lock()
		for pf := range r.host.prefetches {
			if pf.region == r {
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
