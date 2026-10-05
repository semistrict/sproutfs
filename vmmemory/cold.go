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
//     from only when nothing else can go. Without it, the copy is compared
//     with the bytes its volume holds for the page, which is a backing read:
//     see volumeHolds.
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
// A move of the origin moves its pins with it. An origin that goes anyway
// leaves its copies cold, compared with their volume from then on. A protect
// trap is a store into a page the guest
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

// pin keeps origin in the arena while b's cold copy is compared with it: the
// first pin moves it to the zero-fork queue, outside the queues an eviction
// takes from while anything else can go. Caller holds origin's lock, so no
// eviction can be taking it at the same moment.
func (h *Host) pin(origin *resident, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	if origin.coldCopies == nil {
		origin.coldCopies = make(map[*binding]struct{})
	}
	first := len(origin.coldCopies) == 0
	origin.coldCopies[b] = struct{}{}
	if first {
		h.pinnedLocked(origin)
	}
}

// unpin is the reverse of pin: the last unpin moves origin back to the queue
// it belongs in.
func (h *Host) unpin(origin *resident, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	if _, pinned := origin.coldCopies[b]; !pinned {
		return
	}
	delete(origin.coldCopies, b)
	if len(origin.coldCopies) == 0 {
		h.unpinnedLocked(origin)
	}
}

// markCold makes b's copy of origin cold, and records it for its session to
// give back. It reports whether it did: a copy that no longer remembers origin
// is not one. pin kept origin for it, unless an eviction took it anyway.
// Caller holds the page's window.
func (r *MemoryRegion) markCold(b *binding, origin *resident) bool {
	r.bindingsMu.Lock()
	h := r.host
	h.pinMu.Lock()
	_, pinned := origin.coldCopies[b]
	h.pinMu.Unlock()
	if b.origin != origin || !b.dirty || b.checkpoint != nil {
		// The copy is no longer the one made from origin.
		r.bindingsMu.Unlock()
		return false
	}
	if !pinned {
		// An eviction with nothing else to take took origin while the copy
		// was being made: the copy is compared with its volume instead.
		b.origin = nil
	}
	b.cold, b.coldAt = true, h.clock.Now().UnixNano()
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

// requeueCold hands a cold copy back to its session when a give-back could not
// finish it, so the session tries it again after coldCopyAge. A copy that is
// no longer cold is not handed back.
func (r *MemoryRegion) requeueCold(b *binding) {
	r.bindingsMu.Lock()
	queued := b.cold && b.dirty && b.checkpoint == nil
	if queued {
		if r.coldCopies == nil {
			r.coldCopies = make(map[uint64]struct{})
		}
		r.coldCopies[b.index] = struct{}{}
	}
	r.bindingsMu.Unlock()
	if queued {
		select {
		case r.coldCopied <- struct{}{}:
		default:
		}
	}
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
	if b.origin != nil {
		r.host.unpin(b.origin, b)
	}
}

// dropCold lets every cold copy compared with pg go on without it: pg is
// going although it is pinned, because an eviction had nothing else to take,
// its identity was dropped, or a move put its bytes elsewhere. The copies stay
// cold, and are compared with the bytes their volume holds for their page
// instead, which are what pg held: see volumeHolds. Caller holds pg's lock.
func (h *Host) dropCold(pg *resident) {
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(pg.coldCopies))
	pg.coldCopies = nil
	if len(copies) > 0 {
		h.unpinnedLocked(pg)
	}
	h.pinMu.Unlock()
	for _, b := range copies {
		r := b.memoryRegion
		r.bindingsMu.Lock()
		if b.cold && b.origin == pg {
			b.origin = nil
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
	if len(copies) > 0 {
		h.unpinnedLocked(from)
		if to.coldCopies == nil {
			to.coldCopies = make(map[*binding]struct{})
		}
		if len(to.coldCopies) == 0 {
			h.pinnedLocked(to)
		}
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

// GiveBackColdCopies gives back at once the cold copies recorded and not yet
// taken, and reports how many went back: see giveback.go. A session takes
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
		return r.volumeHolds(ctx, b, buffers)
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
	if origin == nil || !origin.mu.TryLock() {
		// A copy whose origin has gone is compared with its volume's bytes,
		// which is I/O a reclaim does not wait for: it is spilled cold.
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
	return time.Unix(0, b.coldAt)
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

// volumeHolds reports whether a cold copy whose origin has gone holds exactly
// the bytes its volume holds for its page. A copy is cold only while no
// checkpoint has taken it, so those are the bytes of the page it was copied
// from. Reading them is a backing read: the page cache's, or the object
// store's, which is why a pinned origin goes only when nothing else can. A
// backing that may answer with another host's bytes has no such guarantee, and
// the copy is reported changed. Caller holds the page's window or the
// exclusive memory region lock, so the copy cannot change while it is read.
func (r *MemoryRegion) volumeHolds(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	if r.peer {
		return false, nil
	}
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if _, err := r.loadBacking(ctx, b.index*h.pageSize, buffers.first); err != nil {
		return false, err
	}
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	if pg != nil {
		defer h.unlock(pg)
	}
	if err := h.read(ctx, b, pg, buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}

// giveBackToVolume is a give-back of a cold copy whose origin has gone: it is
// compared with its volume's bytes, and an unchanged one is dropped rather
// than pointed at a page, so the guest's next access reads the page again as
// any first access does. A changed one stops being cold. Caller holds the
// page's window and the memory region shared.
func (r *MemoryRegion) giveBackToVolume(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	// The guest's mapping goes first, so nothing it stores can land in the copy
	// while it is compared: a store traps and waits for the page's window.
	if err := r.revokeBindings(ctx, []*binding{b}); err != nil {
		return false, err
	}
	same, err := r.volumeHolds(ctx, b, buffers)
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	if err != nil || !same {
		if err == nil {
			r.bindingsMu.Lock()
			r.uncoldLocked(b)
			r.bindingsMu.Unlock()
		}
		return false, err
	}
	pg, err := h.current(ctx, b)
	if err != nil {
		return false, err
	}
	note(r, b.index, "give-back-to-volume", -1, -1)
	if pg != nil {
		err = h.unlink(ctx, b, pg)
		h.unlock(pg)
		if err != nil {
			return false, err
		}
	}
	if slot := r.endDirty(b); slot >= 0 {
		h.releaseSpill(slot)
	}
	h.probe.retired(b)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}
