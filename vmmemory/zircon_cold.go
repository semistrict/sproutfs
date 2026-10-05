package vmmemory

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Cold copies and the give-back over the zircon core, as cold.go and
// giveback.go keep them for the current core. A copy a store trap made of a
// root's page is not yet known to be the guest's state: it is cold, and pins
// the page it was copied from in the zero-fork queue, outside the reclaim
// queues an eviction takes from while anything else can go, until it is
// compared with it. The give-back is Zircon's zero-page scan widened to the
// origin (DedupZeroPage, vm_cow_pages.cc:1363-1437): it checks the copy,
// write-protects it, checks again, and puts the guest back on the origin.

// pin keeps origin in the arena while b's cold copy is compared with it: the
// first pin moves it to the zero-fork queue. Caller holds origin's lock.
func (z *zirconHost) pin(origin *zirconvm.VmPage, b *zbinding) {
	h := z.host
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if f.coldCopies == nil {
		f.coldCopies = make(map[*zbinding]struct{})
	}
	first := len(f.coldCopies) == 0
	f.coldCopies[b] = struct{}{}
	if first {
		z.node.PageQueues().MoveAnonymousToAnonymousZeroFork(origin)
	}
}

// unpin is the reverse of pin: the last unpin moves origin back to the queue
// it belongs in.
func (z *zirconHost) unpin(origin *zirconvm.VmPage, b *zbinding) {
	h := z.host
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if _, pinned := f.coldCopies[b]; !pinned {
		return
	}
	delete(f.coldCopies, b)
	if len(f.coldCopies) == 0 {
		z.unpinnedLocked(origin)
	}
}

// unpinnedLocked moves a page no cold copy pins any more back where it
// belongs: the don't-need queue if it is idle, the newest reclaim queue if
// not. Caller holds h.pinMu.
func (z *zirconHost) unpinnedLocked(origin *zirconvm.VmPage) {
	if _, queued := z.node.PageQueues().Backlink(origin); !queued {
		return
	}
	if frameOf(origin).idle {
		z.node.PageQueues().MoveToReclaimDontNeed(origin)
		return
	}
	z.node.PageQueues().MoveToReclaim(origin)
}

// dropCold lets every cold copy compared with origin go on without it, as
// Host.dropCold does: origin is going although it is pinned. The copies stay
// cold, and are compared with the bytes their volume holds for their page.
func (z *zirconHost) dropCold(origin *zirconvm.VmPage) {
	h := z.host
	f := frameOf(origin)
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(f.coldCopies))
	f.coldCopies = nil
	h.pinMu.Unlock()
	for _, b := range copies {
		q := b.region
		q.mu.Lock()
		if b.cold && b.origin == origin {
			b.origin = nil
		}
		q.mu.Unlock()
	}
}

// moveCold makes every cold copy compared with from compared with to instead,
// which holds the same bytes and takes from's place. Caller holds both pages'
// locks.
func (z *zirconHost) moveCold(from, to *zirconvm.VmPage) {
	h := z.host
	ff, tf := frameOf(from), frameOf(to)
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(ff.coldCopies))
	ff.coldCopies = nil
	if len(copies) > 0 {
		if tf.coldCopies == nil {
			tf.coldCopies = make(map[*zbinding]struct{})
		}
		if len(tf.coldCopies) == 0 {
			z.node.PageQueues().MoveAnonymousToAnonymousZeroFork(to)
		}
	}
	for _, b := range copies {
		tf.coldCopies[b] = struct{}{}
	}
	h.pinMu.Unlock()
	for _, b := range copies {
		q := b.region
		q.mu.Lock()
		if b.origin == from {
			b.origin = to
		}
		q.mu.Unlock()
	}
}

