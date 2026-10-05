package vmmemory

import (
	"context"
	"errors"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Stores over the zircon core. A store makes a page of the region's own layer
// Dirty, as a write makes a page of a VMO a user pager backs Dirty: the pager
// fills a frame with the bytes the page holds now, at the offset of the
// region's private file the placement rule gives it, supplies it to the
// layer, and makes it Dirty there (zirconvm DirtyPages, zx_pager_op_range's
// DIRTY). The layer's page then shadows the identity root's, as a
// snapshot-on-write child's copy shadows its parent's. A store into fresh
// zeros makes its whole write-ahead run Dirty at once. Which pages a store
// makes private, write-ahead, placement and the two rules are the current
// core's (fault.go, placement.go, rules.go), and move as they are.
//
// The dirty reservation a page is admitted under is the pager's, kept in its
// binding beside the layer: the spill moves into this core with eviction, and
// the reservation into the page with it (D5, step 12). So does the cold copy:
// a store trap's copy here is an ordinary dirty page.

// writable reports whether the guest may store into a page where it is: the
// region's own Dirty page.
func (z *zirconRegion) writable(index uint64) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	b, _ := z.lookupLocked(index)
	return b != nil && b.dirty
}

// holdsOwn reports whether the region's own layer holds a page at index,
// whatever the volume says is there.
func (z *zirconRegion) holdsOwn(index uint64) bool {
	z.mu.Lock()
	b, _ := z.lookupLocked(index)
	z.mu.Unlock()
	if b == nil {
		return false
	}
	h := z.region.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return b.page != nil && frameOf(b.page).own
}

// needsPrivatePage reports whether a store to index would have to make a page
// of its own, and with it take a dirty reservation. It takes no region lock:
// a store decides this before it competes for one, and decides again after.
func (z *zirconRegion) needsPrivatePage(index uint64) bool { return !z.writable(index) }

