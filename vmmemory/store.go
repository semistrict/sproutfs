package vmmemory

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A store makes a page of the region's own layer Dirty, as a write makes a
// page of a VMO a user pager backs Dirty: the pager fills a frame with the
// bytes the page holds now, at the offset of the region's private file the
// placement rule gives it, supplies it to the layer, and makes it Dirty there
// (zirconvm DirtyPages, zx_pager_op_range's DIRTY). The layer's page then
// shadows the identity root's, as a snapshot-on-write child's copy shadows its
// parent's. A store into fresh zeros makes its whole write-ahead run Dirty at
// once. Which pages a store makes private, write-ahead, placement and the two
// rules are the pager's own (placement.go, rules.go).
//
// The dirty reservation a page is admitted under is kept in its binding beside
// the layer, which the spill writes the page's bytes to (D5, evict.go).

// writable reports whether the guest may store into a page where it is: the
// region's own Dirty page.
func (r *MemoryRegion) writable(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b != nil && b.writable()
}

// holdsOwn reports whether the region's own layer holds a page at index,
// whatever the volume says is there.
func (r *MemoryRegion) holdsOwn(index uint64) bool {
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(index)
	r.bindingsMu.Unlock()
	if b == nil {
		return false
	}
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return b.page != nil && frameOf(b.page).layer == r
}

// needsPrivatePage reports whether a store to index would have to make a page
// of its own, and with it take a dirty reservation. It takes no region lock:
// a store decides this before it competes for one, and decides again after.
func (r *MemoryRegion) needsPrivatePage(index uint64) bool {
	return !r.writable(index) && !r.journalProtected(index)
}

// fresh reports what a store may assume about a page whose lock it does not
// hold: zero where the page is mapped to zero, untouched where the region
// holds nothing of it at all, no mapping included, so that its bytes are
// whatever the volume holds. Either way it owns no memory, no private state
// and no checkpoint, and nothing needs fencing before a private page takes its
// place. Caller holds the page's fault stripe and the region shared.
//
// The three looks are three holds, and none goes stale between them. The
// stripe keeps out every path that binds the page or makes it private: a
// fault, a prefetch's mapping, a give-back and a rule. A seal, a retire and a
// capture need the region exclusively. An eviction only takes a page away,
// and a page with none that is dirty or the checkpoint's is not fresh.
func (r *MemoryRegion) fresh(index uint64) (zero, untouched bool) {
	r.bindingsMu.Lock()
	b, zeroRun := r.lookupLocked(index)
	r.bindingsMu.Unlock()
	if zeroRun {
		return true, false
	}
	if b == nil {
		return false, true
	}
	h := r.host
	h.mu.Lock()
	bound := b.page != nil
	h.mu.Unlock()
	if bound {
		return false, false
	}
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if b.dirty || b.checkpoint != nil {
		return false, false
	}
	return b.zero, !b.zero && !b.mapped
}

// store serves a store into a page the region may not store into yet.
func (r *MemoryRegion) store(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	if fresh, err := r.storeFresh(ctx, index, spill); fresh || err != nil {
		return false, err
	}
	return r.copyOnWrite(ctx, index, spill)
}

// storeFresh serves a store into a page whose bytes are known zeros and that
// the region holds nothing of: a zero-mapped page, or a hole in the volume the
// guest has never touched. For any other page it reports false having done
// nothing. There is no memory to copy and nothing to fence, so nothing is
// revoked: one mapping command puts fresh pages where the zeros or the trap
// were. Write-ahead makes the fresh zero pages around it private in that same
// command.
func (r *MemoryRegion) storeFresh(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	zero, untouched := r.fresh(index)
	if !zero && !untouched {
		return false, nil
	}
	var plan *plan
	if untouched {
		// Whether an untouched page is a hole is volume metadata, which the
		// window's extents answer for its neighbours too; the page is asked
		// first, alone.
		start, end := r.window(index)
		var err error
		if plan, err = r.planPage(ctx, start, end, index, index); err != nil {
			return false, err
		}
		if id, ok := plan.identity(index); !ok || !id.zero() {
			return false, nil
		}
		if err := plan.locateWindow(ctx); err != nil {
			return false, err
		}
	}
	first, last := r.zeroRun(index, plan)
	return true, r.storeZeros(ctx, index, first, last, spill)
}

