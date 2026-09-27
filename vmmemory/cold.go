package vmmemory

import (
	"context"
	"maps"
	"slices"
	"time"
)

// Why a copy is cold before it is dirty.
//
// A write fault on a page the guest does not map is a store trap, and it is not
// always a store. On x86-64 KVM finishes a guest's cold read from a worker,
// async_pf_execute, that asks for the page writable, and the pager cannot tell
// that fault from a store while it waits: the worker does not finish until the
// page is writable, so the pager copies. A guest reading what it inherited
// makes these copies by the thousand. Kept as ordinary dirty pages, they fill
// the arena, are spilled, and are uploaded by the next checkpoint of a disk or
// fetched from the parent by every child of a fork, although the guest never
// changed a byte of them.
//
// So a copy a store trap makes of a published page is cold: private and
// writable, but not yet known to be the guest's state. It becomes an ordinary
// dirty page only when a comparison with the page it was copied from finds the
// guest changed it. Until then:
//
//   - Its origin is pinned. An eviction takes the page a cold copy was made
//     from only when nothing else can go, so there is almost always something
//     to compare it with.
//   - An eviction that picks the copy itself gives it back rather than spill
//     it, once it is coldCopyAge old and where it can take the locks for that
//     without waiting: see giveBackVictim. A copy it could not give back is
//     spilled, and its session and the seal read it back to compare it.
//   - Its session gives it back soon after it is made: see giveBackColdCopies.
//   - A seal compares every cold copy in the set it took and leaves out each one
//     that still holds its origin's bytes: see leaveOutColdCopies. So no
//     checkpoint, whether a capture, a disk's interval checkpoint or a fork
//     point, holds a cold copy the guest did not change, and none is uploaded
//     or fetched.
//
// An origin that goes anyway, because its identity was dropped or a move put
// its bytes elsewhere, ends the copies it pinned being cold: they are ordinary
// dirty pages from then on. A protect trap is a store into a page the guest
// maps, which KVM reports only for a real store, so its copy is never cold.
//
// A cold copy is given back only once it is coldCopyAge old. KVM's worker takes
// the page writable and only then does the vCPU retry its access, so a copy just
// made holds the origin's bytes whether the guest meant to read or to store.
// Compared at once, a store's copy would go back too, and the store would trap
// on the origin and copy again: nothing lost, but every cold store copied
// twice, and a fork's resume is mostly cold stores. By coldCopyAge the vCPU has
// retried, so a store's copy differs and is kept, and a read's is given back. A
// seal compares sooner, because it has to: a store that has not landed by then
// copies again after the checkpoint.

// coldCopyAge is how old a cold copy is before its session gives it back.
var coldCopyAge = 200 * time.Millisecond

// pin keeps origin in the arena while b's cold copy is compared with it. Caller
// holds origin's lock, so no eviction can be taking it at the same moment.
func (h *Host) pin(origin *resident, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	if origin.coldCopies == nil {
		origin.coldCopies = make(map[*binding]struct{})
	}
	origin.coldCopies[b] = struct{}{}
}

// unpin is the reverse of pin.
func (h *Host) unpin(origin *resident, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	delete(origin.coldCopies, b)
}

// pinned reports whether a cold copy is compared with pg, which no eviction may
// therefore take. Caller holds pg's lock.
func (h *Host) pinned(pg *resident) bool {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	return len(pg.coldCopies) > 0
}

// markCold makes b's copy of origin cold, and records it for its session to
// give back. It reports whether it did: a copy that no longer remembers origin
// is not one. pin has kept origin for it. Caller holds the page's window.
func (r *MemoryRegion) markCold(b *binding, origin *resident) bool {
	r.bindingsMu.Lock()
	h := r.host
	h.pinMu.Lock()
	_, pinned := origin.coldCopies[b]
	h.pinMu.Unlock()
	if !pinned || b.origin != origin || !b.dirty || b.checkpoint != nil {
		// An eviction with nothing else to take took origin, or the copy is
		// no longer the one made from it.
		r.bindingsMu.Unlock()
		return false
	}
	b.cold, b.coldAt = true, h.clock.Now()
	if r.coldPages == nil {
		r.coldPages = make(map[uint64]*binding)
	}
	r.coldPages[b.index] = b
	if r.coldCopies == nil {
		r.coldCopies = make(map[uint64]struct{})
	}
	r.coldCopies[b.index] = struct{}{}
	r.bindingsMu.Unlock()
	select {
	case r.coldCopied <- struct{}{}:
	default:
	}
	return true
}

// uncoldLocked ends b's copy being cold, if it is: a comparison found it
// changed, it was given back, or its dirty epoch or its origin ended some other
// way. Every transition that changes a page's origin or ends its dirty epoch
// calls it first. Caller holds bindingsMu.
func (r *MemoryRegion) uncoldLocked(b *binding) {
	if !b.cold {
		return
	}
	b.cold = false
	delete(r.coldPages, b.index)
	r.host.unpin(b.origin, b)
}