// markCold makes b's copy of origin cold, and records it for its session to
// give back, as MemoryRegion.markCold does. It reports whether it did: a copy
// that no longer remembers origin is not one.
func (z *zirconRegion) markCold(b *zbinding, origin *zirconvm.VmPage) bool {
	r := z.region
	h := r.host
	z.mu.Lock()
	h.pinMu.Lock()
	_, pinned := frameOf(origin).coldCopies[b]
	h.pinMu.Unlock()
	if b.origin != origin || !b.writable() {
		z.mu.Unlock()
		return false
	}
	if !pinned {
		// An eviction with nothing else to take took origin while the copy
		// was being made: the copy is compared with its volume instead.
		b.origin = nil
	}
	b.cold, b.coldAt = true, h.clock.Now().UnixNano()
	if z.coldPages == nil {
		z.coldPages = make(map[uint64]*zbinding)
	}
	z.coldPages[b.index] = b
	if z.coldCopies == nil {
		z.coldCopies = make(map[uint64]struct{})
	}
	z.coldCopies[b.index] = struct{}{}
	z.mu.Unlock()
	select {
	case r.coldCopied <- struct{}{}:
	default:
	}
	return true
}

// requeueCold hands a cold copy back to its session when a give-back could not
// finish it.
func (z *zirconRegion) requeueCold(b *zbinding) {
	r := z.region
	z.mu.Lock()
	queued := b.cold && b.writable()
	if queued {
		if z.coldCopies == nil {
			z.coldCopies = make(map[uint64]struct{})
		}
		z.coldCopies[b.index] = struct{}{}
	}
	z.mu.Unlock()
	if queued {
		select {
		case r.coldCopied <- struct{}{}:
		default:
		}
	}
}

// uncoldLocked ends b's copy being cold, if it is, as
// MemoryRegion.uncoldLocked does. Caller holds z.mu.
func (z *zirconRegion) uncoldLocked(b *zbinding) {
	if !b.cold {
		return
	}
	b.cold = false
	delete(z.coldPages, b.index)
	if b.origin != nil {
		z.host.unpin(b.origin, b)
	}
}

// takeColdCopies is the cold copies recorded since it was last called, in
// page order.
func (z *zirconRegion) takeColdCopies() []uint64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	pages := slices.Sorted(maps.Keys(z.coldCopies))
	clear(z.coldCopies)
	return pages
}

// giveBackColdCopies is MemoryRegion.GiveBackColdCopies over the zircon core.
func (z *zirconRegion) giveBackColdCopies(ctx context.Context) (int, error) {
	return z.givingBack(ctx, z.takeColdCopies)
}

// isCold reports whether b's copy is cold and still its own dirty state.
func (z *zirconRegion) isCold(b *zbinding) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	return b.cold && b.writable()
}

// coldSince reports when b's copy became cold.
func (z *zirconRegion) coldSince(b *zbinding) time.Time {
	z.mu.Lock()
	defer z.mu.Unlock()
	return time.Unix(0, b.coldAt)
}

// originOf reports the page b was copied from, nil where it was copied from
// nothing a comparison may use.
func (z *zirconRegion) originOf(b *zbinding) *zirconvm.VmPage {
	z.mu.Lock()
	defer z.mu.Unlock()
	return b.origin
}

// forgetOrigin stops comparing b with origin: the guest changed it, or origin
// has gone. It is left alone where b has been copied again since.
func (z *zirconRegion) forgetOrigin(b *zbinding, origin *zirconvm.VmPage) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if b.origin == origin {
		z.uncoldLocked(b)
		b.origin = nil
	}
}

// endDirty ends b's dirty epoch with no checkpoint, because its bytes are the
// ones the page it shares again holds, and reports the reservation it was
// admitted under, for the caller to give back. A region left with no dirty
// page holds no unpublished write, so its loss window ends too.
func (z *zirconRegion) endDirty(b *zbinding) reservation {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.uncoldLocked(b)
	spill := b.spill
	b.spill, b.dirty, b.ahead, b.origin = noReservation, false, false, nil
	delete(z.dirtySet, b.index)
	z.noteSealableLocked(b)
	if len(z.dirtySet) == 0 {
		z.dirtySince = time.Time{}
	}
	return spill
}