// zeroRun bounds the run a store into index makes private: index, the fresh
// zero pages after it up to the end of its read-ahead window, then those
// before it, at most WriteAheadPages together. A page is fresh zeros when it
// is zero-mapped or, by the plan's extents when the store has them, an
// untouched hole.
func (r *MemoryRegion) zeroRun(index uint64, plan *plan) (uint64, uint64) {
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

// storeZeros makes the fresh zero pages [first, last), which hold index,
// become Dirty pages of the layer in fresh frames, mapped writable. The
// faulting page brings its own dirty reservation; the rest of the run takes
// only free reservations and free slots, and shrinks to what it finds.
func (r *MemoryRegion) storeZeros(ctx context.Context, index, first, last uint64, spill *reservation) error {
	h := r.host
	extras := h.takeFreeSpill(int(last-first) - 1)
	used := 0
	defer func() {
		for _, extra := range extras[used:] {
			h.releaseSpill(extra)
		}
	}()
	first, last = around(index, first, last, 1+len(extras))
	// allocateRun may give the region up, where index takes the one slot that
	// may evict (reclaimPrivate). The run is still fresh after: a seal, a
	// retire, an unseal and a capture change only pages that are dirty or the
	// checkpoint's, and every path that makes a page of the window private or
	// binds it holds the window's stripe, as this store does.
	first, runs, err := r.allocateRun(ctx, index, first, last)
	if err != nil {
		return err
	}
	count := 0
	for _, run := range runs {
		count += run.Count
	}
	frames, err := r.newZeroFrames(ctx, r.privateFile(), runs)
	if err != nil {
		return err
	}
	// The run's pages are held from their making until their commands land,
	// so no eviction takes one between the binding and the command. The
	// signal's hold and the stats' below share nothing.
	defer func() {
		for _, run := range frames {
			for _, frame := range run {
				frameOf(frame).mu.Unlock()
			}
		}
		h.mu.Lock()
		h.signal()
		h.mu.Unlock()
	}()
	// The run is supplied and made Dirty a run of slots at a time, and bound a
	// lock at a time for the whole of it: a boot's vCPUs fault runs of
	// thousands of pages at once.
	page := first
	for k, run := range runs {
		if err := r.supplyDirty(ctx, page, frames[k]); err != nil {
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
	r.bindDirtyRun(first, all, reservations, ahead)
	if h.measuring() {
		for k := range count {
			r.noteZeroed(first + uint64(k))
		}
	}
	h.mu.Lock()
	h.stats.CopyOnWrites++
	h.stats.WriteAheadPages += uint64(count - 1)
	h.mu.Unlock()
	for _, run := range runs {
		if err := r.mapPages(ctx, run, true); err != nil {
			return r.mappingFailed(err, func() { r.setMapped(first, first+uint64(count), false) })
		}
	}
	for _, run := range runs {
		if err := r.resolvePages(ctx, run.Page, run.Count, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// allocateRun takes slots of the region's private file for the run [first,
// last), which holds index, by the placement rule first, then free consecutive
// slots, then index alone, which is the one slot that may give up idle pages
// for it.
func (r *MemoryRegion) allocateRun(ctx context.Context, index, first, last uint64) (uint64, []MapRun, error) {
	h := r.host
	f := r.privateFile()
	if err := r.host.makeRoom(ctx, f, int(last-first)); err != nil {
		return 0, nil, err
	}
	h.mu.Lock()
	noExtent := h.carving(f) && f.slots.FreeExtents() == 0 && h.extents[extentKey{r, index / uint64(h.extentPages)}] == nil
	if f.owner != nil {
		// Every page of a private file's run is placed at once, so the run is
		// what the pager has room for, and never less than the faulting page.
		first, last = around(index, first, last, max(h.freeLocked(f), 1))
	}
	h.mu.Unlock()
	if noExtent {
		r.host.reclaimExtent(f)
	}
	if start, runs := h.placeRun(r, index, first, last); len(runs) > 0 {
		return start, runs, nil
	}
	if last-first > 1 && f.owner == nil {
		prefer := fileSlot{f, -1}
		if first > 0 {
			if at := r.slotOf(first - 1); at.slot >= 0 && at.file == f {
				prefer = at.plus(1)
			}
		}
		if at, count := h.allocateFreeFrom(prefer, int(last-first)); count > 0 {
			start, _ := around(index, first, last, count)
			return start, []MapRun{r.runAt(start, at, count)}, nil
		}
	}
	at, err := r.reclaimPrivate(ctx, index)
	if err != nil {
		return 0, nil, err
	}
	return index, []MapRun{r.runAt(index, at, 1)}, nil
}

// slotOf is the slot of the page the region maps at index, slot -1 where it
// maps none.
func (r *MemoryRegion) slotOf(index uint64) fileSlot {
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(index)
	r.bindingsMu.Unlock()
	if b == nil {
		return fileSlot{slot: -1}
	}
	h := r.host
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
// neighbours where it has none, evicting where the arena is full.
//
// The caller's page is held by its stripe, so nothing but this reclaim takes
// the page's own offset, and what the caller decided about the page is what a
// seal, a retire or an abandon taken while the region is given up may change:
// the caller decides that again once it is back (copyOnWrite, refault).
func (r *MemoryRegion) reclaimPrivate(ctx context.Context, index uint64) (fileSlot, error) {
	h := r.host
	return r.reclaimWith(ctx, func() (fileSlot, error) {
		if reclaimSeam != nil {
			reclaimSeam(index)
		}
		// In a controlled run another task may go on here, with the region
		// given up: a seal, a retire, a capture, or a fault of another window.
		if err := sim.Admit(ctx, "vmmemory/reclaim-private"); err != nil {
			return fileSlot{}, err
		}
		if err := context.Cause(ctx); err != nil {
			return fileSlot{}, err
		}
		if r.private != nil {
			return r.allocateOwn(ctx, index, false)
		}
		f := r.privateFile()
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return fileSlot{}, err
		}
		at, placeable := h.place(r, index)
		noExtent := !placeable && h.carving(f) && f.slots.FreeExtents() == 0
		h.mu.Unlock()
		if at.slot >= 0 {
			return at, nil
		}
		// noExtent and placeable are a look, which may be out of date once h.mu
		// is given up: another region may take or give back an extent. Each
		// step after looks again under the hold it acts in, and a look out of
		// date costs a page an offset beside its neighbours rather than its
		// own, never a slot.
		if noExtent && r.host.reclaimExtent(f) {
			// The file's extents were held by the idle pages of regions that
			// have gone; one was given back for this range.
			h.mu.Lock()
			at, placeable = h.place(r, index)
			h.mu.Unlock()
			if at.slot >= 0 {
				return at, nil
			}
		}
		if placeable {
			return h.allocate(ctx, r, f, func() int {
				at, placeable := h.place(r, index)
				if at.slot < 0 && !placeable && !sim.Bug(ctx, "pager-wait-for-an-offset-gone-elsewhere") {
					// The offset stopped being this page's to have while it
					// waited for room: another range took the last extent.
					// What it waits for will not come, so it takes an
					// ordinary slot, as it would have had it looked then.
					if slot := h.firstFreeLocked(f); slot >= 0 && h.takeFree(fileSlot{f, slot}, 1) {
						return slot
					}
				}
				return at.slot
			}, evictPastAFreeSlot(ctx))
		}
		for _, delta := range []int64{-1, 1} {
			neighbour := int64(index) + delta
			if neighbour < 0 || neighbour >= int64(r.pageCount) {
				continue
			}
			if at := r.slotOf(uint64(neighbour)); at.slot >= 0 && at.file == f {
				near := at.plus(-int(delta))
				h.mu.Lock()
				taken := near.slot >= 0 && near.slot < f.slots.Offsets() && f.slots.IsFree(near.slot) && h.takeFree(near, 1)
				h.mu.Unlock()
				if taken {
					return near, nil
				}
			}
		}
		return h.allocate(ctx, r, f, nil, evictPastAFreeSlot(ctx))
	})
}

// newZeroFrames fills the slots of every run, which are slots of f, with
// zeros and reports the frames of each run, as Host.createZeroRuns does. A run
// that fails takes the runs after it and the frames before it with it.
func (r *MemoryRegion) newZeroFrames(ctx context.Context, f *arenaFile, runs []MapRun) ([][]*zirconvm.VmPage, error) {
	h := r.host
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
				made[k] = zirconvm.NewFramePage(makeLockedFrame(at.plus(k), r.kind, r))
				r.host.noteFrame(made[k])
			}
			frames = append(frames, made)
			continue
		}
		err = h.abandonSlots(ctx, at, run.Count, err)
		for _, made := range frames {
			for _, frame := range made {
				r.host.releaseFrame(frame)
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
func (r *MemoryRegion) supplyDirty(ctx context.Context, first uint64, frames []*zirconvm.VmPage) error {
	ps := r.host.pageSize
	for _, frame := range frames {
		frameOf(frame).layer = r
	}
	if err := r.host.supply(ctx, r.layer, first, frames); err != nil {
		return err
	}
	return r.layer.DirtyPages(ctx, first*ps, uint64(len(frames))*ps)
}

// bindDirtyRun binds the pages from first to frames, Dirty pages of the
// layer, each under its reservation, and records them mapped, under one hold
// of each lock for the whole run.
func (r *MemoryRegion) bindDirtyRun(first uint64, frames []*zirconvm.VmPage, reservations []reservation, ahead []bool) {
	h := r.host
	bindings := make([]*binding, len(frames))
	r.bindingsMu.Lock()
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*binding)
	}
	for k := range frames {
		b := r.bindingLocked(first + uint64(k))
		r.uncoldLocked(b)
		b.dirty, b.spill, b.ahead, b.origin, b.zero, b.mapped = true, reservations[k], ahead[k], nil, false, true
		b.checkpoint, b.zeroed = nil, true
		bindings[k] = b
		r.dirtySet[b.index] = b
		r.noteStoredLocked(b)
	}
	if r.dirtySince.IsZero() && len(frames) > 0 {
		r.dirtySince = r.host.clock.Now()
	}
	// A run of fresh pages no checkpoint holds is one sealable run: one
	// change to the runs a seal reads rather than one per page.
	r.dirtyRuns.add(first, first+uint64(len(frames)))
	r.bindingsMu.Unlock()
	for k, b := range bindings {
		h.probe.granted(b, frameOf(frames[k]), nil)
	}
	h.mu.Lock()
	for k, b := range bindings {
		if b.page != nil {
			r.host.unaliasLocked(b)
		}
		r.host.aliasLocked(b, frames[k])
	}
	h.mu.Unlock()
}

// takePrivate makes a filled frame page index's own Dirty page, under the
// reservation the store was admitted under, in place of whatever the region
// mapped there, which replaced holds until the store's command lands. origin
// is the root's page the copy was made from, nil where it was made from none.
//
// A page the guest shares with a checkpoint is AwaitingClean in the layer,
// and the copy splits it (D1): the copy is the Dirty page at index, and the
// checkpoint keeps the page of its pause beside the page list, with the
// reservation it was admitted under. Any other page of the layer's index is
// empty, and the copy is supplied there and made Dirty.
func (r *MemoryRegion) takePrivate(ctx context.Context, index uint64, frame *zirconvm.VmPage, spill reservation,
	origin *zirconvm.VmPage, replaced *replacement) error {
	h := r.host
	ps := h.pageSize
	frameOf(frame).layer = r
	r.bindingsMu.Lock()
	b := r.bindingLocked(index)
	r.bindingsMu.Unlock()
	// b is what the caller decided to copy away from, and nothing changes that
	// while bindingsMu is given up here and below. The caller holds the
	// page's stripe, which keeps out every other path that makes the page
	// private or binds it: a fault, a prefetch's mapping, a give-back and a
	// rule (takeOneShared takes the stripe of a page in another window). A
	// seal, a retire and a capture need the region, which the caller holds
	// shared. An eviction only takes b.page away, and lockedPage looks again
	// for that.
	//
	// The page the guest maps is held across the change, so no eviction
	// revokes the guest's mapping of it after the store's command put the copy
	// there.
	old, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return err
	}
	if old != nil {
		defer r.host.unlockPage(old)
	}
	err = r.layer.SplitAwaitingClean(index*ps, frame)
	if errors.Is(err, zirconvm.ErrBadState) {
		err = r.supplyDirty(ctx, index, []*zirconvm.VmPage{frame})
	}
	if err != nil {
		return err
	}
	r.bindingsMu.Lock()
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.zero, b.ahead, b.origin = nil, spill, true, false, false, origin
	b.zeroed = false
	r.noteDirtyLocked(b)
	r.bindingsMu.Unlock()
	h.probe.granted(b, frameOf(frame), probeFrame(origin))
	if h.measuring() {
		if err := r.noteCopiedAt(ctx, index, frameOf(frame).fileSlot); err != nil {
			return err
		}
	}
	// b.page is still old here, and b.mapped what it was: the eviction that
	// could change either takes old's lock, which this store holds, and the
	// copy is held by the store and aliased by nothing yet.
	h.mu.Lock()
	if old := b.page; old != nil {
		// The guest goes on reading the page it maps until the store's command
		// replaces it, so it stays where it is until then.
		r.bindingsMu.Lock()
		mapped := b.mapped
		r.bindingsMu.Unlock()
		if mapped {
			replaced.holdLocked(b, old)
		}
		r.host.unaliasLocked(b)
	}
	r.host.aliasLocked(b, frame)
	h.mu.Unlock()
	return nil
}

// dirtyInPlace makes the region's own Clean page at index Dirty where it is:
// a page with no identity, which the region read into its own file. A write
// to a page a VMO owns dirties it in place, and so does this one.
func (r *MemoryRegion) dirtyInPlace(ctx context.Context, index uint64, spill reservation) error {
	ps := r.host.pageSize
	if err := r.layer.DirtyPages(ctx, index*ps, ps); err != nil {
		return err
	}
	r.bindingsMu.Lock()
	b := r.bindingLocked(index)
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.ahead, b.origin, b.zeroed = nil, spill, true, false, nil, false
	r.noteDirtyLocked(b)
	r.bindingsMu.Unlock()
	h := r.host
	h.mu.Lock()
	page := b.page
	h.mu.Unlock()
	h.probe.granted(b, probeFrame(page), nil)
	return nil
}

// checkpointCopy reports the checkpoint's copy b shares, nil where it holds
// its own state.
func (r *MemoryRegion) checkpointCopy(b *binding) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.checkpoint
}

// copyOnWrite serves a store into a page the region maps from a root or does
// not hold yet: the page's bytes are copied into a frame of its own, Dirty in
// the layer, and the rules make the pages around it private with it, all of
// it mapped by one command, as MemoryRegion.fault does.
func (r *MemoryRegion) copyOnWrite(ctx context.Context, index uint64, spill *reservation) (bool, error) {
	h := r.host
	unmapped := !r.mapped(index)
	r.bindingsMu.Lock()
	b := r.bindingLocked(index)
	held := b.checkpoint
	r.bindingsMu.Unlock()
	// The page copied from is held while its bytes are read, so no eviction
	// takes it from under the copy.
	src, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	readIn := false
	if src == nil && held == nil && !r.zeroMapped(index) {
		// The region holds nothing of the page, so the copy has nothing to be
		// made from. Reading it in first is the read fault this store often
		// really is: it lands in its root, so the copy has an origin and every
		// region that inherits the identity maps it rather than reading it.
		origin, again, err := r.readIn(ctx, index)
		if err != nil || again {
			return again, err
		}
		src, readIn = origin, true
	}
	// A page read in may land in the region's own file rather than a root: a
	// page a fork point names is read there, since the name lasts only as
	// long as the point's seal. It is then the region's own Clean page, as
	// one with no identity is, and the layer holds it, so it is dirtied where
	// it is: a copy's supply would lose to it.
	inPlace := !readIn || !sim.Bug(ctx, "pager-copy-a-page-read-into-its-own-file")
	if src != nil && frameOf(src).layer == r && held == nil && inPlace {
		// The region's own page, Clean, read with no identity: it is dirtied
		// where it is.
		defer r.host.unlockPage(src)
		if err := r.dirtyInPlace(ctx, index, *spill); err != nil {
			return false, err
		}
		*spill = noReservation
		at := frameOf(src).fileSlot
		r.setMapped(index, index+1, true)
		if err := r.mapPages(ctx, r.runAt(index, at, 1), true); err != nil {
			return false, r.mappingFailed(err, func() { r.setMapped(index, index+1, false) })
		}
		if err := r.resolvePages(ctx, index, 1, true); err != nil {
			return false, r.fail(err)
		}
		return false, nil
	}
	// A copy of a root's page remembers it: its bytes cannot change while the
	// root holds it, so a settle can tell a page the guest stored into from
	// one a write fault merely took writable. A copy of the checkpoint's has
	// none, as a copy of any page of the region's own has none.
	var origin *zirconvm.VmPage
	if src != nil && frameOf(src).layer == nil {
		origin = src
	}
	// A store trap's copy of a root's page is cold, which pins the page it
	// was copied from, here, while that page is still held: see cold.go.
	marked := false
	if unmapped && origin != nil {
		r.host.pin(origin, b)
		defer func() {
			if !marked {
				r.host.unpin(origin, b)
			}
		}()
	}
	data := make([]byte, h.pageSize)
	unpublished, err := r.readForCopy(ctx, index, src, held, data)
	if src != nil {
		// The bytes are read: the page may go from here, the copy holding
		// them, and the slot the copy needs may be its.
		r.host.unlockPage(src)
	}
	if err != nil {
		return false, err
	}
	// The region is given up for the reclaim: a seal, a retire or an abandon
	// taken meanwhile is what the page's being the checkpoint's or not was
	// decided against, so it is decided again from the top. The checkpoint's
	// copy is the one thing of b they change: a page that is not dirty is
	// sealed by nothing and captured by nothing, and a retire or an abandon
	// of a page held ends its holding. Nothing else changes b meanwhile: the
	// page's stripe is held (takePrivate).
	at, err := r.reclaimPrivate(ctx, index)
	if err != nil {
		return false, err
	}
	if r.checkpointCopy(b) != held {
		return true, h.abandonSlots(ctx, at, 1, nil)
	}
	frame, err := r.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		return false, err
	}
	frameOf(frame).layer = r
	// The copy is held from its making until the store's command lands.
	replaced := &replacement{region: r}
	defer replaced.unlock()
	replaced.keep(frame)
	if err := context.Cause(ctx); err != nil {
		// The session the store serves ended while it copied. The copy is
		// reachable from nothing, so it goes back now or never.
		r.host.releaseFrame(frame)
		replaced.done()
		return false, err
	}
	if err := r.takePrivate(ctx, index, frame, *spill, origin, replaced); err != nil {
		r.host.releaseFrame(frame)
		replaced.done()
		return false, err
	}
	*spill = noReservation
	if unpublished {
		// The bytes came from the host that still holds them, and this store
		// has just made them the region's own: that host no longer holds the
		// only copy, and its backing is told so.
		r.installedUnpublished(index*h.pageSize, []bool{true})
	}
	h.mu.Lock()
	h.stats.CopyOnWrites++
	if unmapped {
		h.stats.UnmappedCopyOnWrites++
	}
	h.mu.Unlock()
	first, last, err := r.closeAround(ctx, index, at, replaced)
	if err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	r.setMapped(first, last, true)
	count := int(last - first)
	if err := r.mapPages(ctx, r.runAt(first, at.plus(-int(index-first)), count), true); err != nil {
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		err = r.mappingFailed(err, func() { r.setMapped(first, last, false) })
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		// The backstop: the client has no mapping left for this store, so the
		// range the guest is writing in is made whole, and the store is served
		// again. The region closes gaps from now on.
		r.pressed.Store(true)
		merged, mergeErr := r.makeWhole(ctx, index, replaced)
		if mergeErr != nil {
			return false, mergeErr
		}
		return merged, err
	}
	replaced.done()
	if err := r.resolvePages(ctx, first, count, true); err != nil {
		return false, r.fail(err)
	}
	if unmapped && origin != nil {
		marked = r.markCold(b, origin)
	}
	return false, nil
}

// reclaimExtent gives up idle pages until an extent of f is free, where none
// is, and reports whether it freed one. A published page stays at the slot the
// placement rule gave it when it goes idle, so it keeps that slot's extent
// from going back after the region that placed it has gone: a file full of the
// idle pages of stopped VMs would otherwise have no extent left for a running
// one.
func (h *Host) reclaimExtent(f *arenaFile) bool {
	orphaned := func(frame *frame) bool {
		e := frame.file.leases[frame.slot].extent
		return e != nil && e.file == f && h.extents[e.key] != e
	}
	// The look for a free extent and the drop are two holds, and either may
	// be out of date by the other: another region may take or give back an
	// extent, or map the idle page. takeIdleIf looks at its page again under
	// its own hold and its page's lock, so it drops only a page still idle
	// and orphaned; a drop no longer needed costs an idle page of a region
	// that has gone, and a look that missed an extent coming free costs the
	// caller a page beside its neighbours rather than at its own offset. The
	// caller places under a hold of its own after.
	for {
		h.mu.Lock()
		if !h.carving(f) || f.slots.FreeExtents() > 0 {
			freed := h.carving(f) && f.slots.FreeExtents() > 0
			h.mu.Unlock()
			return freed
		}
		h.mu.Unlock()
		if !h.takeIdleIf(func() bool { h.mu.Lock(); return true }, orphaned) {
			return false
		}
	}
}

// zeroMapped reports whether a page is mapped to zero.
func (r *MemoryRegion) zeroMapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, zeroRun := r.lookupLocked(index)
	return zeroRun || (b != nil && b.zero && b.mapped)
}

// readForCopy fills a store's copy with the page's current bytes: those of
// the page it maps, zeros, or the backing's, read with the region given up.
//
// It reports whether the bytes are ones no checkpoint of the VM has: only a
// backing read can say so, and only a backing that fetches from another host
// ever does.
func (r *MemoryRegion) readForCopy(ctx context.Context, index uint64, src *zirconvm.VmPage, held *binding,
	dst []byte) (unpublished bool, err error) {
	if src != nil {
		f := frameOf(src)
		return false, f.file.Read(ctx, f.slot, dst)
	}
	if held != nil {
		// The checkpoint's copy the page shares was spilled: its reservation
		// holds the bytes.
		return false, r.host.readSpill(ctx, r.spillOf(held), dst)
	}
	if r.zeroMapped(index) {
		clear(dst)
		return false, nil
	}
	err = r.withoutMemoryRegion(ctx, func() error {
		return r.requested(index, 1, func() error {
			fetched, err := r.loadBacking(ctx, index*r.host.pageSize, dst)
			unpublished = len(fetched) > 0 && fetched[0]
			return err
		})
	})
	return unpublished, err
}

// readIn gives a store into a page the region holds nothing of something to
// copy from: the page of its identity's root, read in with the faulting
// page's window as a read fault reads it, and bound to the store's binding,
// unmapped, until the copy replaces it. A page with no identity, or a hole,
// has none, and the store reads its copy from the backing. It reports again
// where the fault must look again.
//
// A peer backing's store reads its page alone: a load is what answers that the
// source still holds a page, which is per page, and nothing may be shared
// under the name of a page it gives that answer for.
func (r *MemoryRegion) readIn(ctx context.Context, index uint64) (*zirconvm.VmPage, bool, error) {
	var plan *plan
	var err error
	if r.peer {
		if plan, err = r.plan(ctx, index, index+1, r.end(index)); err == nil {
			plan.reading = readAlone
		}
	} else {
		plan, err = r.planFault(ctx, index, r.end(index))
	}
	if err != nil {
		return nil, false, err
	}
	defer plan.unlock()
	plan.store = index
	if id, named := plan.identity(index); !named || id.zero() {
		return nil, false, nil
	}
	if waiter := plan.inFlight(ctx, index); waiter != nil {
		return nil, true, r.awaitRead(ctx, waiter, index)
	}
	// The look for a read under way and the lookup are two holds, as a read
	// fault's are (loadOnce). A prefetch of another region of the same root
	// may send its read of the page between them. The lookup then finds the
	// root holding nothing there and the store reads the page itself: a
	// second read, whose supply keeps whichever page reached the root first.
	// It costs a read and loses nothing. In a controlled run another task may
	// go on here.
	if err := sim.Admit(ctx, "vmmemory/store-read-in"); err != nil {
		return nil, false, err
	}
	again, err := plan.takeFaulting(ctx, index)
	if err != nil || again {
		return nil, again, err
	}
	if err := plan.read(ctx, index); errors.Is(err, errUnpublishedReservation) {
		// The extents named the page the volume's and the load found the
		// source still holding it: the store decides again from the top.
		return nil, true, nil
	} else if err != nil {
		return nil, false, err
	}
	if plan.private[index-plan.start] {
		// The load took the page as the region's own dirty state: the store
		// decides again from the top, and finds it writable.
		return nil, true, nil
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
	// The caller holds the origin from here; the plan gives back the rest.
	plan.release(origin)
	return origin, false, nil
}

// closeAround is what a store does after the page it faulted on is private: it
// makes the shared pages the two rules name private too and reports the run of
// pages the store maps, which is one mapping command because the whole of it
// sits at consecutive offsets of one extent. A store the placement rule had no
// offset for is its own page and nothing else: without the extent there is no
// run to be part of.
func (r *MemoryRegion) closeAround(ctx context.Context, index uint64, at fileSlot, replaced *replacement) (first, last uint64, err error) {
	h := r.host
	h.mu.Lock()
	placed := h.placedAt(r, index, at)
	first, last = index, index+1
	whole := false
	if placed {
		first, last = r.nearbyLocked(index)
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
	first, last, err = r.takeShared(ctx, first, index, last, replaced)
	if err != nil {
		return 0, 0, err
	}
	if whole {
		h.markWhole(r, index)
	}
	return first, last, nil
}

// nearbyLocked reports the pages one store makes private by the gap rule: the
// faulting page, and the pages between it and the nearest page of its range
// the guest already stores into at that page's own offset, where that page is
// within gapPages. Caller holds h.mu.
func (r *MemoryRegion) nearbyLocked(index uint64) (first, last uint64) {
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
		if r.placedPrivateAtLocked(e, index-d) {
			first = index - d + 1
			break
		}
	}
	for d := uint64(1); d <= gapPages && index+d < high; d++ {
		if r.placedPrivateAtLocked(e, index+d) {
			last = index + d
			break
		}
	}
	return first, last
}

// placedPrivateAtLocked reports the one thing every rule asks of a page: that
// the region may store into it where the placement rule put it. That is its
// own Dirty page, held by no checkpoint, at the offset its range's extent
// gives it, which is exactly what one store's mapping command can cover, so a
// run ends at the first page that is not it. The offset alone answers nothing:
// a retire leaves a page published at the offset it was sealed at, and reading
// that offset as the page's own would put a run's mapping over a page the
// region no longer stores into. Caller holds h.mu.
func (r *MemoryRegion) placedPrivateAtLocked(e *extent, page uint64) bool {
	if e == nil {
		return false
	}
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(page)
	dirty := b != nil && b.writable()
	r.bindingsMu.Unlock()
	return dirty && b.page != nil && frameOf(b.page).fileSlot == r.host.slotIn(e, page)
}

// joinsRun reports a page one store's command may cover.
func (r *MemoryRegion) joinsRun(page uint64) bool {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return r.placedPrivateAtLocked(h.extents[extentKey{r, page / uint64(h.extentPages)}], page)
}

// takeShared makes the pages either side of the faulting one the region's own
// dirty state, at the offset the placement rule gives each. It works outward
// from the faulting page and stops on each side at the first page that cannot
// join the store's run, and reports the run it took, [lo, hi), which holds
// index: exactly what the caller's one mapping command covers. Every page of
// it is held until that command lands, so the run is still what it was when
// the command maps it. It never waits, never evicts and reads nothing this
// host does not hold.
func (r *MemoryRegion) takeShared(ctx context.Context, first, index, last uint64,
	replaced *replacement) (lo, hi uint64, err error) {
	hi = index + 1
	for hi < last {
		joined, err := r.takeOneShared(ctx, index, hi, replaced)
		if err != nil {
			return 0, 0, err
		}
		if !joined {
			break
		}
		hi++
	}
	lo = index
	for lo > first {
		joined, err := r.takeOneShared(ctx, index, lo-1, replaced)
		if err != nil {
			return 0, 0, err
		}
		if !joined {
			break
		}
		lo--
	}
	return lo, hi, nil
}

// ruleSeam runs as a rule has decided to take one page into a store's run and
// before it does. Production leaves it nil; a test puts another fault or an
// eviction of that page there.
var ruleSeam func(page uint64)

// takeOneShared makes one page private for a rule, from bytes this host holds,
// at the offset of its own, never waiting and never evicting. It reports
// whether the page is one the store's run now covers. index is the store's
// own page.
//
// A rule's page may be in another read-ahead window than the store's, whose
// faults the store's stripe does not keep out: a read-ahead run shorter than
// a range is one. So the page's stripe is taken too, or the run ends there. A
// store of that page between the look below and the copy would make it
// private beside this copy, and one of the two would be lost with its
// reservation.
func (r *MemoryRegion) takeOneShared(ctx context.Context, index, page uint64, replaced *replacement) (bool, error) {
	h := r.host
	waits := sim.Bug(ctx, "pager-rule-takes-a-page-of-another-window")
	if stripe := r.stripe(page); stripe != r.stripe(index) && !waits {
		if !stripe.TryLock() {
			// A fault of that window is under way: the run ends here.
			return false, nil
		}
		defer stripe.Unlock()
	}
	if r.writable(page) {
		return r.joinRun(ctx, page, replaced), nil
	}
	r.bindingsMu.Lock()
	b := r.bindingLocked(page)
	held := b.checkpoint
	r.bindingsMu.Unlock()
	if held != nil {
		// The checkpoint's page: the run ends here.
		return false, nil
	}
	// The page is tried, never waited for: the store holds the region, and
	// whatever holds the page may be waiting to take the region back behind
	// a seal that waits for the store. A page held ends the run.
	var src *zirconvm.VmPage
	if waits {
		// The bug waits for the page, as every rule did.
		var err error
		if src, err = r.host.lockedPage(ctx, b); err != nil {
			return false, err
		}
	} else if p, busy := r.host.tryLockedPage(b); busy {
		return false, nil
	} else {
		src = p
	}
	unlock := func() {
		if src != nil {
			r.host.unlockPage(src)
		}
	}
	if src == nil && !r.zeroMapped(page) {
		// This host does not hold the page's bytes, and a rule reads nothing.
		return false, nil
	}
	if src != nil && frameOf(src).layer != nil {
		// The region's own page with no identity, which is not at its own
		// offset: the run ends here.
		unlock()
		return false, nil
	}
	spill, err := h.tryTakeSpill()
	if err != nil {
		unlock()
		if errors.Is(err, ErrCapacity) {
			return false, nil
		}
		return false, err
	}
	data := make([]byte, h.pageSize)
	_, err = r.readForCopy(ctx, page, src, nil, data)
	unlock()
	if err != nil {
		h.releaseSpill(spill)
		return false, err
	}
	// The page's source may go from here, which the copy no longer needs, and
	// nothing else can make the page private: its stripe is held. place takes
	// the offset under the same hold it looks at it in.
	h.mu.Lock()
	at, _ := h.place(r, page)
	h.mu.Unlock()
	if at.slot < 0 {
		// The offset of this page's own is not to be had, and a rule never
		// evicts for a page the guest did not write.
		h.releaseSpill(spill)
		return false, nil
	}
	if ruleSeam != nil {
		ruleSeam(page)
	}
	// In a controlled run another task may go on here, between the rule's
	// look at the page and its copy: a fault of the page's window, say.
	if err := sim.Admit(ctx, "vmmemory/rule-copy"); err != nil {
		h.releaseSpill(spill)
		return false, h.abandonSlots(ctx, at, 1, err)
	}
	frame, err := r.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		h.releaseSpill(spill)
		return false, err
	}
	frameOf(frame).layer = r
	replaced.keep(frame)
	var origin *zirconvm.VmPage
	if src != nil {
		origin = src
	}
	if err := r.takePrivate(ctx, page, frame, spill, origin, replaced); err != nil {
		r.host.releaseFrame(frame)
		h.releaseSpill(spill)
		return false, err
	}
	// The stats share nothing with the hold that placed the page.
	h.mu.Lock()
	h.stats.RuleCopies++
	h.mu.Unlock()
	return true, nil
}

// tryLockedPage is lockedPage that never waits: the page b names, locked, nil
// where it names none, and busy where something else holds that page's lock.
func (h *Host) tryLockedPage(b *binding) (page *zirconvm.VmPage, busy bool) {
	h.mu.Lock()
	p := b.page
	h.mu.Unlock()
	if p == nil {
		return nil, false
	}
	if !frameOf(p).mu.TryLock() {
		return nil, true
	}
	h.mu.Lock()
	same := b.page == p
	h.mu.Unlock()
	if !same {
		// An eviction or a move took the page from b between the two looks:
		// a rule reads nothing it would have to wait for, so it ends there.
		h.unlockPage(p)
		return nil, true
	}
	return p, false
}

// joinRun takes into a store's run a page the region stores into where it is
// already, and holds it until the store's command lands: an eviction between
// the look and the command would leave the command mapping a slot that went
// back. A page something else holds, an eviction most likely, ends the run.
// One the store holds already, which an earlier rule of the same store took,
// is the run's. It never waits.
func (r *MemoryRegion) joinRun(ctx context.Context, page uint64, replaced *replacement) bool {
	if sim.Bug(ctx, "pager-rule-joins-a-page-it-does-not-hold") {
		// The bug looks at the page and holds nothing.
		joined := r.joinsRun(page)
		if joined && ruleSeam != nil {
			ruleSeam(page)
		}
		return joined
	}
	h := r.host
	b := r.lookupBinding(page)
	h.mu.Lock()
	p := b.page
	h.mu.Unlock()
	if p != nil && slices.Contains(replaced.made, p) {
		return r.joinsRun(page)
	}
	p, busy := h.tryLockedPage(b)
	if busy || p == nil {
		return false
	}
	if !r.joinsRun(page) {
		h.unlockPage(p)
		return false
	}
	if ruleSeam != nil {
		ruleSeam(page)
	}
	replaced.keep(p)
	return true
}

// makeWhole is the mapping budget's backstop, which a client's refusal of a
// store's mapping command reaches: it copies every page of one range into the
// holes of its extent and maps the range with one command, so the mappings the
// range was costing that process go and the store is served again. It reports
// whether it took anything. replaced is the store's, which holds the store's
// own page and whatever its rules took.
//
// The range is one run only around a store's page at its own offset. A store
// that copied away from a sealed page took another offset, because its own
// holds the copy the checkpoint is publishing, and a run from the range's
// first offset would map the guest's page onto that copy. The store holds its
// page, so what the look finds stays true until the command lands.
func (r *MemoryRegion) makeWhole(ctx context.Context, index uint64, replaced *replacement) (bool, error) {
	h := r.host
	span := uint64(h.extentPages)
	placed := r.joinsRun(index)
	if sim.Bug(ctx, "pager-make-whole-around-a-page-elsewhere") {
		// The bug asks only that the range has an extent.
		h.mu.Lock()
		placed = h.extents[extentKey{r, index / span}] != nil
		h.mu.Unlock()
	}
	if !placed {
		return false, nil
	}
	first := index - index%span
	last := min(first+span, uint64(r.pageCount))
	before := r.privatePages(first, last)
	lo, hi, err := r.takeShared(ctx, first, index, last, replaced)
	if err != nil {
		return false, errors.Join(err, replaced.revoke(ctx))
	}
	if r.privatePages(first, last) <= before {
		replaced.done()
		return false, nil
	}
	h.markWhole(r, index)
	// Every page of [lo, hi) is held, so the extent and its offsets are the
	// ones takeShared found.
	h.mu.Lock()
	at := h.placedSlot(r, lo)
	h.stats.MappingMerges++
	h.mu.Unlock()
	count := int(hi - lo)
	r.setMapped(lo, hi, true)
	if err := r.mapPages(ctx, r.runAt(lo, at, count), true); err != nil {
		if revoked := replaced.revoke(ctx); revoked != nil {
			return false, errors.Join(r.fail(err), revoked)
		}
		return false, r.mappingFailed(err, func() { r.setMapped(lo, hi, false) })
	}
	replaced.done()
	return true, nil
}

// privatePages is how many pages of [first, last) the region may store into
// where they are.
func (r *MemoryRegion) privatePages(first, last uint64) int {
	count := 0
	for page := first; page < last; page++ {
		if r.writable(page) {
			count++
		}
	}
	return count
}

// releaseDirty gives back the reservations of every page the region has
// stored into, which a detach does: its writes go with it.
func (r *MemoryRegion) releaseDirty() {
	h := r.host
	var spills []reservation
	r.bindingsMu.Lock()
	r.eachBoundLocked(0, uint64(r.pageCount), func(b *binding) {
		if b.dirty && !b.spill.none() {
			spills = append(spills, b.spill)
		}
		r.uncoldLocked(b)
		b.dirty, b.spill, b.origin, b.ahead, b.checkpoint = false, noReservation, nil, false, nil
		b.protected, b.zeroed = false, false
	})
	r.dirtySet, r.dirtySince = nil, time.Time{}
	r.dirtyRuns = newPageRuns(r.host.pageSize)
	r.journal = regionJournal{unjournaled: newPageRuns(r.host.pageSize)}
	r.bindingsMu.Unlock()
	for _, spill := range spills {
		h.releaseSpill(spill)
	}
}
