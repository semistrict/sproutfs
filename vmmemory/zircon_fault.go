package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// fault is MemoryRegion.Fault under the zircon core. A read's lookup is the
// region layer's lookup cursor, Zircon's RequireReadPage, which falls through
// to the identity root of the page; a page no object holds yet is a READ
// request, which the fault answers by reading the page into a frame and
// supplying it. A store makes a page of the layer Dirty: a copy of the page
// it maps, at the offset the placement rule gives it, or fresh zeros
// (zircon_store.go). Which pages a fault reads and in what order is the
// current core's, and moves as it is (faultfirst.go): its page first, the rest
// of its window prefetched behind it, a fault at random alone, and a
// post-copy stream's whole run at once.
func (z *zirconRegion) fault(ctx context.Context, index uint64, write bool) error {
	r := z.region
	if index >= uint64(r.pageCount) {
		return ErrRange
	}
	// Whatever this fault admitted to the dirty budget is measured against
	// the high-water mark here, where it holds nothing.
	defer r.host.askAtHighWater()
	for range faultAttempts {
		// A store that needs a page of its own takes its dirty reservation
		// before any region, page or I/O resource, as in the current core.
		var spill reservation
		if write && z.needsPrivatePage(index) {
			taken, err := r.host.takeSpill(ctx, r)
			if err != nil {
				return err
			}
			spill = taken
		}
		retry, err := z.faultOnce(ctx, index, write, &spill)
		if !spill.none() {
			r.host.releaseSpill(spill)
		}
		if !retry {
			return err
		}
	}
	return ErrContended
}

// faultOnce serves one attempt and reports whether it must be retried with a
// dirty reservation it did not hold. It consumes *spill by setting it to
// noReservation.
func (z *zirconRegion) faultOnce(ctx context.Context, index uint64, write bool, spill *reservation) (retry bool, err error) {
	r := z.region
	started := r.host.clock.Now()
	if err := r.live.RLock(ctx); err != nil {
		return false, err
	}
	defer r.live.RUnlock()
	// The window's stripe comes before the memory region, as in the current
	// core: a fault gives the region up across its backing read and takes it
	// again.
	if err := r.stripe(index).Lock(ctx); err != nil {
		return false, err
	}
	defer r.stripe(index).Unlock()
	if err := r.lockPageAccess(ctx, index, write); err != nil {
		return false, err
	}
	defer func() {
		if !errors.Is(err, errMemoryRegionDropped) {
			r.mu.RUnlock()
		}
	}()
	h := r.host
	if err := h.beginIO(ctx); err != nil {
		return false, err
	}
	defer h.endIO()
	h.mu.Lock()
	h.stats.Faults++
	h.mu.Unlock()
	defer func() { h.faultLatency.Observe(h.clock.Since(started)) }()
	if !write || z.writable(index) {
		return false, z.load(ctx, index)
	}
	if spill.none() {
		// The page needed one after all: it stopped being the region's own
		// between the decision and the region lock.
		return true, nil
	}
	return z.store(ctx, index, spill)
}

// load maps the faulting page and as much of its window as the fault reads,
// starting again from the top whenever it waited for a read, as Zircon's
// page fault does after a page request.
func (z *zirconRegion) load(ctx context.Context, index uint64) error {
	for range loadAttempts {
		resolved, err := z.loadOnce(ctx, index)
		if err != nil || resolved {
			return err
		}
	}
	return ErrContended
}

// loadOnce reports whether the faulting page ended mapped and resolved.
func (z *zirconRegion) loadOnce(ctx context.Context, index uint64) (bool, error) {
	r := z.region
	z.mu.Lock()
	b, zeroRun := z.lookupLocked(index)
	z.mu.Unlock()
	if b == nil && zeroRun {
		// A compressed zero run maps it: its access completes read-only.
		if err := r.resolvePages(ctx, index, 1, false); err != nil {
			return false, r.fail(err)
		}
		return true, nil
	}
	if b != nil {
		// The page is held while its access is completed, so no eviction
		// takes its mapping away in between.
		page, err := z.host.lockedPage(ctx, b)
		if err != nil {
			return false, err
		}
		resolved, err := z.loadBound(ctx, b, page)
		if page != nil {
			z.host.unlockPage(page)
		}
		if err != nil || resolved {
			return resolved, err
		}
		if page == nil && z.private(b) {
			// The region's own state, spilled: it has no source but its
			// reservation.
			return z.refault(ctx, b)
		}
	}
	plan, err := z.planFault(ctx, index, index)
	if err != nil {
		return false, err
	}
	defer plan.unlock()
	if waiter := plan.inFlight(ctx, index); waiter != nil {
		// A prefetch is reading this page already; the fault plans again
		// once it has landed or failed. The plan holds nothing yet.
		return false, z.awaitRead(ctx, waiter)
	}
	again, err := plan.takeFaulting(ctx, index)
	if err != nil || again {
		return false, err
	}
	if err := plan.read(ctx, index); err != nil {
		return false, err
	}
	return plan.install(ctx)
}