// leaveOutColdCopies is MemoryRegion.leaveOutColdCopies over the zircon core:
// every cold copy of the set a seal took that still holds its origin's bytes
// is left out of it, and stays the guest's, cold and writable. Caller holds
// the region exclusively.
func (z *zirconRegion) leaveOutColdCopies(ctx context.Context, pending map[uint64]*zbinding) (int, error) {
	r := z.region
	h := r.host
	z.mu.Lock()
	var cold []*zbinding
	for index, b := range z.coldPages {
		if pending[index] == b {
			cold = append(cold, b)
		}
	}
	z.mu.Unlock()
	slices.SortFunc(cold, func(a, b *zbinding) int { return int(a.index) - int(b.index) })
	var buffers settler
	var unchanged []*zbinding
	for _, b := range cold {
		same, err := z.stillOrigins(ctx, b, &buffers)
		if err != nil {
			return 0, err
		}
		if same {
			delete(pending, b.index)
			unchanged = append(unchanged, b)
			continue
		}
		z.mu.Lock()
		z.uncoldLocked(b)
		z.mu.Unlock()
	}
	if len(unchanged) == 0 {
		return 0, nil
	}
	z.mu.Lock()
	if z.dirtySet == nil {
		z.dirtySet = make(map[uint64]*zbinding)
	}
	for _, b := range unchanged {
		z.dirtySet[b.index] = b
		z.noteSealableLocked(b)
	}
	z.mu.Unlock()
	if err := z.unprotectMapped(ctx, unchanged); err != nil {
		return 0, err
	}
	h.mu.Lock()
	h.stats.UnchangedPages += uint64(len(unchanged))
	h.mu.Unlock()
	return len(unchanged), nil
}