// dropCold ends every cold copy compared with pg, which is going although it
// is pinned: its identity was dropped, or a move put its bytes elsewhere. They
// become ordinary dirty pages that remember no origin. Caller holds pg's lock.
func (h *Host) dropCold(pg *resident) {
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(pg.coldCopies))
	pg.coldCopies = nil
	h.pinMu.Unlock()
	for _, b := range copies {
		r := b.memoryRegion
		r.bindingsMu.Lock()
		if b.cold && b.origin == pg {
			b.cold, b.origin = false, nil
			delete(r.coldPages, b.index)
		}
		r.bindingsMu.Unlock()
	}
}

// moveCold makes every cold copy compared with from compared with to instead,
// which holds the same bytes and takes from's place. Caller holds both pages'
// locks.
func (h *Host) moveCold(from, to *resident) {
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(from.coldCopies))
	from.coldCopies = nil
	if len(copies) > 0 && to.coldCopies == nil {
		to.coldCopies = make(map[*binding]struct{})
	}
	for _, b := range copies {
		to.coldCopies[b] = struct{}{}
	}
	h.pinMu.Unlock()
	for _, b := range copies {
		r := b.memoryRegion
		r.bindingsMu.Lock()
		if b.origin == from {
			b.origin = to
		}
		r.bindingsMu.Unlock()
	}
}

// takeColdCopies is the cold copies recorded since it was last called, in page
// order. A session's worker takes them and gives them back coldCopyAge later.
func (r *MemoryRegion) takeColdCopies() []uint64 {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pages := slices.Sorted(maps.Keys(r.coldCopies))
	clear(r.coldCopies)
	return pages
}

// GiveBackColdCopies gives back at once, as GiveBack does, the cold copies
// recorded and not yet taken, and reports how many went back. A session takes
// them itself, coldCopyAge after they are made; this is for a pager with no
// session, and for a test.
func (r *MemoryRegion) GiveBackColdCopies(ctx context.Context) (int, error) {
	return r.givingBack(ctx, r.takeColdCopies)
}

// leaveOutColdCopies compares every cold copy of the set a seal took with its
// origin, and takes out of the set each one whose bytes are still the origin's.
// Those stay the guest's, cold and writable, in the dirty set of the checkpoint
// after this one; the rest are ordinary dirty pages, sealed. It reports how
// many it left out.
//
// The comparison is exact because nothing can change either page while it
// runs. The pause write-protected every mapped page of the set, a spilled copy
// is read back from where the spill put it, and the caller holds the memory
// region exclusively, so no fault of it is served. The origin is pinned, and
// its bytes cannot change while it holds its identity. Caller holds the
// exclusive memory region lock.
func (r *MemoryRegion) leaveOutColdCopies(ctx context.Context, pending map[uint64]*binding) (int, error) {
	h := r.host
	r.bindingsMu.Lock()
	var cold []*binding
	for index, b := range r.coldPages {
		if pending[index] == b {
			cold = append(cold, b)
		}
	}
	r.bindingsMu.Unlock()
	slices.SortFunc(cold, func(a, b *binding) int { return int(a.index) - int(b.index) })
	var buffers settler
	var unchanged []*binding
	for _, b := range cold {
		same, err := r.stillOrigins(ctx, b, &buffers)
		if err != nil {
			return 0, err
		}
		if same {
			delete(pending, b.index)
			unchanged = append(unchanged, b)
			continue
		}
		// The guest changed it, so it is an ordinary dirty page from here. It
		// keeps its origin, which the checkpoint takes with it: that page is
		// released with the copy, as every copy's is.
		r.bindingsMu.Lock()
		r.uncoldLocked(b)
		r.bindingsMu.Unlock()
	}
	if len(unchanged) == 0 {
		return 0, nil
	}
	r.bindingsMu.Lock()
	if r.dirtyBindings == nil {
		r.dirtyBindings = make(map[uint64]*binding)
	}
	for _, b := range unchanged {
		r.dirtyBindings[b.index] = b
		r.noteSealableLocked(b)
	}
	r.bindingsMu.Unlock()
	if err := r.unprotectMapped(ctx, unchanged); err != nil {
		return 0, err
	}
	h.mu.Lock()
	h.stats.UnchangedPages += uint64(len(unchanged))
	h.mu.Unlock()
	return len(unchanged), nil
}