// loadBound completes a fault on a page the region maps already: a refault
// after a failed ACK, or a page a prefetch or a populate mapped since the
// trap, whose trapped access still has to complete, writable where the page
// is the region's own Dirty page. It reports false where the region maps
// nothing there. Caller holds the page's lock, page being b's.
func (z *zirconRegion) loadBound(ctx context.Context, b *zbinding, page *zirconvm.VmPage) (bool, error) {
	r := z.region
	z.mu.Lock()
	mapped := b.mapped || b.inZeroRun
	writable := b.writable()
	z.mu.Unlock()
	if !mapped {
		return false, nil
	}
	if err := r.resolvePages(ctx, b.index, 1, writable && page != nil); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}

// private reports a page that is the region's own state, its own dirty page
// or the checkpoint's it shares, which no root holds.
func (z *zirconRegion) private(b *zbinding) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	return b.dirty || b.checkpoint != nil
}

// refault is loadOnce's spilled private page, reloaded alone, as the current
// core's: its bytes are read from the reservation that holds them, which is
// the checkpoint's copy's where the page shares it, into a page of the
// region's private file, which goes back into the layer Dirty, or
// AwaitingClean where it shares the copy. A page the checkpoint holds maps
// read-only, so the next store copies away from it. It reports whether the
// fault was resolved: a seal or a retire taken while the region was given up
// for the slot is decided again from the top.
func (z *zirconRegion) refault(ctx context.Context, b *zbinding) (bool, error) {
	r := z.region
	h := r.host
	ps := h.pageSize
	z.mu.Lock()
	dirty, held := b.dirty, b.checkpoint
	spill := b.spill
	if held != nil {
		spill = held.spill
	}
	z.mu.Unlock()
	data := make([]byte, ps)
	if err := h.readSpill(ctx, spill, data); err != nil {
		return false, err
	}
	h.mu.Lock()
	h.stats.SpillRefaults++
	h.mu.Unlock()
	at, err := z.reclaimPrivate(ctx, b.index)
	if err != nil {
		return false, err
	}
	z.mu.Lock()
	same := b.dirty == dirty && b.checkpoint == held
	z.mu.Unlock()
	h.mu.Lock()
	same = same && b.page == nil && (held == nil || held.page == nil)
	h.mu.Unlock()
	if !same {
		return false, h.abandonSlots(ctx, at, 1, nil)
	}
	frame, err := z.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		return false, err
	}
	defer z.host.unlockPage(frame)
	frameOf(frame).layer = z
	if err := z.supplyDirty(ctx, b.index, []*zirconvm.VmPage{frame}); err != nil {
		return false, err
	}
	if held != nil {
		// It shares the checkpoint's copy, whose writeback has begun.
		if err := z.layer.WritebackBegin(b.index*ps, ps, false); err != nil {
			return false, err
		}
	}
	h.mu.Lock()
	z.host.aliasLocked(b, frame)
	if held != nil {
		z.host.aliasLocked(held, frame)
	}
	h.mu.Unlock()
	z.host.node.PageQueues().MarkAccessed(frame)
	writable := held == nil
	z.setMapped(b.index, b.index+1, true)
	if err := r.mapPages(ctx, r.runAt(b.index, frameOf(frame).fileSlot, 1), writable); err != nil {
		return false, r.mappingFailed(err, func() { z.setMapped(b.index, b.index+1, false) })
	}
	if err := r.resolvePages(ctx, b.index, 1, writable); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}