// stillOrigins reports whether a cold copy holds exactly its origin's bytes.
// An origin no longer there to compare with ends the copy being cold, and it
// is reported changed. Caller holds the region exclusively.
func (z *zirconRegion) stillOrigins(ctx context.Context, b *zbinding, buffers *settler) (bool, error) {
	h := z.region.host
	origin := z.originOf(b)
	if origin == nil {
		return z.volumeHolds(ctx, b, buffers)
	}
	if err := z.host.lockPage(ctx, origin); err != nil {
		return false, err
	}
	defer z.host.unlockPage(origin)
	if !z.host.published(origin) {
		return false, nil
	}
	page, err := z.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		defer z.host.unlockPage(page)
		return buffers.equal(ctx, h, frameOf(origin).fileSlot, frameOf(page).fileSlot)
	}
	// Spilled: the reservation holds the copy's bytes.
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	f := frameOf(origin)
	if err := f.file.Read(ctx, f.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.readSpill(ctx, z.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}

// unprotectMapped takes the write protection the pause put on the pages of
// bindings off those the guest still maps, one command per page, with the
// region's protection held exclusively, as MemoryRegion.unprotectMapped does.
// Caller holds the region exclusively.
func (z *zirconRegion) unprotectMapped(ctx context.Context, bindings []*zbinding) error {
	r := z.region
	if err := r.protectMu.Lock(ctx); err != nil {
		return err
	}
	defer r.protectMu.Unlock()
	for _, b := range bindings {
		if !z.isMapped(b) {
			continue
		}
		if err := r.resolvePages(ctx, b.index, 1, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// volumeHolds is MemoryRegion.volumeHolds over the zircon core: whether a
// cold copy whose origin has gone holds exactly the bytes its volume holds
// for its page. A backing that may answer with another host's bytes has no
// such guarantee, and the copy is reported changed.
func (z *zirconRegion) volumeHolds(ctx context.Context, b *zbinding, buffers *settler) (bool, error) {
	r := z.region
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
	page, err := z.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		defer z.host.unlockPage(page)
		f := frameOf(page)
		if err := f.file.Read(ctx, f.slot, buffers.second); err != nil {
			return false, err
		}
	} else if err := h.readSpill(ctx, z.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}

// givingBack is MemoryRegion.givingBack over the zircon core: one give-back
// pass over the pages that pages lists.
func (z *zirconRegion) givingBack(ctx context.Context, pages func() []uint64) (int, error) {
	r := z.region
	if err := r.live.RLock(ctx); err != nil {
		return 0, err
	}
	defer r.live.RUnlock()
	if err := r.serving(); err != nil {
		return 0, err
	}
	var buffers settler
	given := 0
	for _, index := range pages() {
		back, err := z.giveBack(ctx, index, &buffers)
		if back {
			given++
		}
		if err != nil {
			return given, err
		}
	}
	return given, nil
}

// giveBack is one page of a pass, holding it as a store fault holds it: the
// page's window, the region shared, the origin's lock and the copy's. It
// reports whether the copy went back.
func (z *zirconRegion) giveBack(ctx context.Context, index uint64, buffers *settler) (bool, error) {
	r := z.region
	h := r.host
	if err := r.stripe(index).Lock(ctx); err != nil {
		return false, err
	}
	defer r.stripe(index).Unlock()
	if err := r.lockPageAccess(ctx, index, true); err != nil {
		return false, err
	}
	defer r.mu.RUnlock()
	b := z.lookupBinding(index)
	if b == nil {
		return false, nil
	}
	origin := z.originOf(b)
	if origin == nil && z.isCold(b) && !h.wholeRange(r, index) {
		return z.giveBackToVolume(ctx, b, buffers)
	}
	if origin == nil || !z.ownDirty(b) || h.wholeRange(r, index) {
		// Stored into and compared since the list was made, or a range the
		// rules made one mapping, which a page given back would break in
		// three.
		return false, nil
	}
	// Origin first, as the settle takes them: clean before private.
	if err := z.host.lockPage(ctx, origin); err != nil {
		return false, err
	}
	defer z.host.unlockPage(origin)
	if !z.comparable(origin) {
		z.forgetOrigin(b, origin)
		return false, nil
	}
	page, err := z.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page == nil {
		if z.isCold(b) {
			// A cold copy the pager spilled is read back: it pins its origin
			// until it is compared.
			return z.giveBackSpilled(ctx, b, origin, buffers)
		}
		return false, nil
	}
	defer z.host.unlockPage(page)
	return z.giveBackCopy(ctx, b, origin, page, buffers)
}

// ownDirty reports whether b is the region's own dirty state no checkpoint
// holds.
func (z *zirconRegion) ownDirty(b *zbinding) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	return b.writable()
}

// comparable reports whether a copy can still be compared with origin: it is
// resident, still its identity's page, and in a file this region's session
// was given. Caller holds origin's lock.
func (z *zirconRegion) comparable(origin *zirconvm.VmPage) bool {
	r := z.region
	if !z.host.published(origin) {
		return false
	}
	return r.fileNumber(frameOf(origin).file) >= 0
}

// giveBackCopy is one page of a give-back once its locks are held: the
// page's window, the region shared, the origin and then the copy page. It
// reports whether the copy went back.
func (z *zirconRegion) giveBackCopy(ctx context.Context, b *zbinding, origin, page *zirconvm.VmPage, buffers *settler) (bool, error) {
	r := z.region
	h := r.host
	index := b.index
	if !z.isMapped(b) {
		return false, nil
	}
	if err := r.protectPages(ctx, index, 1); err != nil {
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
			return false, err
		}
		return false, r.fail(err)
	}
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	same, err := buffers.equal(ctx, h, frameOf(origin).fileSlot, frameOf(page).fileSlot)
	if err == nil && !same && sim.Bug(ctx, "pager-give-back-changed-copy") {
		// The copy goes back although the guest stored into it, which loses
		// what it stored.
		same = true
	}
	if err != nil || !same {
		if !same && err == nil {
			z.forgetOrigin(b, origin)
		}
		return false, errors.Join(err, r.liftProtection(ctx, index))
	}
	// Mapped and installed read-only, so the guest's next read maps it
	// without a fault, which is what would otherwise come back as a write.
	if err := z.mapInPlace(ctx, b, origin); err != nil {
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		z.requeueCold(b)
		return false, r.liftProtection(ctx, index)
	}
	return true, z.shareOrigin(b, page, origin)
}

// shareOrigin makes the origin b's page again, now that the guest maps it:
// b is clean, and the copy and the reservation it was admitted under go back.
// Caller holds the page's window, the region shared and both pages.
func (z *zirconRegion) shareOrigin(b *zbinding, page, origin *zirconvm.VmPage) error {
	h := z.region.host
	ps := h.pageSize
	h.mu.Lock()
	z.host.unaliasLocked(b)
	z.host.aliasLocked(b, origin)
	h.mu.Unlock()
	if spill := z.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	z.layer.RemovePage(b.index*ps, page)
	z.host.releaseFrame(page)
	z.host.node.PageQueues().MarkAccessed(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return nil
}

// giveBackSpilled is MemoryRegion.giveBackSpilled over the zircon core: a
// spilled cold copy, read back from the spill, compared with its origin. An
// unchanged one goes back to the origin, unmapped, and its reservation is
// freed; a changed one stops being cold. Caller holds the page's window, the
// region shared and the origin.
func (z *zirconRegion) giveBackSpilled(ctx context.Context, b *zbinding, origin *zirconvm.VmPage, buffers *settler) (bool, error) {
	h := z.region.host
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	f := frameOf(origin)
	if err := f.file.Read(ctx, f.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.readSpill(ctx, z.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	if !slices.Equal(buffers.first, buffers.second) {
		z.forgetOrigin(b, origin)
		return false, nil
	}
	h.mu.Lock()
	z.host.aliasLocked(b, origin)
	h.mu.Unlock()
	if spill := z.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	z.host.node.PageQueues().MarkAccessed(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}

// giveBackToVolume is MemoryRegion.giveBackToVolume over the zircon core: a
// cold copy whose origin has gone is compared with its volume's bytes, and an
// unchanged one is dropped, so the guest's next access reads the page again
// as any first access does. Caller holds the page's window and the region
// shared.
func (z *zirconRegion) giveBackToVolume(ctx context.Context, b *zbinding, buffers *settler) (bool, error) {
	h := z.region.host
	ps := h.pageSize
	// The guest's mapping goes first, so nothing it stores can land in the
	// copy while it is compared.
	if err := z.revokeBindings(ctx, []*zbinding{b}); err != nil {
		return false, err
	}
	same, err := z.volumeHolds(ctx, b, buffers)
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	if err != nil || !same {
		if err == nil {
			z.mu.Lock()
			z.uncoldLocked(b)
			z.mu.Unlock()
		}
		return false, err
	}
	page, err := z.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		h.mu.Lock()
		z.host.unaliasLocked(b)
		h.mu.Unlock()
		z.layer.RemovePage(b.index*ps, page)
		z.host.releaseFrame(page)
		z.host.unlockPage(page)
	}
	if spill := z.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}

// giveBackVictim is Host.giveBackVictim over the zircon core: an eviction's
// victim that is a cold copy the guest has not changed goes back to its
// origin instead of the spill. A reclaim holds none of the locks a give-back
// takes before the victim's, so it takes each without waiting, and gives up
// where one is held: the copy is then spilled, and compared from there.
// Caller holds page's lock.
func (z *zirconHost) giveBackVictim(ctx context.Context, page *zirconvm.VmPage) (bool, error) {
	h := z.host
	f := frameOf(page)
	h.mu.Lock()
	var b *zbinding
	if f.layer != nil && f.aliases.len() == 1 {
		for alias := range f.aliases.all() {
			b = alias
		}
	}
	h.mu.Unlock()
	if b == nil {
		return false, nil
	}
	q := b.region
	r := q.region
	if !q.isCold(b) || h.clock.Since(q.coldSince(b)) < coldCopyAge || !r.live.TryRLock() {
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
	if r.ready() != nil || !q.isCold(b) {
		return false, nil
	}
	origin := q.originOf(b)
	if origin == nil || !frameOf(origin).mu.TryLock() {
		return false, nil
	}
	defer z.unlockPage(origin)
	h.mu.Lock()
	current := b.page == page
	h.mu.Unlock()
	if !current || !q.comparable(origin) {
		return false, nil
	}
	var buffers settler
	return q.giveBackCopy(ctx, b, origin, page, &buffers)
}

// mapInPlace is MemoryRegion.mapInPlace over the zircon core: to, read-only,
// where b is mapped, in one MAP, installed in the region's page tables, so
// the guest reads on without a fault. It reports ErrMappingRefused where the
// client refused the MAP, having changed nothing. Caller holds b's page and
// to.
func (z *zirconRegion) mapInPlace(ctx context.Context, b *zbinding, to *zirconvm.VmPage) error {
	r := z.region
	h := r.host
	return r.underProtection(ctx, func() error {
		if !z.isMapped(b) {
			return nil
		}
		if err := r.mapPages(ctx, r.runAt(b.index, frameOf(to).fileSlot, 1), false); err != nil {
			return r.mappingFailed(err, func() {})
		}
		h.mu.Lock()
		h.stats.Mappings++
		h.stats.MappingRuns++
		h.stats.MappedPages++
		h.mu.Unlock()
		if err := r.resolvePages(ctx, b.index, 1, false); err != nil {
			return r.fail(err)
		}
		return nil
	})
}
