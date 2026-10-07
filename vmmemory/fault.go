package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Fault orders operations only within this memory region's read-ahead window. Shared
// page transitions additionally take that page's lock. Different windows and
// volumes can load, spill, and flush concurrently, bounded by ConcurrentIO and
// the resident/dirty budgets. A read fault loads and maps as much of its window
// as free slots allow; pages of the window that are already resident under the
// same stored identity are mapped without loading anything.
//
// A read's lookup is the region layer's lookup cursor, Zircon's
// RequireReadPage, which falls through to the identity root of the page. A
// page no object holds yet is a READ request, which the fault answers by
// reading the page into a frame and supplying it. A store makes a page of the
// layer Dirty: a copy of the page it maps, at the offset the placement rule
// gives it, or fresh zeros (store.go). Which pages a fault reads and in what
// order is faultfirst.go's: its page first, the rest of its window prefetched
// behind it, a fault at random alone, and a post-copy stream's whole run at
// once.
func (r *MemoryRegion) Fault(ctx context.Context, index uint64, write bool) error {
	if index >= uint64(r.pageCount) {
		return ErrRange
	}
	// Whatever this fault admitted to the dirty budget is measured against
	// the high-water mark here, where it holds nothing.
	defer r.host.askAtHighWater()
	reserve := false
	decisions, lost := 0, 0
	for decisions < faultAttempts && lost < loadAttempts {
		// A store that needs a page of its own takes its dirty reservation
		// before any region, page or I/O resource, and so does a read of a
		// page only another host holds, which the load makes the region's own
		// dirty state.
		var spill reservation
		if reserve || (write && r.needsPrivatePage(index)) {
			taken, err := r.host.takeSpill(ctx, r)
			if err != nil {
				return err
			}
			spill = taken
		}
		retry, err := r.faultOnce(ctx, index, write, &spill)
		if !spill.none() {
			r.host.releaseSpill(spill)
		}
		switch {
		case errors.Is(err, errUnpublishedReservation):
			reserve, retry, err = true, true, nil
		case errors.Is(err, errLostRead) && !sim.Bug(ctx, "pager-count-a-lost-read-as-a-decision"):
			lost++
			continue
		}
		if !retry {
			return err
		}
		decisions++
	}
	return fmt.Errorf("%w: page %d decided whether it needs a page of its own %d times and lost its read %d",
		ErrContended, index, decisions, lost)
}

// errUnpublishedReservation reports a fault whose window turned out to hold
// pages no checkpoint has, which the load takes as this memory region's dirty state.
// It never leaves the package: the fault releases everything it holds, takes a
// dirty reservation through the waiting path, and tries again.
var errUnpublishedReservation = errors.New("managed-memory fault needs a dirty reservation")

// faultAttempts bounds how often a fault re-decides whether it needs a private
// page. Only a seal taken between that decision and the memory region lock can force
// another attempt, so one repetition is enough in every observed case. A store
// that lost the read of the page it copies to another reader (errLostRead)
// tries again without a new decision, bounded as a load is (loadAttempts).
const faultAttempts = 8

// errLostRead is a store that lost the read of the page it copies: another
// fault or a prefetch was reading the page, and the store waited for it, or
// the page's root gave it up before the store could bind it. Whoever won made
// progress, so the store reads again, as a load does after a lost race, and
// its decision to make a page of its own stands. Under many forks of one
// checkpoint reading at once, a store loses several of these running; counted
// as decisions, eight ended the guest's session (the unscheduled soak).
var errLostRead = errors.New("managed-memory store lost the read of its page")

// around narrows [first, last) to at most n pages that still hold index,
// preferring the pages after it: access tends to continue forward.
func around(index, first, last uint64, n int) (uint64, uint64) {
	if last-first <= uint64(n) {
		return first, last
	}
	start := max(first, min(index, last-uint64(n)))
	return start, start + uint64(n)
}