// planFault is MemoryRegion.planFault over the zircon core, with the same
// policy and the same guards.
func (z *zirconRegion) planFault(ctx context.Context, index, fault uint64) (*zplan, error) {
	r := z.region
	start, end := r.window(index)
	var p *zplan
	var err error
	how := readAlone
	switch {
	case runFirst(ctx):
		how = readRun
		p, err = z.plan(ctx, start, end, fault)
	case r.prefetches(ctx, start):
		how = readFirst
		p, err = z.planPage(ctx, start, end, fault, index)
	case sim.Bug(ctx, "pager-plan-the-window-at-random"):
		// The bug plans the whole window of a fault at random.
		p, err = z.plan(ctx, start, end, fault)
	default:
		p, err = z.plan(ctx, index, index+1, fault)
	}
	if err != nil {
		return nil, err
	}
	p.reading = how
	return p, nil
}

// read brings the faulting page in as the plan says the fault reads it.
func (p *zplan) read(ctx context.Context, index uint64) error {
	switch p.reading {
	case readFirst:
		return p.readFirst(ctx, index)
	case readRun:
		if err := p.takeRun(ctx, index); err != nil {
			return err
		}
		return p.loadReserved(ctx)
	default:
		return p.readAlone(ctx, index)
	}
}

// readAlone is windowPlan.readAlone over the zircon core.
func (p *zplan) readAlone(ctx context.Context, index uint64) error {
	p.survey(index, false)
	if p.reserved[index-p.start].slot >= 0 {
		h := p.z.region.host
		h.mu.Lock()
		h.stats.PrefetchRandom++
		h.mu.Unlock()
		sim.Probe(ctx, ProbePrefetchRandom)
	}
	return p.loadReserved(ctx)
}

// readFirst is windowPlan.readFirst over the zircon core: the faulting page's
// read starts first, the rest of the window is planned while it runs, and
// the rest is prefetched behind it.
func (p *zplan) readFirst(ctx context.Context, index uint64) error {
	z := p.z
	r := z.region
	if sim.Bug(ctx, "pager-plan-the-window-first") {
		// The bug plans the whole window before the faulting page's read
		// starts.
		into, err := p.planRest(ctx, index)
		if err != nil {
			return err
		}
		read := p.beginFaulting(ctx, index)
		if pf := p.splitPrefetch(ctx, index, into); pf != nil {
			pf.begin()
		}
		return read.land(ctx, p)
	}
	read := p.beginFaulting(ctx, index)
	into, err := p.planRest(ctx, index)
	if err != nil {
		read.abandon()
		return err
	}
	if pf := p.splitPrefetch(ctx, index, into); pf != nil {
		pf.begin()
		if sim.Bug(ctx, "pager-fault-waits-for-its-prefetch") {
			// The bug installs the page only once the rest of its run is in:
			// it waits on the prefetch's requests.
			if waiter := pf.waiter(); waiter != nil {
				if err := r.withoutMemoryRegion(ctx, func() error {
					return waiter.wait(ctx)
				}); err != nil {
					read.abandon()
					return err
				}
			}
		}
	}
	return read.land(ctx, p)
}

// takeFaulting takes the faulting page into the plan before any other, by
// the layer's lookup of it: a page an object holds is bound, a hole is a
// zero, and a missing page is a READ request on the region's own source,
// which this plan answers and takes a slot for, as windowPlan.takeFaulting
// does. It reports again where the lookup met a request of a root that a
// read answered meanwhile, and the fault must look again.
func (p *zplan) takeFaulting(ctx context.Context, index uint64) (again bool, err error) {
	z := p.z
	r := z.region
	i := index - p.start
	id, named := p.identity(index)
	if named && id.zero() && !z.holdsOwn(index) {
		// A hole the region has not stored into reads as zeros.
		p.observeZeros()
		p.zeros[i], p.fresh[i] = true, true
		return false, nil
	}
	found, request, err := z.lookup(ctx, index, p.locationsOf(index))
	if errors.Is(err, errPageBusy) {
		// Something holds the page, an eviction most likely: the fault waits
		// for it with nothing held, and looks again.
		return true, r.withoutMemoryRegion(ctx, func() error {
			if err := z.host.lockPage(ctx, found); err != nil {
				return err
			}
			z.host.unlockPage(found)
			return nil
		})
	}
	if err != nil {
		return false, err
	}
	if found != nil {
		p.locked = append(p.locked, found)
		p.pages[i], p.fresh[i] = found, true
		// The region's own Dirty page is mapped writable: the guest may store
		// into it where it is.
		p.writable[i] = frameOf(found).layer == z && z.writable(index)
		if named && frameOf(found).layer != z {
			h := r.host
			h.mu.Lock()
			h.stats.IdentityHits++
			h.mu.Unlock()
		}
		return false, nil
	}
	if request.source != r.reads {
		// The root the resolver named gave the page up between the
		// resolver's look and the lookup's, which then asked the root. Its
		// request is answered at once, so nothing waits on it, and the fault
		// looks again: the page is the region's own to read now.
		request.answer(nil)
		return true, nil
	}
	p.request = request
	if p.own(index) {
		at, err := z.reclaimOwn(ctx, index)
		if err != nil {
			return false, err
		}
		p.reserve(index, at)
		return false, nil
	}
	if p.reading == readRun {
		p.reserveAround(index)
	} else {
		p.reserveProvisional(index)
	}
	if p.reserved[i].slot >= 0 {
		return false, nil
	}
	at, err := z.reclaim(ctx, p.fileOf(index))
	if err != nil {
		return false, err
	}
	p.reserve(index, at)
	return false, nil
}