// fresh is MemoryRegion.fresh over the bindings beside the layer: zero where
// the page is mapped to zero, untouched where the region holds nothing of it
// at all.
func (z *zirconRegion) fresh(index uint64) (zero, untouched bool) {
	z.mu.Lock()
	b, zeroRun := z.lookupLocked(index)
	z.mu.Unlock()
	if zeroRun {
		return true, false
	}
	if b == nil {
		return false, true
	}
	h := z.region.host
	h.mu.Lock()
	bound := b.page != nil
	h.mu.Unlock()
	if bound {
		return false, false
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if b.dirty {
		return false, false
	}
	return b.zero, !b.zero && !b.mapped
}

// store serves a store into a page the region may not store into yet.
func (z *zirconRegion) store(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	if fresh, err := z.storeFresh(ctx, index, spill); fresh || err != nil {
		return false, err
	}
	return z.copyOnWrite(ctx, index, spill)
}

// storeFresh is MemoryRegion.storeFresh over the zircon core: a store into a
// page whose bytes are known zeros and that the region holds nothing of.
func (z *zirconRegion) storeFresh(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	zero, untouched := z.fresh(index)
	if !zero && !untouched {
		return false, nil
	}
	var plan *zplan
	if untouched {
		// Whether an untouched page is a hole is volume metadata, which the
		// window's extents answer for its neighbours too; the page is asked
		// first, alone.
		start, end := z.region.window(index)
		var err error
		if plan, err = z.planPage(ctx, start, end, index, index); err != nil {
			return false, err
		}
		if id, ok := plan.identity(index); !ok || !id.zero() {
			return false, nil
		}
		if err := plan.locateWindow(ctx); err != nil {
			return false, err
		}
	}
	first, last := z.zeroRun(index, plan)
	return true, z.storeZeros(ctx, index, first, last, spill)
}

// zeroRun is MemoryRegion.zeroRun over the zircon core.
func (z *zirconRegion) zeroRun(index uint64, plan *zplan) (uint64, uint64) {
	r := z.region
	start, end := r.window(index)
	limit := uint64(r.host.cfg.WriteAheadPages)
	zeros := func(page uint64) bool {
		zero, untouched := z.fresh(page)
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

// storeZeros is MemoryRegion.storeZeros over the zircon core: the fresh zero
// pages [first, last), which hold index, become Dirty pages of the layer in
// fresh frames, mapped writable. The faulting page brings its own dirty
// reservation; the rest of the run takes only free reservations and free
// slots, and shrinks to what it finds.
func (z *zirconRegion) storeZeros(ctx context.Context, index, first, last uint64, spill *reservation) error {
	r := z.region
	h := r.host
	extras := h.takeFreeSpill(int(last-first) - 1)
	used := 0
	defer func() {
		for _, extra := range extras[used:] {
			h.releaseSpill(extra)
		}
	}()
	first, last = around(index, first, last, 1+len(extras))
	first, runs, err := z.allocateRun(ctx, index, first, last)
	if err != nil {
		return err
	}
	count := 0
	for _, run := range runs {
		count += run.Count
	}
	frames, err := z.host.newZeroFrames(ctx, r.privateFile(), runs, r.kind)
	if err != nil {
		return err
	}
	// The run is supplied and made Dirty a run of slots at a time, and bound
	// a lock at a time for the whole of it, as the current core binds it: a
	// boot's vCPUs fault runs of thousands of pages at once.
	page := first
	for k, run := range runs {
		if err := z.supplyDirty(ctx, page, frames[k]); err != nil {
			return err
		}
		page += uint64(run.Count)
	}
	reservations := make([]reservation, count)
	ahead := make([]bool, count)
	for k := range count {
		if first+uint64(k) == index {
			reservations[k], *spill = *spill, noReservation
			continue
		}
		reservations[k], ahead[k] = extras[used], true
		used++
	}
	all := make([]*zirconvm.VmPage, 0, count)
	for _, run := range frames {
		all = append(all, run...)
	}
	// Mapped before the command: an ambiguous answer may still have
	// installed it.
	z.bindDirtyRun(first, all, reservations, ahead)
	h.mu.Lock()
	h.stats.CopyOnWrites++
	h.stats.WriteAheadPages += uint64(count - 1)
	h.mu.Unlock()
	for _, run := range runs {
		if err := r.mapPages(ctx, run, true); err != nil {
			return r.mappingFailed(err, func() { z.setMapped(first, first+uint64(count), false) })
		}
	}
	for _, run := range runs {
		if err := r.resolvePages(ctx, run.Page, run.Count, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// allocateRun is MemoryRegion.allocateRun over the zircon core: slots of the
// region's private file for the run [first, last), which holds index, by the
// placement rule first, then free consecutive slots, then index alone, which
// is the one slot that may give up idle pages for it.
func (z *zirconRegion) allocateRun(ctx context.Context, index, first, last uint64) (uint64, []MapRun, error) {
	r := z.region
	h := r.host
	f := r.privateFile()
	if err := z.host.makeRoom(ctx, f, int(last-first)); err != nil {
		return 0, nil, err
	}
	h.mu.Lock()
	if f.owner != nil {
		// Every page of a private file's run is placed at once, so the run is
		// what the pager has room for, and never less than the faulting page.
		first, last = around(index, first, last, max(h.freeLocked(f), 1))
	}
	h.mu.Unlock()
	if start, runs := h.placeRun(r, index, first, last); len(runs) > 0 {
		return start, runs, nil
	}
	if last-first > 1 && f.owner == nil {
		prefer := fileSlot{f, -1}
		if first > 0 {
			if at := z.slotOf(first - 1); at.slot >= 0 && at.file == f {
				prefer = at.plus(1)
			}
		}
		if at, count := h.allocateFreeFrom(prefer, int(last-first)); count > 0 {
			start, _ := around(index, first, last, count)
			return start, []MapRun{r.runAt(start, at, count)}, nil
		}
	}
	at, err := z.reclaimPrivate(ctx, index)
	if err != nil {
		return 0, nil, err
	}
	return index, []MapRun{r.runAt(index, at, 1)}, nil
}

// slotOf is the slot of the page the region maps at index, slot -1 where it
// maps none.
func (z *zirconRegion) slotOf(index uint64) fileSlot {
	z.mu.Lock()
	b, _ := z.lookupLocked(index)
	z.mu.Unlock()
	if b == nil {
		return fileSlot{slot: -1}
	}
	h := z.region.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if b.page == nil {
		return fileSlot{slot: -1}
	}
	return frameOf(b.page).fileSlot
}

// reclaimPrivate takes the slot the placement rule gives a private page of
// index, with the region given up, as MemoryRegion.reclaimPrivate does: in an
// isolated arena one of the page's two places in the region's own file, and
// otherwise its offset in its range's extent, or an ordinary one beside its
// neighbours where it has none. It gives up idle pages for it and evicts no
// page a region maps.
func (z *zirconRegion) reclaimPrivate(ctx context.Context, index uint64) (fileSlot, error) {
	r := z.region
	h := r.host
	return r.reclaimWith(ctx, func() (fileSlot, error) {
		if reclaimSeam != nil {
			reclaimSeam(index)
		}
		if err := context.Cause(ctx); err != nil {
			return fileSlot{}, err
		}
		if r.private != nil {
			for _, at := range r.ownPlaces(index, false) {
				h.mu.Lock()
				_, held := at.file.leases[at.slot]
				h.mu.Unlock()
				if !held {
					return z.host.allocate(ctx, at.file, func() int { return h.takeOwnLocked(r, index, at).slot })
				}
			}
			return fileSlot{}, errors.Join(ErrCapacity, errBothPlacesHeld)
		}
		f := r.privateFile()
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		at, placeable := h.place(r, index)
		h.mu.Unlock()
		if at.slot >= 0 {
			return at, nil
		}
		if placeable {
			return z.host.allocate(ctx, f, func() int {
				at, _ := h.place(r, index)
				return at.slot
			})
		}
		for _, delta := range []int64{-1, 1} {
			neighbour := int64(index) + delta
			if neighbour < 0 || neighbour >= int64(r.pageCount) {
				continue
			}
			if at := z.slotOf(uint64(neighbour)); at.slot >= 0 && at.file == f {
				near := at.plus(-int(delta))
				h.mu.Lock()
				taken := near.slot >= 0 && near.slot < f.slots.Offsets() && f.slots.IsFree(near.slot) && h.takeFree(near, 1)
				h.mu.Unlock()
				if taken {
					return near, nil
				}
			}
		}
		return z.host.allocate(ctx, f, nil)
	})
}

// errBothPlacesHeld is a page of an isolated arena's private file whose two
// places both hold a page.
var errBothPlacesHeld = errors.New("vmmemory: both places of a page of a memory region hold a page")

// newZeroFrames fills the slots of every run, which are slots of f, with
// zeros and reports the frames of each run, as Host.createZeroRuns does. A run
// that fails takes the runs after it and the frames before it with it.
func (z *zirconHost) newZeroFrames(ctx context.Context, f *arenaFile, runs []MapRun, kind MemoryRegionKind) ([][]*zirconvm.VmPage, error) {
	h := z.host
	frames := make([][]*zirconvm.VmPage, 0, len(runs))
	for i, run := range runs {
		at := fileSlot{f, run.Slot}
		var err error
		if zeroing, ok := f.ArenaFile.(ZeroFile); ok {
			err = zeroing.Zero(ctx, at.slot, run.Count)
		} else {
			zeros := make([]byte, h.pageSize)
			for s := at.slot; s < at.slot+run.Count && err == nil; s++ {
				err = f.Write(ctx, s, zeros)
			}
		}
		if err == nil {
			made := make([]*zirconvm.VmPage, run.Count)
			for k := range made {
				made[k] = zirconvm.NewFramePage(&zframe{fileSlot: at.plus(k), kind: kind, own: true})
			}
			frames = append(frames, made)
			continue
		}
		err = h.abandonSlots(ctx, at, run.Count, err)
		for _, made := range frames {
			for _, frame := range made {
				z.releaseFrame(frame)
			}
		}
		for _, rest := range runs[i+1:] {
			err = errors.Join(err, h.abandonSlots(ctx, fileSlot{f, rest.Slot}, rest.Count, nil))
		}
		return nil, err
	}
	return frames, nil
}

// supplyDirty supplies frames, consecutive pages of the region from first, to
// its layer and makes them Dirty there: the pager's supply and its DIRTY
// range op. Nothing of the layer holds them yet.
func (z *zirconRegion) supplyDirty(ctx context.Context, first uint64, frames []*zirconvm.VmPage) error {
	ps := z.region.host.pageSize
	for _, frame := range frames {
		frameOf(frame).own = true
	}
	if err := z.host.supply(ctx, z.layer, first, frames); err != nil {
		return err
	}
	return z.layer.DirtyPages(ctx, first*ps, uint64(len(frames))*ps)
}

// bindDirtyRun binds the pages from first to frames, Dirty pages of the
// layer, each under its reservation, and records them mapped, under one hold
// of each lock for the whole run.
func (z *zirconRegion) bindDirtyRun(first uint64, frames []*zirconvm.VmPage, reservations []reservation, ahead []bool) {
	h := z.region.host
	bindings := make([]*zbinding, len(frames))
	z.mu.Lock()
	for k := range frames {
		b := z.bindingLocked(first + uint64(k))
		b.dirty, b.spill, b.ahead, b.origin, b.zero, b.mapped = true, reservations[k], ahead[k], nil, false, true
		bindings[k] = b
	}
	z.noteDirtyLocked(len(frames))
	z.mu.Unlock()
	h.mu.Lock()
	for k, b := range bindings {
		if b.page != nil {
			z.host.unaliasLocked(b)
		}
		z.host.aliasLocked(b, frames[k])
	}
	h.mu.Unlock()
}

// noteDirtyLocked counts pages the region has made Dirty, and starts its loss
// window where it held none. Caller holds z.mu.
func (z *zirconRegion) noteDirtyLocked(pages int) {
	if z.dirty == 0 && pages > 0 && z.dirtySince.IsZero() {
		z.dirtySince = z.region.host.clock.Now()
	}
	z.dirty += pages
}

// zreplacement is replacement over the zircon core: the pages a store's copy
// replaces the guest's mapping of, kept bound to their bindings, so that no
// idle drop takes one before the command that replaces it lands.
type zreplacement struct {
	z      *zirconRegion
	pages  []*zirconvm.VmPage
	guests []*zbinding
}

// hold keeps the page b mapped before it was copied.
func (p *zreplacement) hold(b *zbinding, page *zirconvm.VmPage) {
	p.pages = append(p.pages, page)
	p.guests = append(p.guests, b)
}

// done gives up every page held, now that the guest maps none of them.
func (p *zreplacement) done() {
	h := p.z.region.host
	h.mu.Lock()
	for k, page := range p.pages {
		p.z.host.releaseHeldLocked(p.guests[k], page)
	}
	h.mu.Unlock()
	p.pages, p.guests = nil, nil
}

// revoke takes the guest's mappings of the held pages away, which a store
// whose command did not land does, and gives the pages up.
func (p *zreplacement) revoke(ctx context.Context) error {
	r := p.z.region
	for _, b := range p.guests {
		if err := r.revokePage(ctx, b.index); err != nil {
			return r.fail(err)
		}
	}
	p.done()
	return nil
}

// releaseHeldLocked takes the alias b kept of page while a store replaced
// it. Caller holds h.mu.
func (z *zirconHost) releaseHeldLocked(b *zbinding, page *zirconvm.VmPage) {
	f := frameOf(page)
	if !f.aliases.remove(b) {
		return
	}
	b.region.region.resident--
	if f.aliases.len() == 0 && !f.own {
		z.host.idlePages++
		z.node.PageQueues().MoveToReclaimDontNeed(page)
	}
}

// takePrivate makes a filled frame page index's own Dirty page, under the
// reservation the store was admitted under, in place of whatever the region
// mapped there, which replaced holds until the store's command lands. origin
// is the root's page the copy was made from, nil where it was made from none.
func (z *zirconRegion) takePrivate(ctx context.Context, index uint64, frame *zirconvm.VmPage, spill reservation,
	origin *zirconvm.VmPage, replaced *zreplacement) error {
	if err := z.supplyDirty(ctx, index, []*zirconvm.VmPage{frame}); err != nil {
		return err
	}
	h := z.region.host
	z.mu.Lock()
	b := z.bindingLocked(index)
	b.dirty, b.spill, b.ahead, b.origin = true, spill, false, origin
	z.noteDirtyLocked(1)
	z.mu.Unlock()
	h.mu.Lock()
	if old := b.page; old != nil {
		// The binding keeps its alias of the page it maps until the store's
		// command replaces it.
		replaced.hold(b, old)
		b.page = nil
	}
	z.host.aliasLocked(b, frame)
	h.mu.Unlock()
	return nil
}

// dirtyInPlace makes the region's own Clean page at index Dirty where it is:
// a page with no identity, which the region read into its own file. A write
// to a page a VMO owns dirties it in place, and so does this one.
func (z *zirconRegion) dirtyInPlace(ctx context.Context, index uint64, spill reservation) error {
	ps := z.region.host.pageSize
	if err := z.layer.DirtyPages(ctx, index*ps, ps); err != nil {
		return err
	}
	z.mu.Lock()
	b := z.bindingLocked(index)
	b.dirty, b.spill, b.ahead, b.origin = true, spill, false, nil
	z.noteDirtyLocked(1)
	z.mu.Unlock()
	return nil
}

// copyOnWrite serves a store into a page the region maps from a root or does
// not hold yet: the page's bytes are copied into a frame of its own, Dirty in
// the layer, and the rules make the pages around it private with it, all of
// it mapped by one command, as MemoryRegion.fault does.
func (z *zirconRegion) copyOnWrite(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	r := z.region
	h := r.host
	unmapped := !z.mapped(index)
	z.mu.Lock()
	b := z.bindingLocked(index)
	z.mu.Unlock()
	h.mu.Lock()
	src := b.page
	h.mu.Unlock()
	if src != nil && frameOf(src).own {
		// The region's own page, Clean, read with no identity: it is dirtied
		// where it is.
		if err := z.dirtyInPlace(ctx, index, *spill); err != nil {
			return false, err
		}
		*spill = noReservation
		at := frameOf(src).fileSlot
		if err := r.mapPages(ctx, r.runAt(index, at, 1), true); err != nil {
			return false, r.mappingFailed(err, func() { z.setMapped(index, index+1, false) })
		}
		z.setMapped(index, index+1, true)
		return false, r.resolvePages(ctx, index, 1, true)
	}
	if src == nil && !z.zeroMapped(index) {
		// The region holds nothing of the page, so the copy has nothing to be
		// made from. Reading it in first is the read fault this store often
		// really is: it lands in its root, so the copy has an origin and every
		// region that inherits the identity maps it rather than reading it.
		origin, again, err := z.readIn(ctx, index)
		if err != nil || again {
			return again, err
		}
		src = origin
	}
	var origin *zirconvm.VmPage
	if src != nil && !frameOf(src).own {
		origin = src
	}
	data := make([]byte, h.pageSize)
	if err := z.readForCopy(ctx, index, src, data); err != nil {
		return false, err
	}
	at, err := z.reclaimPrivate(ctx, index)
	if err != nil {
		return false, err
	}
	frame, err := z.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		return false, err
	}
	frameOf(frame).own = true
	if err := context.Cause(ctx); err != nil {
		// The session the store serves ended while it copied. The copy is
		// reachable from nothing, so it goes back now or never.
		z.host.releaseFrame(frame)
		return false, err
	}
	replaced := &zreplacement{z: z}
	if err := z.takePrivate(ctx, index, frame, *spill, origin, replaced); err != nil {
		z.host.releaseFrame(frame)
		return false, err
	}
	*spill = noReservation
	h.mu.Lock()
	h.stats.CopyOnWrites++
	if unmapped {
		h.stats.UnmappedCopyOnWrites++
	}
	h.mu.Unlock()
	first, last, err := z.closeAround(ctx, index, at, replaced)
	if err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	z.setMapped(first, last, true)
	count := int(last - first)
	if err := r.mapPages(ctx, r.runAt(first, at.plus(-int(index-first)), count), true); err != nil {
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		err = r.mappingFailed(err, func() { z.setMapped(first, last, false) })
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		// The backstop: the client has no mapping left for this store, so the
		// range the guest is writing in is made whole, and the store is served
		// again. The region closes gaps from now on.
		r.pressed.Store(true)
		merged, mergeErr := z.makeWhole(ctx, index)
		if mergeErr != nil {
			return false, mergeErr
		}
		return merged, err
	}
	replaced.done()
	if err := r.resolvePages(ctx, first, count, true); err != nil {
		return false, r.fail(err)
	}
	return false, nil
}

// zeroMapped reports whether a page is mapped to zero.
func (z *zirconRegion) zeroMapped(index uint64) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	b, zeroRun := z.lookupLocked(index)
	return zeroRun || (b != nil && b.zero && b.mapped)
}

// readForCopy fills a store's copy with the page's current bytes: those of
// the page it maps, zeros, or the backing's, read with the region given up.
func (z *zirconRegion) readForCopy(ctx context.Context, index uint64, src *zirconvm.VmPage, dst []byte) error {
	r := z.region
	if src != nil {
		f := frameOf(src)
		return f.file.Read(ctx, f.slot, dst)
	}
	if z.zeroMapped(index) {
		clear(dst)
		return nil
	}
	return r.withoutMemoryRegion(ctx, func() error {
		return r.requested(index, 1, func() error {
			_, err := r.loadBacking(ctx, index*r.host.pageSize, dst)
			return err
		})
	})
}

// readIn gives a store into a page the region holds nothing of something to
// copy from: the page of its identity's root, read in with the faulting
// page's window as a read fault reads it, and bound to the store's binding,
// unmapped, until the copy replaces it. A page with no identity, or a hole,
// has none, and the store reads its copy from the backing. It reports again
// where the fault must look again.
func (z *zirconRegion) readIn(ctx context.Context, index uint64) (*zirconvm.VmPage, bool, error) {
	plan, err := z.planFault(ctx, index, z.region.end(index))
	if err != nil {
		return nil, false, err
	}
	defer plan.unlock()
	plan.store = index
	if id, named := plan.identity(index); !named || id.zero() {
		return nil, false, nil
	}
	if waiter := plan.inFlight(ctx, index); waiter != nil {
		return nil, true, z.awaitRead(ctx, waiter)
	}
	again, err := plan.takeFaulting(ctx, index)
	if err != nil || again {
		return nil, again, err
	}
	if err := plan.read(ctx, index); err != nil {
		return nil, false, err
	}
	origin := plan.pages[index-plan.start]
	if origin == nil {
		// Its root gave it up before the store could bind it; the store
		// decides again from the top.
		return nil, true, nil
	}
	if _, err := plan.install(ctx); err != nil {
		return nil, false, err
	}
	return origin, false, nil
}

// closeAround is MemoryRegion.closeAround over the zircon core.
func (z *zirconRegion) closeAround(ctx context.Context, index uint64, at fileSlot, replaced *zreplacement) (first, last uint64, err error) {
	r := z.region
	h := r.host
	h.mu.Lock()
	placed := h.placedAt(r, index, at)
	first, last = index, index+1
	whole := false
	if placed {
		first, last = z.nearbyLocked(index)
		whole = h.halfPrivate(r, index)
	}
	h.mu.Unlock()
	if !placed {
		return index, index + 1, nil
	}
	if whole {
		span := uint64(h.extentPages)
		first, last = index-index%span, index-index%span+span
		last = min(last, uint64(r.pageCount))
	}
	if last-first == 1 {
		return first, last, nil
	}
	if err := z.takeShared(ctx, first, index, last, replaced); err != nil {
		return 0, 0, err
	}
	if whole {
		h.markWhole(r, index)
	}
	first, last = z.placedRun(index, first, last)
	return first, last, nil
}

// nearbyLocked is Host.nearby over the zircon core. Caller holds h.mu.
func (z *zirconRegion) nearbyLocked(index uint64) (first, last uint64) {
	r := z.region
	h := r.host
	if !r.pressed.Load() {
		return index, index + 1
	}
	span := uint64(h.extentPages)
	e := h.extents[extentKey{r, index / span}]
	low := index - index%span
	high := min(low+span, uint64(r.pageCount))
	first, last = index, index+1
	for d := uint64(1); d <= gapPages && index-d >= low && index >= d; d++ {
		if z.placedPrivateAtLocked(e, index-d) {
			first = index - d + 1
			break
		}
	}
	for d := uint64(1); d <= gapPages && index+d < high; d++ {
		if z.placedPrivateAtLocked(e, index+d) {
			last = index + d
			break
		}
	}
	return first, last
}

// placedPrivateAtLocked is Host.placedPrivateAt over the zircon core: the
// region's own Dirty page, at the offset its range's extent gives it. Caller
// holds h.mu.
func (z *zirconRegion) placedPrivateAtLocked(e *extent, page uint64) bool {
	if e == nil {
		return false
	}
	z.mu.Lock()
	b, _ := z.lookupLocked(page)
	dirty := b != nil && b.dirty
	z.mu.Unlock()
	return dirty && b.page != nil && frameOf(b.page).fileSlot == z.region.host.slotIn(e, page)
}

// placedRun is Host.placedRun over the zircon core.
func (z *zirconRegion) placedRun(index, first, last uint64) (uint64, uint64) {
	r := z.region
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.extents[extentKey{r, index / uint64(h.extentPages)}]
	start, end := index, index+1
	for start > first && z.placedPrivateAtLocked(e, start-1) {
		start--
	}
	for end < last && z.placedPrivateAtLocked(e, end) {
		end++
	}
	return start, end
}

// joinsRun reports a page one store's command may cover.
func (z *zirconRegion) joinsRun(page uint64) bool {
	r := z.region
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return z.placedPrivateAtLocked(h.extents[extentKey{r, page / uint64(h.extentPages)}], page)
}

// takeShared is MemoryRegion.takeShared over the zircon core.
func (z *zirconRegion) takeShared(ctx context.Context, first, index, last uint64, replaced *zreplacement) error {
	for page := index + 1; page < last; page++ {
		joined, err := z.takeOneShared(ctx, page, replaced)
		if err != nil {
			return err
		}
		if !joined {
			break
		}
	}
	for page := index; page > first; page-- {
		joined, err := z.takeOneShared(ctx, page-1, replaced)
		if err != nil {
			return err
		}
		if !joined {
			break
		}
	}
	return nil
}

// takeOneShared is MemoryRegion.takeOneShared over the zircon core: one page
// made private for a rule, from bytes this host holds, at the offset of its
// own, never waiting and never evicting.
func (z *zirconRegion) takeOneShared(ctx context.Context, page uint64, replaced *zreplacement) (bool, error) {
	r := z.region
	h := r.host
	if z.writable(page) {
		return z.joinsRun(page), nil
	}
	z.mu.Lock()
	b := z.bindingLocked(page)
	z.mu.Unlock()
	h.mu.Lock()
	src := b.page
	h.mu.Unlock()
	if src == nil && !z.zeroMapped(page) {
		// This host does not hold the page's bytes, and a rule reads nothing.
		return false, nil
	}
	if src != nil && frameOf(src).own {
		// The region's own page with no identity, which is not at its own
		// offset: the run ends here.
		return false, nil
	}
	spill, err := h.tryTakeSpill()
	if err != nil {
		if errors.Is(err, ErrCapacity) {
			return false, nil
		}
		return false, err
	}
	data := make([]byte, h.pageSize)
	if err := z.readForCopy(ctx, page, src, data); err != nil {
		h.releaseSpill(spill)
		return false, err
	}
	h.mu.Lock()
	at, _ := h.place(r, page)
	h.mu.Unlock()
	if at.slot < 0 {
		// The offset of this page's own is not to be had, and a rule never
		// evicts for a page the guest did not write.
		h.releaseSpill(spill)
		return false, nil
	}
	frame, err := z.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		h.releaseSpill(spill)
		return false, err
	}
	frameOf(frame).own = true
	var origin *zirconvm.VmPage
	if src != nil {
		origin = src
	}
	if err := z.takePrivate(ctx, page, frame, spill, origin, replaced); err != nil {
		z.host.releaseFrame(frame)
		h.releaseSpill(spill)
		return false, err
	}
	h.mu.Lock()
	h.stats.RuleCopies++
	h.mu.Unlock()
	return true, nil
}

// makeWhole is MemoryRegion.makeWhole over the zircon core: the mapping
// budget's backstop, which copies every page of one range into the holes of
// its extent and maps the range with one command.
func (z *zirconRegion) makeWhole(ctx context.Context, index uint64) (bool, error) {
	r := z.region
	h := r.host
	span := uint64(h.extentPages)
	h.mu.Lock()
	placed := h.extents[extentKey{r, index / span}] != nil
	h.mu.Unlock()
	if !placed {
		return false, nil
	}
	first := index - index%span
	last := min(first+span, uint64(r.pageCount))
	before := z.privatePages(first, last)
	replaced := &zreplacement{z: z}
	if err := z.takeShared(ctx, first, index, last, replaced); err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	if z.privatePages(first, last) <= before {
		replaced.done()
		return false, nil
	}
	h.markWhole(r, index)
	first, last = z.placedRun(index, first, last)
	h.mu.Lock()
	at := h.placedSlot(r, first)
	h.stats.MappingMerges++
	h.mu.Unlock()
	count := int(last - first)
	z.setMapped(first, last, true)
	if err := r.mapPages(ctx, r.runAt(first, at, count), true); err != nil {
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		return false, r.mappingFailed(err, func() { z.setMapped(first, last, false) })
	}
	replaced.done()
	return true, nil
}

// privatePages is how many pages of [first, last) the region may store into
// where they are.
func (z *zirconRegion) privatePages(first, last uint64) int {
	count := 0
	for page := first; page < last; page++ {
		if z.writable(page) {
			count++
		}
	}
	return count
}

// releaseDirty gives back the reservations of every page the region has
// stored into, which a detach does: its writes go with it.
func (z *zirconRegion) releaseDirty() {
	h := z.region.host
	var spills []reservation
	z.mu.Lock()
	z.eachBoundLocked(0, uint64(z.region.pageCount), func(b *zbinding) {
		if b.dirty && !b.spill.none() {
			spills = append(spills, b.spill)
		}
		b.dirty, b.spill, b.origin, b.ahead = false, noReservation, nil, false
	})
	z.dirty, z.dirtySince = 0, time.Time{}
	z.mu.Unlock()
	for _, spill := range spills {
		h.releaseSpill(spill)
	}
}
