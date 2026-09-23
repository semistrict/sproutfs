package vmmemory

import (
	"context"
	"errors"
)

// Fault orders operations only within this region's read-ahead window. Shared
// page transitions additionally take that page's lock. Different windows and
// volumes can load, spill, and flush concurrently, bounded by ConcurrentIO and
// the resident/dirty budgets. A read fault loads and maps as much of its window
// as free slots allow; pages of the window that are already resident under the
// same stored identity are mapped without loading anything.
func (r *Region) Fault(ctx context.Context, index uint64, write bool) error {
	if index >= uint64(r.pageCount) {
		return ErrRange
	}
	// Whatever this fault admitted to the dirty budget is measured against the
	// high-water mark here, where it holds nothing: every path that takes a
	// reservation is under this one.
	defer r.host.askAtHighWater()
	reserve := false
	for range faultAttempts {
		// A store that needs a private page takes its dirty reservation before
		// any region, page or I/O resource. While a checkpoint publishes, that
		// reservation waits for it to release one, and the publication needs
		// exactly those resources to get there. A read of a page another host
		// still holds is the same kind of page — the load makes it this
		// region's dirty state — so the attempt that discovers it comes back
		// here to take one the same way.
		spill := -1
		if reserve || (write && r.needsPrivatePage(index)) {
			slot, err := r.host.takeSpill(ctx, r)
			if err != nil {
				return err
			}
			spill = slot
		}
		retry, err := r.fault(ctx, index, write, &spill)
		if spill >= 0 {
			r.host.releaseSpill(spill)
		}
		if errors.Is(err, errUnpublishedReservation) {
			reserve, retry, err = true, true, nil
		}
		if !retry {
			return err
		}
	}
	return ErrContended
}

// errUnpublishedReservation reports a fault whose window turned out to hold
// pages no checkpoint has, which the load takes as this region's dirty state.
// It never leaves the package: the fault releases everything it holds, takes a
// dirty reservation through the waiting path, and tries again.
var errUnpublishedReservation = errors.New("managed-memory fault needs a dirty reservation")

// faultAttempts bounds how often a fault re-decides whether it needs a private
// page. Only a seal taken between that decision and the region lock can force
// another attempt, so one repetition is enough in every observed case.
const faultAttempts = 8