// loadAttempts bounds how often a fault retries after losing a publication race
// for its identity. Each retry begins by waiting for the winner while holding
// no other resident lock, so one is enough in every observed case.
const loadAttempts = 64

// end is the window end that names no faulting page: a plan for it resolves
// nothing.
func (r *MemoryRegion) end(index uint64) uint64 {
	_, end := r.window(index)
	return end
}

// faultOnce serves one attempt and reports whether it must be retried with a
// dirty reservation it did not hold. It consumes *spill by setting it to
// noReservation.
func (r *MemoryRegion) faultOnce(ctx context.Context, index uint64, write bool, spill *reservation) (retry bool, err error) {
	started := r.host.clock.Now()
	if err := lockAdmitted(ctx, "vmmemory/live", r.live.TryRLock, r.live.RLock, r.live.RUnlock); err != nil {
		return false, err
	}
	defer r.live.RUnlock()
	// The window's stripe comes before the memory region: a fault gives the
	// region up across its backing read and takes it again.
	stripe := r.stripe(index)
	if err := lockAdmitted(ctx, "vmmemory/stripe", stripe.TryLock, stripe.Lock, stripe.Unlock); err != nil {
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
	if !write || r.writable(index) {
		return false, r.load(ctx, index, spill)
	}
	if r.journalProtected(index) {
		// A protect trap on a page a journal capture write-protected: it is
		// the region's own already, so nothing is copied.
		return r.unprotectForStore(ctx, index)
	}
	if spill.none() {
		// The page needed one after all: it stopped being the region's own
		// between the decision and the region lock.
		return true, nil
	}
	return r.store(ctx, index, spill)
}

// load maps the faulting page and as much of its window as the fault reads,
// starting again from the top whenever it waited for a read, as Zircon's
// page fault does after a page request.
func (r *MemoryRegion) load(ctx context.Context, index uint64, spill *reservation) error {
	for range loadAttempts {
		resolved, err := r.loadOnce(ctx, index, spill)
		if err != nil || resolved {
			return err
		}
	}
	return fmt.Errorf("%w: page %d was loaded %d times", ErrContended, index, loadAttempts)
}

// loadOnce reports whether the faulting page ended mapped and resolved.
func (r *MemoryRegion) loadOnce(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	r.bindingsMu.Lock()
	b, zeroRun := r.lookupLocked(index)
	r.bindingsMu.Unlock()
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
		page, err := r.host.lockedPage(ctx, b)
		if err != nil {
			return false, err
		}
		resolved, err := r.loadBound(ctx, b, page)
		if page != nil {
			r.host.unlockPage(page)
		}
		if err != nil || resolved {
			return resolved, err
		}
		if page == nil && r.isPrivate(b) {
			// The region's own state, spilled: it has no source but its
			// reservation.
			return r.refault(ctx, b)
		}
	}
	plan, err := r.planFault(ctx, index, index)
	if err != nil {
		return false, err
	}
	defer plan.unlock()
	if waiter := plan.inFlight(ctx, index); waiter != nil {
		// A prefetch is reading this page already; the fault plans again
		// once it has landed or failed. The plan holds nothing yet.
		return false, r.awaitRead(ctx, waiter, index)
	}
	// No read of the page was under way when inFlight looked, and the
	// lookup sends the fault's own under a later hold. A prefetch another
	// region sends in between is not waited for: the lookup asks this
	// region's own source for a page its root does not hold yet, so the
	// fault reads the page beside that prefetch, and whichever supply lands
	// second gives its copy back (ProbePrefetchDuplicate). In a controlled
	// run another task may go on here, a fault of another fork say.
	if err := sim.Admit(ctx, "vmmemory/fault-lookup"); err != nil {
		return false, err
	}
	if plan.unpublished(index) && spill.none() {
		// The extents say another host still holds this page, so the load
		// takes it as the region's dirty state, under a reservation the
		// waiting path takes with nothing held.
		return false, errUnpublishedReservation
	}
	plan.spill = spill
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
func (r *MemoryRegion) loadBound(ctx context.Context, b *binding, page *zirconvm.VmPage) (bool, error) {
	r.bindingsMu.Lock()
	mapped := b.mapped || b.inZeroRun
	writable := b.writable()
	r.bindingsMu.Unlock()
	if !mapped {
		return false, nil
	}
	if err := r.resolvePages(ctx, b.index, 1, writable && page != nil); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}

// isPrivate reports a page that is the region's own state, its own dirty page
// or the checkpoint's it shares, which no root holds.
func (r *MemoryRegion) isPrivate(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.dirty || b.checkpoint != nil
}

// refault is loadOnce's spilled private page, reloaded alone: its bytes are
// read from the reservation that holds them, which is the checkpoint's copy's
// where the page shares it, into a page of the region's private file, which
// goes back into the layer Dirty, or AwaitingClean where it shares the copy. A
// page the checkpoint holds maps read-only, so the next store copies away from
// it. It reports whether the fault was resolved: a seal or a retire taken
// while the region was given up for the slot is decided again from the top.
func (r *MemoryRegion) refault(ctx context.Context, b *binding) (bool, error) {
	h := r.host
	ps := h.pageSize
	// Whose the page is and where its bytes are is read with the region held
	// shared and the window's stripe held, so no seal, retire, capture or
	// store of this page changes it until the region is given up for the slot
	// below. The bytes are read from that reservation meanwhile.
	r.bindingsMu.Lock()
	dirty, held, protected := b.dirty, b.checkpoint, b.protected
	spill := b.spill
	if held != nil {
		spill = held.spill
	}
	r.bindingsMu.Unlock()
	data := make([]byte, ps)
	if err := h.readSpill(ctx, spill, data); err != nil {
		return false, err
	}
	// Counted under a hold of its own, which reads nothing.
	h.mu.Lock()
	h.stats.SpillRefaults++
	h.mu.Unlock()
	// The region is given up for the slot. A seal, a retire or an unseal
	// taken meanwhile changes whose the page is, which is decided again from
	// the top; a seal and its unseal both taken hand the page back the same
	// reservation, so the bytes read stand. A capture taken meanwhile changes
	// only whether the page is protected, which is read again: a page it
	// journaled is mapped read-only, so the guest's next store traps and makes
	// it unjournaled again.
	at, err := r.reclaimPrivate(ctx, b.index)
	if err != nil {
		return false, err
	}
	r.bindingsMu.Lock()
	same := b.dirty == dirty && b.checkpoint == held
	if !sim.Bug(ctx, "pager-refault-maps-by-a-stale-protection") {
		protected = b.protected
	}
	r.bindingsMu.Unlock()
	// b and held are seen to hold no page here, and are given the new frame
	// under a later hold of h.mu, once it is filled and supplied. Nothing
	// gives either a page in between: the stripe keeps out every fault,
	// store, give-back and prefetch landing of the page, the region held
	// shared every seal, settle and capture, and an eviction only takes a
	// page away.
	h.mu.Lock()
	same = same && b.page == nil && (held == nil || held.page == nil)
	h.mu.Unlock()
	if !same {
		return false, h.abandonSlots(ctx, at, 1, nil)
	}
	frame, err := r.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		return false, err
	}
	defer r.host.unlockPage(frame)
	frameOf(frame).layer = r
	if err := r.supplyDirty(ctx, b.index, []*zirconvm.VmPage{frame}); err != nil {
		return false, err
	}
	if held != nil {
		// It shares the checkpoint's copy, whose writeback has begun.
		if err := r.layer.WritebackBegin(b.index*ps, ps, false); err != nil {
			return false, err
		}
	}
	h.mu.Lock()
	r.host.aliasLocked(b, frame)
	if held != nil {
		r.host.aliasLocked(held, frame)
	}
	h.mu.Unlock()
	// The refaulted page holds the guest's own current bytes: the newest
	// generation of them, not a step back.
	h.probe.granted(b, frameOf(frame), nil)
	r.host.node.PageQueues().MarkAccessed(frame)
	// A page a journal capture protected stays protected: the store that
	// traps on it is what makes it unjournaled again.
	writable := held == nil && !protected
	r.setMapped(b.index, b.index+1, true)
	if err := r.mapPages(ctx, r.runAt(b.index, frameOf(frame).fileSlot, 1), writable); err != nil {
		return false, r.mappingFailed(err, func() { r.setMapped(b.index, b.index+1, false) })
	}
	if err := r.resolvePages(ctx, b.index, 1, writable); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}