// stillOrigins reports whether a cold copy holds exactly its origin's bytes. An
// origin that is no longer there to compare with, or no longer published, ends
// the copy being cold, and it is reported changed. Caller holds the exclusive
// memory region lock.
func (r *MemoryRegion) stillOrigins(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	origin := r.originOf(b)
	if origin == nil {
		return false, nil
	}
	// Origin first, as every comparison takes them: clean before private.
	if err := origin.mu.Lock(ctx); err != nil {
		return false, err
	}
	defer h.unlock(origin)
	if !origin.published() || origin.slot < 0 {
		return false, nil
	}
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	if pg != nil {
		defer h.unlock(pg)
		return buffers.equal(ctx, h, origin.fileSlot, pg.fileSlot)
	}
	// Spilled: the reservation holds the copy's bytes, checked as a refault
	// checks them.
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if err := origin.file.Read(ctx, origin.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.read(ctx, b, nil, buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}

// unprotectMapped takes the write protection the pause put on the pages of
// bindings off those the guest still maps, so the guest may store into them
// again. It is one command per page, as a give-back's is: consecutive pages the
// guest maps are not one mapping of the client's, and the command that lifts
// the protection covers one. A reclaim revokes its victim's pages under the
// memory region's protection held shared and under that page's lock alone, so
// the protection is held exclusively across reading which pages are mapped and
// lifting it, as the pause holds it: every revocation is then finished, and
// unmapped here, or has not begun. Caller holds the exclusive memory region
// lock.
func (r *MemoryRegion) unprotectMapped(ctx context.Context, bindings []*binding) error {
	if err := r.protectMu.Lock(ctx); err != nil {
		return err
	}
	defer r.protectMu.Unlock()
	for _, b := range bindings {
		if !r.isMapped(b) {
			continue
		}
		if err := r.resolvePages(ctx, b.index, 1, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// giveBackVictim gives an eviction's victim back to its origin instead of
// spilling it, where the victim is a cold copy the guest has not changed: its
// slot is freed, and nothing is written to the spill or kept in a reservation.
// It reports whether it did; a victim it did not give back is spilled as
// usual. A copy it finds changed stops being cold.
//
// A reclaim holds its victim's lock and none of the locks a give-back takes
// before it: the page's window, the memory region, the origin. So it takes
// each of those without waiting, and gives up where one is held: the copy is
// then spilled, and the seal compares it from there. Caller holds pg's lock.
func (h *Host) giveBackVictim(ctx context.Context, pg *resident) (bool, error) {
	h.mu.Lock()
	var b *binding
	if pg.private && pg.aliases.len() == 1 {
		for alias := range pg.aliases.all() {
			b = alias
		}
	}
	h.mu.Unlock()
	if b == nil {
		return false, nil
	}
	r := b.memoryRegion
	// A younger copy may be one the vCPU whose fault made it has not stored
	// into yet: given back, that store would copy again, and in an arena short
	// enough to evict it at once, again and again. It is spilled cold instead,
	// and its session compares it from there.
	if !r.isCold(b) || h.clock.Since(r.coldSince(b)) < coldCopyAge || !r.live.TryRLock() {
		return false, nil
	}
	defer r.live.RUnlock()
	stripe := r.stripe(b.index)
	if !stripe.TryLock() {
		return false, nil
	}
	defer stripe.Unlock()
	if !r.mu.TryRLock() {
		return false, nil
	}
	defer r.mu.RUnlock()
	if r.ready() != nil || !r.isCold(b) {
		return false, nil
	}
	origin := r.originOf(b)
	if !origin.mu.TryLock() {
		return false, nil
	}
	defer h.unlock(origin)
	h.mu.Lock()
	current := b.resident == pg
	h.mu.Unlock()
	if !current || !r.comparable(origin) {
		return false, nil
	}
	var buffers settler
	return r.giveBackCopy(ctx, b, origin, pg, &buffers)
}

// coldSince reports when b's copy became cold.
func (r *MemoryRegion) coldSince(b *binding) time.Time {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.coldAt
}

// isCold reports whether b's copy is cold and still its own dirty state.
func (r *MemoryRegion) isCold(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.cold && b.dirty && b.checkpoint == nil
}

// giveBackSpilled compares a spilled cold copy, read back from the spill, with
// its origin. An unchanged one goes back to the origin, unmapped, and its
// reservation is freed; a changed one stops being cold. The guest maps
// neither, and the page's window keeps its faults out, so the copy cannot
// change while it is compared. Caller holds the page's window, the memory
// region shared and the origin.
func (r *MemoryRegion) giveBackSpilled(ctx context.Context, b *binding, origin *resident, buffers *settler) (bool, error) {
	h := r.host
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if err := origin.file.Read(ctx, origin.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.read(ctx, b, nil, buffers.second); err != nil {
		return false, err
	}
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	if !slices.Equal(buffers.first, buffers.second) {
		r.forgetOrigin(b, origin)
		return false, nil
	}
	note(r, b.index, "give-back-spilled", -1, origin.slot)
	// The pager hands the guest back an older page on purpose here, as a
	// give-back does, so the audit compares the bytes itself. See probe_on.go.
	if found := h.probe.resharedSpilled(ctx, h, b, buffers.second, origin); found != "" {
		panic(found)
	}
	h.bind(b, origin)
	if slot := r.endDirty(b); slot >= 0 {
		h.releaseSpill(slot)
	}
	h.probe.retired(b)
	h.touch(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}