// planRest is windowPlan.planRest over the zircon core.
func (p *zplan) planRest(ctx context.Context, index uint64) ([]*arenaFile, error) {
	if err := p.locateWindow(ctx); err != nil {
		return nil, err
	}
	found := p.survey(index, true)
	p.keepProvisional(found.into)
	return found.into, p.reserveRuns(ctx, index, found.into)
}

// takeRun is windowPlan.takeRun over the zircon core: the run read first
// takes its resident pages and slots for the rest, and places in the
// region's own file for the pages read there.
func (p *zplan) takeRun(ctx context.Context, index uint64) error {
	found := p.survey(index, false)
	if err := p.reserveRuns(ctx, index, found.into); err != nil {
		return err
	}
	p.reserveOwn()
	return nil
}

// errPageBusy reports a page a lookup found whose lock something else holds.
var errPageBusy = errors.New("vmmemory: a page's lock is held")

// zrequest is the READ request a lookup sent for a page no object holds,
// which the fault that made it answers once its read has been supplied, or
// fails where it could not read it.
type zrequest struct {
	host   *zirconHost
	multi  *zirconvm.MultiPageRequest
	source *requestSource
	// offset and length are the range it asks for in its source's object,
	// and answered marks it supplied or failed.
	offset, length uint64
	answered       bool
}

// answer resolves the request: supplied where err is nil, failed otherwise.
// Every read waiting on it looks again, and the request goes back to be made
// again. Answering it twice does nothing.
func (q *zrequest) answer(err error) {
	if q == nil || q.answered {
		return
	}
	q.answered = true
	if err != nil {
		q.source.source.OnPagesFailed(q.offset, q.length, zirconvm.ErrIO)
	} else {
		q.source.source.OnPagesSupplied(q.offset, q.length)
	}
	// It is resolved: the wait takes its completion at once, and leaves it
	// ready for another lookup.
	_ = q.multi.Wait(context.Background())
	q.host.multis.Put(q.multi)
	q.multi = nil
}

// fail fails the request where the fault did not answer it.
func (q *zrequest) fail() { q.answer(zirconvm.ErrIO) }

// lookup is the layer's lookup of one page, Zircon's RequireReadPage over a
// lookup cursor, with the resolver naming the root of each page loc located
// that a root holds. A page an object holds is reported, bound to the region
// while its owner's lock is still held, so that no idle drop takes it first:
// mapped by the fault's install, or for a store's own page, kept unmapped to
// copy from. A missing page is reported as the READ request the lookup sent,
// which only the faults of its window make on the region's own source, one at
// a time. A zero is reported as neither.
func (z *zirconRegion) lookup(ctx context.Context, page uint64, loc *locations) (*zirconvm.VmPage, *zrequest, error) {
	ps := z.region.host.pageSize
	lock := z.pages.Lock()
	lock.Lock()
	defer lock.Unlock()
	z.resolver.located = loc
	defer func() { z.resolver.located = nil }()
	cursor, err := z.pages.GetLookupCursorLocked(zirconvm.CowRange{Offset: page * ps, Len: ps})
	if err != nil {
		return nil, nil, err
	}
	defer cursor.Release()
	multi := z.host.multis.Get().(*zirconvm.MultiPageRequest)
	// A read changes no mapping and frees no page, so it defers nothing: no
	// DeferredOps to finish.
	result, err := cursor.RequireReadPage(ctx, 1, nil, multi)
	if !errors.Is(err, zirconvm.ErrShouldWait) {
		// No request was made, so the request goes back as it came.
		z.host.multis.Put(multi)
	}
	switch {
	case err == nil && result.Page == z.host.pmm.zero:
		return nil, nil, nil
	case err == nil:
		// The page is held from here until the fault's command lands: an
		// eviction takes no page whose lock is held. One an eviction holds is
		// looked up again once it is done.
		if !frameOf(result.Page).mu.TryLock() {
			return result.Page, nil, errPageBusy
		}
		z.bind(page, result.Page)
		return result.Page, nil, nil
	case errors.Is(err, zirconvm.ErrShouldWait):
		return nil, z.requestOf(multi), nil
	}
	return nil, nil, err
}