// planFault plans the window of the page index for the faulting page fault, or
// the window's end for none, as far as the fault will read it: the whole
// window, located at once, for a fault that reads its run first; the window,
// with the faulting page located alone, for one that reads its page first; and
// the faulting page alone for one at random.
func (r *MemoryRegion) planFault(ctx context.Context, index, fault uint64) (*plan, error) {
	start, end := r.window(index)
	var p *plan
	var err error
	how := readAlone
	switch {
	case runFirst(ctx):
		how = readRun
		p, err = r.plan(ctx, start, end, fault)
	case r.prefetches(ctx, start):
		how = readFirst
		p, err = r.planPage(ctx, start, end, fault, index)
	case sim.Bug(ctx, "pager-plan-the-window-at-random"):
		// The bug plans the whole window of a fault at random.
		p, err = r.plan(ctx, start, end, fault)
	default:
		p, err = r.plan(ctx, index, index+1, fault)
	}
	if err != nil {
		return nil, err
	}
	p.reading = how
	return p, nil
}

// read brings the faulting page in as the plan says the fault reads it.
func (p *plan) read(ctx context.Context, index uint64) error {
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

// readAlone reads the faulting page of a fault at random, where it needs
// reading. The read is the fault's own, as a run read first is: nothing is
// planned beside it.
func (p *plan) readAlone(ctx context.Context, index uint64) error {
	p.survey(ctx, index, false)
	if p.reserved[index-p.start].slot >= 0 {
		h := p.region.host
		h.mu.Lock()
		h.stats.PrefetchRandom++
		h.mu.Unlock()
		sim.Probe(ctx, ProbePrefetchRandom)
	}
	return p.loadReserved(ctx)
}

// readFirst serves the faulting page of a fault that reads its page first: the
// page's read starts first, the rest of the window is planned while it runs,
// and the rest is prefetched behind it.
func (p *plan) readFirst(ctx context.Context, index uint64) error {
	r := p.region
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

// takeFaulting takes the faulting page into the plan before any other, by the
// layer's lookup of it: a page an object holds is bound, a hole is a zero, and
// a missing page is a READ request on the region's own source, which this plan
// answers and takes a slot for. It reports again where the page it found was
// held by something else, which it waited for, and the fault must look
// again.
func (p *plan) takeFaulting(ctx context.Context, index uint64) (again bool, err error) {
	r := p.region
	i := index - p.start
	id, named := p.identity(index)
	if named && id.zero() && !r.holdsOwn(index) {
		// A hole the region has not stored into reads as zeros.
		p.observeZeros()
		p.zeros[i], p.fresh[i] = true, true
		return false, nil
	}
	found, request, err := r.lookup(ctx, index, p.locationsOf(index))
	if errors.Is(err, errPageBusy) {
		// Something holds the page, an eviction most likely: the fault waits
		// for it with nothing held, and looks again.
		return true, r.withoutMemoryRegion(ctx, func() error {
			if err := r.host.lockPage(ctx, found); err != nil {
				return err
			}
			r.host.unlockPage(found)
			return nil
		})
	}
	if errors.Is(err, errUnreachable) {
		// A page another region's private file holds, or a fork point lends:
		// it is moved or copied where this region's process may map it. Where
		// it cannot be, the fault reads its own copy.
		reached, err := r.reach(ctx, found, id)
		if err != nil {
			return false, err
		}
		if reached != nil {
			r.bind(index, reached)
		}
		found = reached
	} else if err != nil {
		return false, err
	}
	if found != nil {
		p.locked = append(p.locked, found)
		p.pages[i], p.fresh[i] = found, true
		// The region's own Dirty page is mapped writable: the guest may store
		// into it where it is.
		p.writable[i] = frameOf(found).layer == r && r.writable(index)
		if named && frameOf(found).layer != r {
			h := r.host
			h.mu.Lock()
			h.stats.IdentityHits++
			h.mu.Unlock()
		}
		return false, nil
	}
	p.request = request
	// A slot is taken with the region given up and the request outstanding
	// on the region's own source. Only the faults of this window ask that
	// source, and this one holds the window's stripe, so no other request
	// meets it; and a seal, retire or capture taken meanwhile changes only
	// pages the region holds, which this one is not.
	if p.own(index) {
		at, err := r.reclaimOwn(ctx, index)
		if err != nil {
			return false, err
		}
		p.reserve(index, at)
		return false, nil
	}
	if p.reading == readRun {
		p.reserveAround(ctx, index)
	} else {
		p.reserveProvisional(index)
	}
	if p.reserved[i].slot >= 0 {
		return false, nil
	}
	at, err := r.reclaim(ctx, p.fileOf(index))
	if err != nil {
		return false, err
	}
	p.reserve(index, at)
	return false, nil
}

// planRest locates the whole window of a fault that reads its page first, in
// one lookup, once the faulting page is in the plan, and takes free slots,
// never an eviction, for the pages the prefetch reads. It reports the file
// each of those pages goes in (survey).
func (p *plan) planRest(ctx context.Context, index uint64) ([]*arenaFile, error) {
	if err := p.locateWindow(ctx); err != nil {
		return nil, err
	}
	found := p.survey(ctx, index, true)
	p.keepProvisional(found.into)
	return found.into, p.reserveRuns(ctx, index, found.into)
}

// takeRun takes the rest of a run read first into the plan: its resident pages
// and slots for the rest, and places in the region's own file for the pages
// read there.
func (p *plan) takeRun(ctx context.Context, index uint64) error {
	found := p.survey(ctx, index, false)
	if err := p.reserveRuns(ctx, index, found.into); err != nil {
		return err
	}
	p.reserveOwn()
	return nil
}

// errUnreachable reports a page a lookup found, and holds, that this region's
// process may not map where it is.
var errUnreachable = errors.New("vmmemory: a page is in a file the region's process does not hold")

// errPageBusy reports a page a lookup found whose lock something else holds.
var errPageBusy = errors.New("vmmemory: a page's lock is held")

// readRequest is the READ request a lookup sent for a page no object holds,
// which the fault that made it answers once its read has been supplied, or
// fails where it could not read it.
type readRequest struct {
	host   *Host
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
func (q *readRequest) answer(err error) {
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
func (q *readRequest) fail() { q.answer(zirconvm.ErrIO) }

// lookup is the layer's lookup of one page, Zircon's RequireReadPage over a
// lookup cursor, with the resolver naming the root of each page loc located
// that a root holds. A page an object holds is reported, bound to the region
// while its owner's lock is still held, so that no idle drop takes it first:
// mapped by the fault's install, or for a store's own page, kept unmapped to
// copy from. A missing page is reported as the READ request the lookup sent,
// which only the faults of its window make on the region's own source, one at
// a time. A zero is reported as neither.
func (r *MemoryRegion) lookup(ctx context.Context, page uint64, loc *locations) (*zirconvm.VmPage, *readRequest, error) {
	ps := r.host.pageSize
	lock := r.pages.Lock()
	lock.Lock()
	defer lock.Unlock()
	r.resolver.located, r.resolver.ctx = loc, ctx
	defer func() { r.resolver.located, r.resolver.ctx = nil, nil }()
	cursor, err := r.pages.GetLookupCursorLocked(zirconvm.CowRange{Offset: page * ps, Len: ps})
	if err != nil {
		return nil, nil, err
	}
	defer cursor.Release()
	multi := r.host.multis.Get().(*zirconvm.MultiPageRequest)
	// A read changes no mapping and frees no page, so it defers nothing: no
	// DeferredOps to finish.
	result, err := cursor.RequireReadPage(ctx, 1, nil, multi)
	if lookupSentSeam != nil && errors.Is(err, zirconvm.ErrShouldWait) {
		lookupSentSeam(page)
	}
	if !errors.Is(err, zirconvm.ErrShouldWait) {
		// No request was made, so the request goes back as it came.
		r.host.multis.Put(multi)
	}
	switch {
	case err == nil && result.Page == r.host.pmm.zero:
		return nil, nil, nil
	case err == nil:
		// The page is held from here until the fault's command lands: an
		// eviction takes no page whose lock is held. One an eviction holds is
		// looked up again once it is done.
		if !frameOf(result.Page).mu.TryLock() {
			return result.Page, nil, errPageBusy
		}
		if !r.reachable(result.Page) {
			// The caller reaches it first, with no object lock held.
			return result.Page, nil, errUnreachable
		}
		r.bind(page, result.Page)
		return result.Page, nil, nil
	case errors.Is(err, zirconvm.ErrShouldWait):
		return nil, r.requestOf(multi), nil
	}
	return nil, nil, err
}

// lookupSentSeam runs in a lookup that sent a READ request, before the request
// is looked at under h.mu. Production leaves it nil.
var lookupSentSeam func(page uint64)

// requestOf is the request a lookup sent, which is on the region's own
// source. A lookup goes down into a root only where the root still holds the
// page under the root's lock (rootResolver.Holds), so it never asks a root's
// source, and every READ of a root is sent under h.mu. Only the faults of one
// window ask the region's own source, one at a time, so the request is always
// sent.
func (r *MemoryRegion) requestOf(multi *zirconvm.MultiPageRequest) *readRequest {
	request := multi.ReadRequest()
	if zirconvm.RequestSource(request) != r.reads.source {
		panic("vmmemory: a lookup asked an identity root's page source")
	}
	if !r.reads.proxy.Holds(request) {
		panic("vmmemory: a fault's read request met another read of its memory region's pages")
	}
	return &readRequest{host: r.host, multi: multi, source: r.reads, offset: zirconvm.RequestOffset(request),
		length: zirconvm.RequestLen(request)}
}

// inFlight is a READ request of the faulting page waiting on the prefetch
// reading that page, nil where none is. The in-tree bug that reads such a page
// again reports none. Every request of a root is sent and answered under h.mu
// but a supply's, which answers under the root's lock: a request this sends
// that its root's proxy then holds met no read, because a supply answered it
// between the look and the send, and it is answered at once.
func (p *plan) inFlight(ctx context.Context, page uint64) *waiter {
	key, named := p.identity(page)
	if !named || key.zero() || sim.Bug(ctx, "pager-read-in-flight-again") {
		return nil
	}
	h := p.region.host
	ps := h.pageSize
	h.mu.Lock()
	defer h.mu.Unlock()
	root := h.roots[rootOf(key)]
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
	return &waiter{host: h, request: request}
}

// awaitRead waits, with the region given up as a backing read gives it up,
// for the prefetch reading the faulting page to land or drop it, and then for
// every prefetch of the region whose run holds page to give its pages up. The
// fault then plans its window again from the top, and takes the whole run
// that landed: a plan made while the prefetch still held its pages would
// leave them to faults of their own.
func (r *MemoryRegion) awaitRead(ctx context.Context, waiter *waiter, page uint64) error {
	h := r.host
	h.mu.Lock()
	h.stats.PrefetchWaits++
	h.mu.Unlock()
	sim.Probe(ctx, ProbePrefetchWaited)
	return r.withoutMemoryRegion(ctx, func() error {
		if err := waiter.wait(ctx); err != nil {
			return err
		}
		if !sim.Bug(ctx, "pager-replan-before-the-prefetch-lets-go") {
			if err := r.awaitLanded(ctx, page); err != nil {
				return err
			}
		}
		// Every fault waiting on this prefetch is released at once. In a
		// controlled run they go on one at a time, in the order it chooses.
		return sim.Admit(ctx, "vmmemory/prefetch-wait")
	})
}

// awaitLanded waits until no prefetch of this region whose run holds page is
// still landing: each has given every page it landed up, or dropped it.
//
// It looks under h.mu, and the fault acts on what it saw only after giving
// h.mu up, when it plans again from the top. No prefetch of this region whose
// run holds page can begin in between: a prefetch is split off by a
// fault of its own window, and this fault holds that window's stripe
// throughout. What a prefetch of another region does meanwhile, the new plan
// meets as it is.
func (r *MemoryRegion) awaitLanded(ctx context.Context, page uint64) error {
	h := r.host
	for {
		h.mu.Lock()
		landing := false
		for pf := range h.prefetches {
			if pf.region == r && pf.holding && pf.start <= page && page < pf.end {
				landing = true
				break
			}
		}
		changed := h.changed
		h.mu.Unlock()
		if !landing {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// faultRead is the faulting page's backing read, under way on a task of its
// own while the fault plans the rest of its window. The fault supplies what it
// read, which answers the page's request.
type faultRead struct {
	host   *Host
	page   uint64
	buffer *[]byte
	// unpublished is what a peer backing said of the page: another host's.
	unpublished []bool
	err         error
	done        chan struct{}
	cancel      context.CancelCauseFunc
}

// beginFaulting starts the faulting page's read where the plan reserved a
// slot for it, nil where the page needs none.
func (p *plan) beginFaulting(ctx context.Context, index uint64) *faultRead {
	if p.reserved[index-p.start].slot < 0 {
		return nil
	}
	r := p.region
	h := r.host
	readCtx, cancel := context.WithCancelCause(sim.WithTask(ctx, fmt.Sprintf("fault-read-%d", index)))
	read := &faultRead{host: h, page: index, buffer: h.takeWindow(1), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(read.done)
		if read.err = sim.Admit(readCtx, "vmmemory/fault-read"); read.err == nil {
			read.unpublished, read.err = r.readRun(readCtx, index, []bool{true}, *read.buffer, &h.loadLatency)
		}
	}()
	return read
}

// land waits for the read, with the region given up, and supplies the page.
func (read *faultRead) land(ctx context.Context, p *plan) error {
	if read == nil {
		return nil
	}
	defer read.release()
	r := p.region
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
	return p.publishRead(ctx, read.page, []bool{true}, *read.buffer, read.unpublished)
}

// abandon ends a read the fault no longer wants.
func (read *faultRead) abandon() {
	if read == nil {
		return
	}
	read.cancel(errReadAbandoned)
	read.release()
}

// release waits for the read's goroutine to end and gives its buffer back.
func (read *faultRead) release() {
	read.cancel(nil)
	<-read.done
	read.host.putWindow(read.buffer)
}

// reclaim takes one slot of f with the region given up, evicting where the
// arena is full.
func (r *MemoryRegion) reclaim(ctx context.Context, f *arenaFile) (fileSlot, error) {
	return r.reclaimWith(ctx, func() (fileSlot, error) { return r.host.allocate(ctx, r, f, nil, evictPastAFreeSlot(ctx)) })
}

// reclaimOwn takes a place of page index in the region's own file, with the
// region given up.
func (r *MemoryRegion) reclaimOwn(ctx context.Context, index uint64) (fileSlot, error) {
	return r.reclaimWith(ctx, func() (fileSlot, error) { return r.allocateOwn(ctx, index, true) })
}

// allocateOwn takes a place of page index in the region's own file, evicting
// where the page budget rather than the place is missing. A page never needs a
// third place: where both are taken, one holds a root's page nothing maps,
// published from this region and idle since, which is given up, or a page an
// eviction is taking, whose slot is back once its lock is.
func (r *MemoryRegion) allocateOwn(ctx context.Context, index uint64, clean bool) (fileSlot, error) {
	h := r.host
	places := r.ownPlaces(index, clean)
	preferEviction := evictPastAFreeSlot(ctx)
	for range loadAttempts {
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		for _, at := range places {
			if preferEviction {
				break
			}
			if taken := h.takeOwnLocked(r, index, at); taken.slot >= 0 {
				h.mu.Unlock()
				return taken, nil
			}
		}
		var free *fileSlot
		for _, at := range places {
			if _, held := at.file.leases[at.slot]; !held {
				free = &at
				break
			}
		}
		var idle, moving *zirconvm.VmPage
		if free == nil {
			for _, at := range places {
				page := at.file.frames[at.slot]
				if page == nil {
					continue
				}
				f := frameOf(page)
				switch {
				case f.aliases.len() > 0:
				case f.layer == nil && f.replacing == 0:
					idle = page
				default:
					moving = page
				}
			}
		}
		h.mu.Unlock()
		if free != nil {
			// The free place is taken under the allocation's own hold, by
			// takeOwnLocked again: what was free here may not be by then.
			at := *free
			return h.allocate(ctx, r, at.file, func() int { return h.takeOwnLocked(r, index, at).slot }, preferEviction)
		}
		if idle == nil && moving != nil && !sim.Bug(ctx, "pager-take-a-moving-page-for-a-mapped-one") {
			// Nothing maps the page, yet it is not idle: whoever holds its
			// lock is moving it. An eviction takes every alias off and then
			// gives the slot back, under holds of h.mu of their own and the
			// page's lock throughout. Once that lock is given back the page is
			// gone, mapped or idle, and the places are looked at again from
			// the top.
			if err := r.host.lockPage(ctx, moving); err != nil {
				return fileSlot{}, err
			}
			r.host.unlockPage(moving)
			continue
		}
		if idle == nil {
			return fileSlot{}, fmt.Errorf("%w: both places of page %d of a memory region hold a page it maps", ErrCapacity, index)
		}
		// The idle page was chosen under h.mu, and its lock is taken after.
		// The caller has given the region up, so a settle may hand the page
		// back to this region in between, and an eviction's look age it old
		// enough to take: whether it is idle is asked again under its lock.
		// The places are looked at again from the top either way.
		if allocateOwnSeam != nil {
			allocateOwnSeam(index)
		}
		if !frameOf(idle).mu.TryLock() {
			if err := r.host.lockPage(ctx, idle); err != nil {
				return fileSlot{}, err
			}
		}
		if sim.Bug(ctx, "pager-give-up-an-own-place-as-chosen") {
			// The bug gives the page up as it was chosen, before its lock was
			// held: a page the region maps again is freed, and the pager
			// panics.
			if link, ok := r.host.node.PageQueues().Backlink(idle); ok && frameOf(idle).layer == nil {
				r.host.evictIdle(link.Cow, link.Offset)
			}
		} else {
			r.host.evictIfIdle(idle)
		}
		r.host.unlockPage(idle)
	}
	return fileSlot{}, fmt.Errorf("%w: page %d looked for a place of its own %d times", ErrContended, index,
		loadAttempts)
}

// allocateOwnSeam runs in allocateOwn once it has chosen the idle page it
// will give up and before it takes that page's lock, which is where a settle
// can hand the page back to the region. Production leaves it nil.
var allocateOwnSeam func(index uint64)

// makeRoom gives up idle pages until want slots of f are free, or no idle
// page is left, as Host.makeRoom does.
func (h *Host) makeRoom(_ context.Context, f *arenaFile, want int) error {
	for {
		h.mu.Lock()
		free := h.freeLocked(f)
		err := h.err
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if free >= want || !h.takeIdle() {
			return nil
		}
	}
}
