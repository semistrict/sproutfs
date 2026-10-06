package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// zprefetch is prefetch over the zircon core: the rest of one fault's run,
// read behind the fault, as READ requests on the identity roots of its pages
// that it answers by supplying them, which is Zircon's PrefetchRange with a
// pager that reads ahead. Which faults prefetch, the bound on prefetches in
// flight, the bulk class of their reads and mapping what landed into the
// region that asked stay the pager's (prefetch.go), and so do its sites,
// probes and controlled points, by the same names.
type zprefetch struct {
	region     *MemoryRegion
	start, end uint64
	pages      []prefetchPage
	// requests are the READ requests it sent, one for each run of its pages
	// in one root. Guarded by Host.mu.
	requests []zprefetchRequest
	cancel   context.CancelCauseFunc
	// reading, holding, cancelled and finished are prefetch's. Guarded by
	// Host.mu.
	reading, holding, cancelled, finished bool
	ctx                                   context.Context
}

// zprefetchRequest is one READ request a prefetch sent, and the range it
// asks for, which a supply may have resolved before the prefetch answers it.
type zprefetchRequest struct {
	root           *identityRoot
	request        *zirconvm.PageRequest
	offset, length uint64
}

// zreadingIn is readingIn over the zircon core's roots.
type zreadingIn struct {
	host       *Host
	start, end uint64
	asked      bool
	root       rootKey
	ranges     []zirconvm.RequestRange
}

func (h *Host) readingIn(start, end uint64) zreadingIn {
	return zreadingIn{host: h, start: start, end: end}
}

// of reports whether a read is under way of the page key names, which is in
// the window: a request of its root is outstanding over it.
func (in *zreadingIn) of(key pageKey) bool {
	h := in.host
	ps := h.pageSize
	if root := rootOf(key); !in.asked || root != in.root {
		in.asked, in.root, in.ranges = true, root, in.ranges[:0]
		h.mu.Lock()
		found := h.roots[root]
		h.mu.Unlock()
		if found != nil {
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

// splitPrefetch is windowPlan.splitPrefetch over the zircon core.
func (p *zplan) splitPrefetch(ctx context.Context, index uint64, into []*arenaFile) *zprefetch {
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
	h.mu.Unlock()
	kept := pages[:0]
	for _, page := range pages {
		// A read reached this identity between the plan and here: it is
		// that read's to bring in.
		if reading.of(page.key) {
			back = append(back, page.at)
			continue
		}
		kept = append(kept, page)
	}
	h.mu.Lock()
	if len(kept) == 0 {
		return nil
	}
	pf := &zprefetch{region: r, start: p.start, end: p.end, pages: kept, reading: true, holding: true}
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
func (pf *zprefetch) sendLocked() {
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
		pf.requests = append(pf.requests, zprefetchRequest{root: root, request: request,
			offset: first.key.id.Page * ps, length: uint64(run) * ps})
		at += run
	}
}

// answerLocked resolves the prefetch's requests: supplied, or failed where
// err says its read failed. Either wakes every read waiting on one. A page a
// prefetch did not land is then its fault's to read. Caller holds h.mu.
func (pf *zprefetch) answerLocked(err error) {
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

// zwaiter is a READ request waiting on a prefetch's.
type zwaiter struct {
	host    *Host
	request *zirconvm.PageRequest
}

// waiter is a READ request waiting on the prefetch's first, nil once the
// prefetch has finished.
func (pf *zprefetch) waiter() *zwaiter {
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
	return &zwaiter{host: h, request: request}
}

// wait waits for the prefetch's request to be answered and gives the
// waiting request back.
func (w *zwaiter) wait(ctx context.Context) error {
	status := w.request.Wait(ctx)
	w.host.requests.Put(w.request)
	if status != nil && !zirconvm.IsValidInternalFailureCode(status) {
		return context.Cause(ctx)
	}
	return nil
}

// begin runs the prefetch on a goroutine of its own.
func (pf *zprefetch) begin() { go pf.run(pf.ctx) }

func (pf *zprefetch) run(ctx context.Context) {
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
func (pf *zprefetch) finish(err error) {
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
func (pf *zprefetch) settle() {
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
func (pf *zprefetch) land(ctx context.Context) []prefetchPage {
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
func (pf *zprefetch) landRun(ctx context.Context, pages []prefetchPage, frames []*zirconvm.VmPage) []prefetchPage {
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
	for _, frame := range frames {
		frameOf(frame).mu.Unlock()
	}
	return landed
}

// drop gives the slots of pages that did not land back, as prefetch.drop
// does.
func (pf *zprefetch) drop(ctx context.Context, pages []prefetchPage) {
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
func (r *MemoryRegion) mapPrefetched(ctx context.Context, pf *zprefetch, landed []prefetchPage) {
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
func (p *zplan) bindLanded(page prefetchPage) bool {
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

// cancelPrefetchesLocked is Host.cancelPrefetchesLocked over the zircon
// core's prefetches. Caller holds h.mu.
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