// requestOf is the request a lookup sent: on the region's own source, where
// only the faults of one window ask, one at a time, so it is always sent; or
// on a root's, where a supply may have answered it already.
func (z *zirconRegion) requestOf(multi *zirconvm.MultiPageRequest) *zrequest {
	request := multi.ReadRequest()
	source := zirconvm.RequestSource(request)
	r := z.region
	rs := r.reads
	if source != rs.source {
		rs = nil
		h := r.host
		h.mu.Lock()
		for _, root := range z.host.roots {
			if root.reads.source == source {
				rs = root.reads
				break
			}
		}
		h.mu.Unlock()
		if rs == nil {
			panic("vmmemory: a lookup asked a page source no root and no region has")
		}
	}
	if !rs.proxy.Holds(request) {
		if rs == r.reads {
			panic("vmmemory: a fault's read request met another read of its memory region's pages")
		}
		// It waits on a read of the root's, which the caller looks again
		// after: withdrawn, it waits on nothing, and goes back.
		multi.CancelRequests()
		z.host.multis.Put(multi)
		return &zrequest{source: rs, answered: true}
	}
	return &zrequest{host: z.host, multi: multi, source: rs, offset: zirconvm.RequestOffset(request),
		length: zirconvm.RequestLen(request)}
}

// inFlight is a READ request of the faulting page waiting on the prefetch
// reading that page, nil where none is: windowPlan.inFlight over the zircon
// core's roots. The in-tree bug that reads such a page again reports none.
// Every request of a root is sent and answered under h.mu but a supply's,
// which answers under the root's lock: a request this sends that its root's
// proxy then holds met no read, because a supply answered it between the
// look and the send, and it is answered at once.
func (p *zplan) inFlight(ctx context.Context, page uint64) *zwaiter {
	key, named := p.identity(page)
	if !named || key.zero() || sim.Bug(ctx, "pager-read-in-flight-again") {
		return nil
	}
	z := p.z.host
	h := z.host
	ps := h.pageSize
	h.mu.Lock()
	defer h.mu.Unlock()
	root := z.roots[rootOf(key)]
	if root == nil {
		return nil
	}
	offset := key.id.Page * ps
	var reading [1]zirconvm.RequestRange
	if len(root.reads.source.AppendOutstanding(reading[:0], zirconvm.ReadRequest, offset, offset+ps)) == 0 {
		return nil
	}
	request := h.newRequest()
	_ = root.reads.source.GetPages(offset, ps, request)
	if root.reads.proxy.Holds(request) {
		root.reads.source.OnPagesSupplied(offset, ps)
		h.requests.Put(request)
		return nil
	}
	return &zwaiter{host: h, request: request}
}

// awaitRead waits, with the region given up as a backing read gives it up,
// for the prefetch reading the faulting page to land or drop it. The fault
// then plans its window again from the top.
func (z *zirconRegion) awaitRead(ctx context.Context, waiter *zwaiter) error {
	r := z.region
	h := r.host
	h.mu.Lock()
	h.stats.PrefetchWaits++
	h.mu.Unlock()
	sim.Probe(ctx, ProbePrefetchWaited)
	return r.withoutMemoryRegion(ctx, func() error {
		if err := waiter.wait(ctx); err != nil {
			return err
		}
		// Every fault waiting on this prefetch is released at once. In a
		// controlled run they go on one at a time, in the order it chooses.
		return sim.Admit(ctx, "vmmemory/prefetch-wait")
	})
}