// fault serves one attempt and reports whether it must be retried with a dirty
// reservation it did not hold. It consumes *spill by setting it to -1.
func (r *Region) fault(ctx context.Context, index uint64, write bool, spill *int) (retry bool, err error) {
	started := r.host.clock.Now()
	if err := r.live.RLock(ctx); err != nil {
		return false, err
	}
	defer r.live.RUnlock()
	// The window's stripe comes before the region, not after it: a fault gives
	// the region up across its backing read and takes it again, and a fault
	// waiting for a stripe while holding the region would leave that second
	// acquisition queued behind a seal the stripe holder is waiting for.
	if err := r.stripe(index).Lock(ctx); err != nil {
		return false, err
	}
	defer r.stripe(index).Unlock()
	if err := r.lockPageAccess(ctx, index, write); err != nil {
		return false, err
	}
	defer func() {
		// A read that was cancelled while it waited for the region back holds
		// it no longer, and says so.
		if !errors.Is(err, errRegionDropped) {
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
	// Observed for exactly the attempts the counter above counts, and from the
	// top of the attempt, so the histogram decomposes Stats.Faults and includes
	// the region, page and I/O waits a fault can spend before it does any work.
	defer func() { h.faultLatency.Observe(h.clock.Since(started)) }()
	if !write && r.zeroMapped(index) {
		if err := r.resolvePages(ctx, index, 1, false); err != nil {
			return false, r.fail(err)
		}
		return false, nil
	}
	b := r.binding(index)
	if !write || b.writable() {
		return false, r.load(ctx, index, spill)
	}
	if *spill < 0 {
		// A seal took this page into a checkpoint after the reservation decision.
		return true, nil
	}
	if fresh, err := r.storeFresh(ctx, index, spill); fresh || err != nil {
		return false, err
	}
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	if pg == nil && !b.zero && !b.dirty {
		// This region holds no memory for the page, so the copy has nothing
		// here to be made from. Reading the page in first is the read fault
		// this write fault often really is: it lands in the sharing index under
		// the identity its volume gives it, so the copy has an origin and every
		// region that inherits that identity maps the page rather than reading
		// it again.
		if pg, err = r.readIn(ctx, index); err != nil {
			return false, err
		}
		if pg != nil && !r.needsPrivatePage(index) {
			// The region was given up to read, and this page is the guest's own
			// state now. What the store needs is decided again from the top.
			h.unlock(pg)
			return true, nil
		}
	}
	defer func() {
		if pg != nil {
			h.unlock(pg)
		}
	}()
	held := r.checkpointCopy(b)
	// A copy of a page holding a published identity remembers where it came
	// from: those bytes are immutable while that page holds that name, so a
	// settle can tell a page the guest really stored into from one a write
	// fault merely took writable.
	var origin *resident
	if pg.published() {
		origin = pg
	}
	data := make([]byte, h.pageSize)
	unpublished, err := r.readForCopy(ctx, b, pg, data)
	if err != nil {
		return false, err
	}
	if pg != nil {
		// The old shared page is unlocked before allocation, allowing a host
		// with a single slot to reclaim it and install this private copy. It
		// keeps the page and the checkpoint keeps it until that copy is bound:
		// a page released from the checkpoint before its replacement exists is
		// dirty state with no memory, no reservation and no checkpoint holding
		// either, which is what any failure in between would leave behind.
		h.unlock(pg)
		pg = nil
	}
	// The region is given up for the reclaim too: this page is in no dirty set
	// until the store below puts it back in one, so a seal taken while the
	// reclaim runs has nothing of this page's to take. Ending a seal does reach
	// it, and hands it either its own reservation or clean state, so what this
	// store needs is decided again from the top.
	slot, err := r.reclaimPrivate(ctx, index)
	if err != nil {
		return false, err
	}
	if r.checkpointCopy(b) != held {
		if err := h.abandonSlots(ctx, slot, 1, nil); err != nil {
			return false, err
		}
		return true, nil
	}
	pg, err = h.create(ctx, slot, data, pageKey{}, true, r.kind)
	if err != nil {
		return false, err
	}
	// The page leaves the checkpoint's memory for its own under that page's
	// lock, so that at no moment is it dirty with neither a reservation of its
	// own nor a checkpoint holding one.
	old, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	// The pages this store copies from stay where they are until its one mapping
	// command has replaced the guest's mappings of them; nothing is revoked.
	replaced := &replacement{region: r}
	if err := r.takePrivate(ctx, b, old, pg, *spill, origin, replaced); err != nil {
		return false, err
	}
	*spill = -1
	if unpublished {
		// The bytes came from the host that still holds them and this store has
		// just bound them here as this region's own dirty state, so that host no
		// longer holds the only copy. Telling the backing is the whole of how it
		// learns: the load path is not the only way a page only another host had
		// arrives, and a page nothing reports stays one the source may not stop
		// serving and the destination believes it is missing.
		r.installedUnpublished(index*h.pageSize, []bool{true})
	}
	h.mu.Lock()
	h.stats.CopyOnWrites++
	h.mu.Unlock()
	h.touch(pg)
	// The two rules: the shared pages between this store and a page its range
	// already held, and the holes of a range that has become half its own, are
	// made private in this same fault. The whole run sits at consecutive offsets
	// of one extent, so one command maps it.
	first, last, err := r.closeAround(ctx, index, pg.slot, replaced)
	if err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	for page := first; page < last; page++ {
		r.setMapped(r.binding(page), true) // a failed ACK may still have installed the mapping
	}
	slot, count := pg.slot-int(index-first), int(last-first)
	// One command for the run, and it replaces what the guest had: the copies go
	// where the pages they were made from were mapped, so nothing was taken away
	// first and nothing is left without a mapping in between.
	if err := r.mapPages(ctx, first, slot, count, true); err != nil {
		// It did not land, so the guest goes on mapping the pages this store
		// copied from while their memory is about to go back. Taking those
		// mappings away is the one revocation a store ever issues.
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		err = r.mappingFailed(err, func() { r.unmapPages(first, count) })
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		// The backstop. The client has no mapping left for this store, so
		// the range the guest is writing in is made whole: its alternations
		// stop costing that process a mapping each, and the store is served
		// again. It is expected never to act.
		merged, mergeErr := r.makeWhole(ctx, index)
		if mergeErr != nil {
			return false, mergeErr
		}
		return merged, err
	}
	if err := replaced.done(ctx); err != nil {
		return false, r.fail(err)
	}
	if err := r.resolvePages(ctx, first, int(last-first), true); err != nil {
		return false, r.fail(err)
	}
	return false, nil
}

// readIn gives a store into a page this region holds no memory for something to
// copy away from: the resident page that holds the identity this page's volume
// gives it, locked. One is there already where another region of this pager
// inherited the same identity; otherwise the bytes are read into a page of
// their own, which enters the sharing index under that identity — so the copy
// has an origin the settle can compare it with, and the next region to inherit
// the identity maps that page rather than reading it again.
//
// It brings the faulting page's whole read-ahead window in while it is there,
// because a store is how a cold fork faults. On x86-64 KVM finishes a fault
// that had to wait for the pager from a worker that asks for the page writable
// whatever the guest's access was, so a guest merely reading what it inherited
// reaches the pager as a store; a store that read its own page alone made that
// pass one round trip per page, which at 4 KiB is 512 of them per 2 MiB the
// guest touches. The window is installed exactly as a read fault installs it —
// shared residents under their identities, mapped read-only in runs, taking
// only free slots — but for the faulting page itself, which is left unmapped
// because the copy is about to replace it and mapping it read-only first would
// cost every store a revocation and a fence for nothing.
//
// A page whose bytes no checkpoint published has none: a hole, a page whose
// backing names another page, and a page only another host still holds, whose
// bytes are not the volume's at all. The store reads its own copy from the
// backing then, exactly as it always did, and remembers no origin.
//
// Which of those a page is has two answers on a post-copy destination, and they
// come from different places: the extents report the set its handoff fixed, and
// a load reports what the source said when it answered. The load's is the one
// that saw the bytes, so it decides — the extents only save the read. That
// second answer is per page and only a load can give it, so a peer backing's
// store reads its page alone, as every store did before.
func (r *Region) readIn(ctx context.Context, index uint64) (*resident, error) {
	if !r.peer {
		for range loadAttempts {
			pg, retry, err := r.readInWindow(ctx, index)
			if err != nil || !retry {
				return pg, err
			}
		}
		// Nothing but a publication race gets here, and a store that could not
		// win one still has a volume to read its copy from.
		return nil, nil
	}
	return r.readInPage(ctx, index)
}

// readInWindow is readIn over the faulting page's whole window, for a backing
// every page of which the volume itself holds. It reports the origin the store
// copies from, still locked, with every other page of the window resident,
// shared and mapped; or that the faulting page lost a publication race and the
// store must try again, holding nothing.
func (r *Region) readInWindow(ctx context.Context, index uint64) (pg *resident, retry bool, err error) {
	start, end := r.window(index)
	// The plan names no faulting page: nothing here resolves the guest's access
	// or maps the page it trapped on, which the store does for itself once it
	// holds its copy.
	plan, err := r.plan(ctx, start, end, end)
	if err != nil {
		return nil, false, err
	}
	defer plan.unlock()
	// The faulting page is the store's, so the plan leaves it bound to nothing
	// and maps nothing for it; every other page of the window is this region's
	// to hold and to map.
	plan.store = index
	if id, named := plan.identity(index); !named || id.zero() {
		return nil, false, nil
	}
	// The faulting page comes first, waiting for its identity's resident while
	// the plan holds no other, and reserving a slot for it among the run of
	// pages around it so that the run lands in consecutive slots.
	if err := plan.bindShared(ctx, index, true); err != nil {
		return nil, false, err
	}
	if plan.pages[index-start] == nil {
		plan.reserveAround(index)
		if plan.reserved[index-start] < 0 {
			slot, err := r.reclaim(ctx)
			if err != nil {
				return nil, false, err
			}
			plan.reserve(index, slot)
		}
	}
	for page := start; page < end; page++ {
		i := page - start
		if plan.pages[i] != nil || plan.zeros[i] || plan.reserved[i] >= 0 || !plan.eligible(page) {
			continue
		}
		if err := plan.bindShared(ctx, page, false); err != nil {
			return nil, false, err
		}
	}
	if err := plan.reserveRuns(ctx, index); err != nil {
		return nil, false, err
	}
	if err := plan.loadReserved(ctx); err != nil {
		return nil, false, err
	}
	if pg = plan.pages[index-start]; pg == nil {
		// Another fault published this identity while the read ran and its page
		// was busy, so the plan left this one for a later fault. The store
		// decides again from the top, holding nothing.
		return nil, true, nil
	}
	// Nothing is installed for the store's own page: its page tables are the
	// copy's to install once the store holds it.
	plan.fresh[index-start] = false
	if _, err := plan.install(ctx); err != nil {
		return nil, false, err
	}
	// The caller holds the origin from here; the plan releases everything else.
	delete(plan.locked, pg)
	return pg, false, nil
}

// readInPage is readIn of the faulting page alone, which is what a backing
// whose loads can answer that the source still holds a page needs: that answer
// is per page, and a page it gives it for is one nothing may be shared under.
func (r *Region) readInPage(ctx context.Context, index uint64) (*resident, error) {
	h := r.host
	window, err := r.plan(ctx, index, index+1, index)
	if err != nil {
		return nil, err
	}
	id, named := window.identity(index)
	if !named || id.zero() || window.unpublished(index) {
		return nil, nil
	}
	for range loadAttempts {
		h.mu.Lock()
		pg := h.clean[id]
		h.mu.Unlock()
		if pg != nil {
			// Waiting for it is safe: this fault holds no other page.
			if err := pg.mu.Lock(ctx); err != nil {
				return nil, err
			}
			h.mu.Lock()
			current := h.clean[id] == pg
			if current {
				h.stats.IdentityHits++
			}
			h.mu.Unlock()
			if !current {
				h.unlock(pg)
				continue
			}
			h.touch(pg)
			return pg, nil
		}
		slot, err := r.reclaimNear(ctx, index)
		if err != nil {
			return nil, err
		}
		data := make([]byte, h.pageSize)
		unpublished, err := r.loadWindow(ctx, index*h.pageSize, data)
		if err != nil {
			return nil, h.abandonSlots(ctx, slot, 1, err)
		}
		if len(unpublished) > 0 && unpublished[0] {
			// The extents named this page the volume's and the load found the
			// source still holding it. The two answers come from different
			// places — the set the handoff fixed, and what the source said when
			// it answered — and the one that saw the bytes is the load's. They
			// are not this identity's bytes, so nothing may be shared under it:
			// the store reads its own copy from the backing and tells the
			// backing it took the page, exactly as for a page the extents
			// themselves call unpublished.
			return nil, h.abandonSlots(ctx, slot, 1, nil)
		}
		h.mu.Lock()
		h.stats.Loads++
		h.stats.LoadedPages++
		h.mu.Unlock()
		pg, err = h.create(ctx, slot, data, id, false, r.kind)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		published := h.clean[id]
		if published == nil {
			h.clean[id] = pg
			h.cleanVersion++
		}
		h.mu.Unlock()
		if published == nil {
			return pg, nil
		}
		// Another fault published this identity while the read ran; that page
		// is the one every region maps, so this one goes back to the arena.
		err = h.release(ctx, pg)
		h.unlock(pg)
		if err != nil {
			return nil, err
		}
	}
	// Nothing but a publication race gets here, and a store that could not win
	// one still has a volume to read its copy from.
	return nil, nil
}

// takePrivate makes a freshly filled resident page this page's own. The alias
// of the page it is leaving is taken away under that page's lock, and the dirty
// reservation the store was admitted under is installed in the same step: a
// private page reachable from a binding owning neither a reservation nor a
// checkpoint is one a reclaim would punch. The caller holds the new resident
// page; old is the one the page is leaving, if any. origin is that page where
// the copy was made from a published identity, and it is left in the arena
// rather than released, because it is what the settle compares this copy
// against.
//
// Nothing is revoked. The guest's mapping of this page is replaced by the one
// command the store issues for its whole run, so the page it is leaving is
// handed to replaced instead: held where it is until that command lands, and
// given up there.
func (r *Region) takePrivate(ctx context.Context, b *binding, old, pg *resident, slot int, origin *resident, replaced *replacement) error {
	h := r.host
	if old != nil {
		defer h.unlock(old)
		if r.isMapped(b) {
			replaced.hold(b, old)
		}
	}
	switch {
	case old == nil:
	case old == origin:
		h.leave(b, old)
	default:
		// A page still held by a checkpoint keeps its memory: the checkpoint's
		// own alias survives this unlink, and the guest gets its own copy.
		if err := h.unlink(ctx, b, old); err != nil {
			return err
		}
	}
	h.bind(b, pg)
	r.takeFromCheckpoint(b, slot, origin)
	h.probe.granted(b, pg, origin)
	from := -1
	if origin != nil {
		from = origin.slot
	}
	note(r, b.index, "copy-on-write", pg.slot, from)
	return nil
}

// storeFresh serves a store into a page whose bytes are known zeros and that
// owns nothing: a zero-mapped page, or a hole in the volume the guest has never
// touched. For any other page it reports false having done nothing. There is no
// memory to copy and nothing to fence, so nothing is revoked: one mapping
// command puts fresh pages where the zeros or the trap were, and a zero
// mapping keeps serving reads until it lands. Write-ahead makes the fresh zero
// pages around it private in that same command.
func (r *Region) storeFresh(ctx context.Context, index uint64, spill *int) (bool, error) {
	zero, untouched := r.fresh(index)
	if !zero && !untouched {
		return false, nil
	}
	var plan *windowPlan
	if untouched {
		// Whether an untouched page is a hole is volume metadata, which the
		// window's extents answer for its neighbours too; no bytes are read.
		start, end := r.window(index)
		var err error
		if plan, err = r.plan(ctx, start, end, index); err != nil {
			return false, err
		}
		if id, ok := plan.identity(index); !ok || !id.zero() {
			return false, nil
		}
	}
	first, last := r.zeroRun(index, plan)
	return true, r.storeZeros(ctx, index, first, last, spill)
}

// zeroRun bounds the run a store into index makes private: index, the
// fresh zero pages after it up to the end of its read-ahead window, then those
// before it, at most WriteAheadPages together. A page is fresh zeros when it is
// zero-mapped or, by the window's extents when the store has them, an untouched
// hole.
func (r *Region) zeroRun(index uint64, plan *windowPlan) (uint64, uint64) {
	start, end := r.window(index)
	limit := uint64(r.host.cfg.WriteAheadPages)
	zeros := func(page uint64) bool {
		zero, untouched := r.fresh(page)
		if zero || !untouched || plan == nil {
			return zero
		}
		id, ok := plan.identity(page)
		return ok && id.zero()
	}
	first, last := index, index+1
	for last < end && last-first < limit && zeros(last) {
		last++
	}
	for first > start && last-first < limit && zeros(first-1) {
		first--
	}
	return first, last
}

// around narrows [first, last) to at most n pages that still hold index,
// preferring the pages after it: access tends to continue forward.
func around(index, first, last uint64, n int) (uint64, uint64) {
	if last-first <= uint64(n) {
		return first, last
	}
	start := max(first, min(index, last-uint64(n)))
	return start, start + uint64(n)
}

// storeZeros gives the fresh zero pages [first, last), which hold index, fresh
// private pages and maps them writable. The faulting page brings its own dirty
// reservation and may evict for its slot; the rest of the run takes only free
// reservations and free slots, never waiting for either, and shrinks to what it
// finds. Each set of consecutive arena offsets the run landed in is one mapping
// command — one, unless the placement rule put the run in the extents of
// several ranges and those extents are not themselves consecutive.
func (r *Region) storeZeros(ctx context.Context, index, first, last uint64, spill *int) error {
	h := r.host
	extras := h.takeFreeSpill(int(last-first) - 1)
	used := 0
	defer func() {
		for _, slot := range extras[used:] {
			h.releaseSpill(slot)
		}
	}()
	first, last = around(index, first, last, 1+len(extras))
	first, runs, err := r.allocateRun(ctx, index, first, last)
	if err != nil {
		return err
	}
	count := 0
	for _, run := range runs {
		count += run.Count
	}
	pages, err := h.createZeroRuns(ctx, runs, r.kind)
	if err != nil {
		return err
	}
	defer h.unlockRun(pages)
	// The run is bound, made dirty and marked mapped a lock at a time for the
	// whole of it: a boot's vCPUs fault runs of thousands of pages at once, and
	// taking the host's lock per page is what they would queue on.
	bindings := r.bindingRun(first, uint64(len(pages)))
	h.bindRun(bindings, pages)
	r.setDirtyMappedRun(bindings) // a failed ACK may still have installed the mapping
	for k, b := range bindings {
		h.probe.granted(b, pages[k], nil)
		b.zero = false
		if b.index == index {
			b.spillSlot, *spill = *spill, -1
		} else {
			b.spillSlot, b.ahead = extras[used], true
			used++
		}
	}
	h.mu.Lock()
	h.stats.CopyOnWrites++
	h.stats.WriteAheadPages += uint64(count - 1)
	h.mu.Unlock()
	for _, run := range runs {
		if err := r.mapPages(ctx, run.Page, run.Slot, run.Count, true); err != nil {
			return r.mappingFailed(err, func() { r.unmapPages(first, count) })
		}
	}
	for _, run := range runs {
		if err := r.resolvePages(ctx, run.Page, run.Count, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// allocateRun takes arena slots for the run [first, last), which holds index,
// and reports the pages it covered and the runs of consecutive offsets they
// landed in.
//
// The placement rule comes first: every page goes at the offset it has within
// its range's extent, so a window crossing a range boundary is one run per
// range unless the extents are consecutive too. Where a range can have no
// extent, a run of several pages takes only free ordinary slots, consecutive so
// that one command maps them, preferably those after the slot of the page
// before it so that the mapping continues its neighbour's; it shrinks to the
// free slots it finds. A lone page, or a run finding no free slot, allocates
// for index alone, which may evict.
func (r *Region) allocateRun(ctx context.Context, index, first, last uint64) (uint64, []MapRun, error) {
	h := r.host
	if err := h.makeRoom(ctx, int(last-first)); err != nil {
		return 0, nil, err
	}
	h.mu.Lock()
	noExtent := h.placing() && h.slots.FreeExtents() == 0 && h.extents[extentKey{r, index / uint64(h.extentPages)}] == nil
	h.mu.Unlock()
	if noExtent {
		if _, err := h.reclaimExtent(ctx); err != nil {
			return 0, nil, err
		}
	}
	if start, runs := h.placeRun(r, index, first, last); len(runs) > 0 {
		return start, runs, nil
	}
	if last-first > 1 {
		prefer := -1
		if first > 0 {
			if b := r.lookupBinding(first - 1); b != nil {
				h.mu.Lock()
				if b.resident != nil && b.resident.slot >= 0 {
					prefer = b.resident.slot + 1
				}
				h.mu.Unlock()
			}
		}
		if slot, count := h.allocateFreeFrom(prefer, int(last-first)); count > 0 {
			start, _ := around(index, first, last, count)
			return start, []MapRun{{Page: start, Slot: slot, Count: count}}, nil
		}
	}
	slot, err := r.reclaimPrivate(ctx, index)
	if err != nil {
		return 0, nil, err
	}
	return index, []MapRun{{Page: index, Slot: slot, Count: 1}}, nil
}

// loadAttempts bounds how often a fault retries after losing a publication race
// for its identity. Each retry begins by waiting for the winner while holding
// no other resident lock, so one is enough in every observed case.
const loadAttempts = 64

// load maps the faulting page and as much of its page as can be served
// without evicting. A publication race for the faulting page's identity is
// resolved by retrying with no resident lock held, never by waiting for the
// winner from inside a plan that already holds others.
func (r *Region) load(ctx context.Context, index uint64, spill *int) error {
	for range loadAttempts {
		resolved, err := r.loadOnce(ctx, index, spill)
		if err != nil || resolved {
			return err
		}
	}
	return ErrContended
}

// loadOnce reports whether the faulting page ended mapped and resolved. The
// faulting page may evict; read-ahead only uses free slots and only pages whose
// locks are free, so it never waits behind other work and cannot deadlock.
func (r *Region) loadOnce(ctx context.Context, index uint64, spill *int) (bool, error) {
	h := r.host
	b := r.binding(index)
	if b.zero {
		if !b.mapped {
			r.setMapped(b, true)
			if err := r.mapZeroPages(ctx, index, 1); err != nil {
				return false, r.mappingFailed(err, func() { r.setMapped(b, false) })
			}
		}
		if err := r.resolvePages(ctx, index, 1, false); err != nil {
			return false, r.fail(err)
		}
		return true, nil
	}
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	if pg != nil {
		// Resident: a dirty refault, a refault after a failed ACK, or a page
		// read-ahead installed. Install page tables and wake the accessor.
		defer h.unlock(pg)
		h.touch(pg)
		if !b.mapped {
			r.setMapped(b, true)
			if err := r.mapPages(ctx, index, pg.slot, 1, b.writable()); err != nil {
				return false, r.mappingFailed(err, func() { r.setMapped(b, false) })
			}
		}
		if err := r.resolvePages(ctx, index, 1, b.writable()); err != nil {
			return false, r.fail(err)
		}
		return true, nil
	}
	dirty, held := r.privateEpoch(b)
	if dirty {
		// Spilled private state is reloaded alone; it has no shared source. A page a
		// checkpoint still holds reloads read-only, so the next store copies, and
		// onto a page it shares with the checkpoint again: the checkpoint's copy is
		// the alias that owns the reservation spilling those bytes, and retiring the
		// checkpoint retires this page with it rather than stranding a private one
		// no reservation covers.
		data := make([]byte, h.pageSize)
		if err := h.read(ctx, b, nil, data); err != nil {
			return false, err
		}
		h.mu.Lock()
		h.stats.SpillRefaults++
		h.mu.Unlock()
		slot, err := r.reclaimPrivate(ctx, index)
		if err != nil {
			return false, err
		}
		// The region was given up for the reclaim, and a seal or a retire taken
		// while it was is exactly what this page being the guest's own dirty
		// state was decided against. A retire ends that epoch — the volume holds
		// these bytes now and the reservation that spilled them has gone back —
		// and a private page bound to a binding owning neither a reservation nor
		// a checkpoint is one the next reclaim punches without writing it
		// anywhere. What this page is, is decided again from the top.
		if now, current := r.privateEpoch(b); now != dirty || current != held {
			if err := h.abandonSlots(ctx, slot, 1, nil); err != nil {
				return false, err
			}
			return false, nil
		}
		pg, err := h.create(ctx, slot, data, pageKey{}, true, r.kind)
		if err != nil {
			return false, err
		}
		defer h.unlock(pg)
		h.bind(b, pg)
		if b.checkpoint != nil {
			h.bind(b.checkpoint, pg)
		}
		// The refaulted page holds the guest's own current bytes, so it is the
		// newest generation of them and not a step back.
		h.probe.granted(b, pg, nil)
		h.touch(pg)
		r.setMapped(b, true)
		if err := r.mapPages(ctx, index, pg.slot, 1, b.writable()); err != nil {
			return false, r.mappingFailed(err, func() { r.setMapped(b, false) })
		}
		if err := r.resolvePages(ctx, index, 1, b.writable()); err != nil {
			return false, r.fail(err)
		}
		return true, nil
	}
	start, end := r.window(index)
	plan, err := r.plan(ctx, start, end, index)
	if err != nil {
		return false, err
	}
	defer plan.unlock()
	if plan.unpublished(index) && *spill < 0 {
		// The extents say another host still holds this page, so the load takes
		// it as this region's dirty state. The reservation for it is taken by
		// the waiting path, with no region, page or I/O resource held: a
		// destination at its dirty bound stalls the post-copy read until a
		// checkpoint relieves the budget, it never fails the session.
		return false, errUnpublishedReservation
	}
	plan.spill = spill
	// The faulting page comes first: bound to its resident identity if one
	// exists, otherwise reserved together with the run of pages around it so
	// the run lands in consecutive slots, and as a last resort by evicting.
	// Waiting here is safe because the plan holds no other resident lock yet.
	if err := plan.bindShared(ctx, index, true); err != nil {
		return false, err
	}
	if plan.pages[index-start] == nil && !plan.zeros[index-start] {
		plan.reserveAround(index)
	}
	if plan.pages[index-start] == nil && !plan.zeros[index-start] && plan.reserved[index-start] < 0 {
		slot, err := r.reclaim(ctx)
		if err != nil {
			return false, err
		}
		plan.reserve(index, slot)
	}
	for p := start; p < end; p++ {
		if plan.pages[p-start] != nil || plan.zeros[p-start] || plan.reserved[p-start] >= 0 || !plan.eligible(p) {
			continue
		}
		if err := plan.bindShared(ctx, p, false); err != nil {
			return false, err
		}
	}
	if err := plan.reserveRuns(ctx, index); err != nil {
		return false, err
	}
	if err := plan.loadReserved(ctx); err != nil {
		return false, err
	}
	return plan.install(ctx)
}