// zfaultRead is faultRead over the zircon core: the faulting page's backing
// read, under way on a task of its own while the fault plans the rest of its
// window. The fault supplies what it read, which answers the page's request.
type zfaultRead struct {
	host   *Host
	page   uint64
	buffer *[]byte
	err    error
	done   chan struct{}
	cancel context.CancelCauseFunc
}

// beginFaulting starts the faulting page's read where the plan reserved a
// slot for it, nil where the page needs none.
func (p *zplan) beginFaulting(ctx context.Context, index uint64) *zfaultRead {
	if p.reserved[index-p.start].slot < 0 {
		return nil
	}
	r := p.z.region
	h := r.host
	readCtx, cancel := context.WithCancelCause(sim.WithTask(ctx, fmt.Sprintf("fault-read-%d", index)))
	read := &zfaultRead{host: h, page: index, buffer: h.takeWindow(1), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(read.done)
		if read.err = sim.Admit(readCtx, "vmmemory/fault-read"); read.err == nil {
			_, read.err = r.readRun(readCtx, index, []bool{true}, *read.buffer, &h.loadLatency)
		}
	}()
	return read
}

// land waits for the read, with the region given up, and supplies the page.
func (read *zfaultRead) land(ctx context.Context, p *zplan) error {
	if read == nil {
		return nil
	}
	defer read.release()
	r := p.z.region
	err := r.withoutMemoryRegion(ctx, func() error {
		select {
		case <-read.done:
		case <-ctx.Done():
			read.cancel(context.Cause(ctx))
			return context.Cause(ctx)
		}
		// In a controlled run the fault goes on when the run chooses.
		if err := sim.Admit(ctx, "vmmemory/fault-read-landed"); err != nil {
			return err
		}
		return read.err
	})
	if err != nil {
		return err
	}
	return p.publishRead(ctx, read.page, []bool{true}, *read.buffer)
}

// abandon ends a read the fault no longer wants.
func (read *zfaultRead) abandon() {
	if read == nil {
		return
	}
	read.cancel(errReadAbandoned)
	read.release()
}

// release waits for the read's goroutine to end and gives its buffer back.
func (read *zfaultRead) release() {
	read.cancel(nil)
	<-read.done
	read.host.putWindow(read.buffer)
}

// reclaim takes one slot of f with the region given up, as the current core's
// reclaim does, evicting where the arena is full.
func (z *zirconRegion) reclaim(ctx context.Context, f *arenaFile) (fileSlot, error) {
	return z.region.reclaimWith(ctx, func() (fileSlot, error) { return z.host.allocate(ctx, z.region, f, nil) })
}

// reclaimOwn takes a place of page index in the region's own file, with the
// region given up.
func (z *zirconRegion) reclaimOwn(ctx context.Context, index uint64) (fileSlot, error) {
	r := z.region
	h := r.host
	return r.reclaimWith(ctx, func() (fileSlot, error) {
		for _, at := range r.ownPlaces(index, true) {
			h.mu.Lock()
			_, held := at.file.leases[at.slot]
			h.mu.Unlock()
			if !held {
				return z.host.allocate(ctx, r, at.file, func() int { return h.takeOwnLocked(r, index, at).slot })
			}
		}
		return fileSlot{}, fmt.Errorf("%w: both places of page %d of a memory region hold a page", ErrCapacity, index)
	})
}

// allocate returns one slot of f for a page of r, or the slot place takes
// where place is not nil: Host.allocate, whose evictor takes its victims by
// this core's reclaimStep. Taking a victim where a free slot would do is legal
// and merely wasteful (evictPastAFreeSlot), and it is how an arena that is not
// full reaches the eviction paths at all.
func (z *zirconHost) allocate(ctx context.Context, r *MemoryRegion, f *arenaFile, place func() int) (fileSlot, error) {
	return z.host.allocate(ctx, r, f, place, evictPastAFreeSlot(ctx))
}

// makeRoom gives up idle pages until want slots of f are free, or no idle
// page is left, as Host.makeRoom does.
func (z *zirconHost) makeRoom(_ context.Context, f *arenaFile, want int) error {
	h := z.host
	for {
		h.mu.Lock()
		free := h.freeLocked(f)
		err := h.err
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if free >= want || !z.takeIdle() {
			return nil
		}
	}
}

// dropIdle gives up every idle page, as Host.DropIdle does.
func (z *zirconHost) dropIdle(context.Context) (int, error) {
	dropped := 0
	for z.takeIdle() {
		dropped++
	}
	h := z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return dropped, h.err
}
