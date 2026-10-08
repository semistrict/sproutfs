package vmmemory

import (
	"context"
	"errors"
	"slices"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Why a copy is given back without a checkpoint.
//
// A write fault is not always a store. On x86-64 KVM finishes a cold read from
// a worker thread, async_pf_execute, that always asks for the page writable. On
// aarch64 a guest's first execution of a page reaches the pager as a write too.
// Either way the pager makes a private copy. The settle notices a copy that
// never changed, but only behind a checkpoint, and RAM is never checkpointed on
// the interval: only captures, forks and migrations seal it. So without this, a
// RAM page shared between VMs becomes a private copy for as long as the VM
// lives, which at a 2 MiB page is 2 MiB the host holds twice.
//
// What is given back is a cold copy: one a write fault made of a page the
// guest did not map, which is what such a read looks like. Its session gives it
// back once it is coldCopyAge old, and a seal or an eviction sooner: see
// cold.go. A copy of a page the guest had mapped is a store the guest really
// made, and is left to the settle behind its next checkpoint.
//
// The give-back needs no pause. It works one page at a time, and it holds that
// page the way a store fault holds it: the page's window, the memory region
// shared, the origin's lock and the copy's. Holding the window keeps every
// fault of the page out, and holding the memory region keeps a seal out.
// Write-protecting the copy stops the guest's stores, because every store then
// traps and waits for the window. From then on the copy's bytes cannot change,
// so comparing them with the origin's is exact. A store that trapped in the
// meantime is served once the window is free. Against the origin it copies
// again, and against a copy that was kept it lands. It loses nothing.
//
// The guest is pointed at the origin in place, with the mapping command a
// store uses to replace the page it copied from, and the page is installed
// read-only at once: mapInPlace, which a move uses for the same reason. It is not revoked. A revoked page is missing, so on x86-64
// the next cold read would arrive as a write again and copy again, and the
// give-back would undo its own work at every pass. An installed page is present,
// so KVM maps it for a read without asking the pager.
//
// It relies on every writer of guest RAM going through the VMM's page tables,
// where the write-protection stops it: the VMM's own threads, the kernel's
// copy_to_user for the sync block engine and the tap device, and KVM's own
// writes, which use the userspace address or a pfn cache that the MMU notifier
// invalidates. A writer that pinned the copy before the write-protection would
// write into a page this frees. Managed RAM refuses the io_uring block engine
// and vhost-user, and Firecracker has no vhost-net or vhost-vsock. KVM's maps
// for a nested guest are the one exception, and every seal and copy-on-write
// shares it: see "Writers that bypass the page tables" in docs/vm-memory.md.

// liftProtection takes off the write-protection a give-back put on a copy it keeps,
// which also wakes a store that trapped on it.
func (r *MemoryRegion) liftProtection(ctx context.Context, index uint64) error {
	if err := r.resolvePages(ctx, index, 1, true); err != nil {
		return r.fail(err)
	}
	return nil
}

// givingBack is one give-back pass over the pages that pages lists, which it
// calls once the memory region is known to be live.
func (r *MemoryRegion) givingBack(ctx context.Context, pages func() []uint64) (int, error) {
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
		back, err := r.giveBack(ctx, index, &buffers)
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
func (r *MemoryRegion) giveBack(ctx context.Context, index uint64, buffers *settler) (bool, error) {
	h := r.host
	if err := r.stripe(index).Lock(ctx); err != nil {
		return false, err
	}
	defer r.stripe(index).Unlock()
	if err := r.lockPageAccess(ctx, index, true); err != nil {
		return false, err
	}
	defer r.mu.RUnlock()
	b := r.lookupBinding(index)
	if b == nil {
		return false, nil
	}
	origin := r.originOf(b)
	if origin == nil && r.isCold(b) && !h.wholeRange(r, index) {
		return r.giveBackToVolume(ctx, b, buffers)
	}
	if origin == nil || !r.ownDirty(b) || h.wholeRange(r, index) {
		// Stored into and compared since the list was made, or a range the
		// rules made one mapping, which a page given back would break in
		// three.
		return false, nil
	}
	// Origin first, as the settle takes them: clean before private.
	if err := r.host.lockPage(ctx, origin); err != nil {
		return false, err
	}
	defer r.host.unlockPage(origin)
	if !r.comparable(origin) {
		r.forgetOrigin(b, origin)
		return false, nil
	}
	page, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page == nil {
		if r.isCold(b) {
			// A cold copy the pager spilled is read back: it pins its origin
			// until it is compared.
			return r.giveBackSpilled(ctx, b, origin, buffers)
		}
		return false, nil
	}
	defer r.host.unlockPage(page)
	return r.giveBackCopy(ctx, b, origin, page, buffers)
}

// ownDirty reports whether b is the region's own dirty state no checkpoint
// holds.
func (r *MemoryRegion) ownDirty(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.writable()
}

// comparable reports whether a copy can still be compared with origin: it is
// resident, still its identity's page, and in a file this region's session
// was given. Caller holds origin's lock.
func (r *MemoryRegion) comparable(origin *zirconvm.VmPage) bool {
	if !r.host.published(origin) {
		return false
	}
	return r.fileNumber(frameOf(origin).file) >= 0
}

// giveBackCopy is one page of a give-back once its locks are held: the
// page's window, the region shared, the origin and then the copy page. It
// reports whether the copy went back.
func (r *MemoryRegion) giveBackCopy(ctx context.Context, b *binding, origin, page *zirconvm.VmPage, buffers *settler) (bool, error) {
	h := r.host
	index := b.index
	if !r.isMapped(b) {
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
			r.forgetOrigin(b, origin)
		}
		return false, errors.Join(err, r.liftProtection(ctx, index))
	}
	// Mapped and installed read-only, so the guest's next read maps it
	// without a fault, which is what would otherwise come back as a write.
	if err := r.mapInPlace(ctx, b, origin); err != nil {
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		r.requeueCold(b)
		return false, r.liftProtection(ctx, index)
	}
	return true, r.shareOrigin(ctx, b, page, origin)
}

// shareOrigin makes the origin b's page again, now that the guest maps it:
// b is clean, and the copy and the reservation it was admitted under go back.
// Caller holds the page's window, the region shared and both pages.
func (r *MemoryRegion) shareOrigin(ctx context.Context, b *binding, page, origin *zirconvm.VmPage) error {
	h := r.host
	ps := h.pageSize
	// The pager hands a guest back an older page on purpose here, as the
	// settle does, so the audit compares the bytes itself.
	if found := h.probe.reshared(context.Background(), h, frameOf(page), frameOf(origin)); found != "" {
		panic(found)
	}
	h.mu.Lock()
	r.host.unaliasLocked(b)
	r.host.aliasLocked(b, origin)
	h.mu.Unlock()
	// What is held keeps out all that could change b or the copy before the
	// copy goes back: a fault or a prefetch of the page needs the window, a
	// seal or a detach the region, and an eviction, a move or another store
	// either page. The copy is the region's own, which nothing else maps, so
	// nothing can alias it again before releaseFrame looks.
	admitGoingOn(ctx, "vmmemory/give-back-share")
	if spill := r.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	r.layer.RemovePage(b.index*ps, page)
	r.host.releaseFrame(page)
	r.host.node.PageQueues().MarkAccessed(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return nil
}

// giveBackSpilled compares a spilled cold copy, read back from the spill, with
// its origin. An unchanged one goes back to the origin, unmapped, and its
// reservation is freed; a changed one stops being cold. Caller holds the
// page's window, the region shared and the origin.
func (r *MemoryRegion) giveBackSpilled(ctx context.Context, b *binding, origin *zirconvm.VmPage, buffers *settler) (bool, error) {
	h := r.host
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	f := frameOf(origin)
	if err := f.file.Read(ctx, f.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.readSpill(ctx, r.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	h.mu.Lock()
	h.stats.GiveBackCompares++
	h.mu.Unlock()
	if !slices.Equal(buffers.first, buffers.second) {
		r.forgetOrigin(b, origin)
		return false, nil
	}
	if found := h.probe.resharedSpilled(ctx, h, b, buffers.second, frameOf(origin)); found != "" {
		panic(found)
	}
	// What was compared stands until b is the origin's: b names no page
	// (giveBack looked under the window), and only a fault of the page binds
	// one, which the window keeps out; its spill goes only with a seal, a
	// retire or a detach, which the region keeps out; and the origin's bytes
	// and slot only with an eviction or a move of it, which its lock keeps
	// out.
	if err := sim.Admit(ctx, "vmmemory/give-back-spilled"); err != nil {
		return false, err
	}
	h.mu.Lock()
	r.host.aliasLocked(b, origin)
	h.mu.Unlock()
	// The same locks keep b's dirty state as it was until endDirty ends it.
	if spill := r.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	r.host.node.PageQueues().MarkAccessed(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}

// giveBackToVolume compares a cold copy whose origin has gone with its
// volume's bytes, and an unchanged one is dropped, so the guest's next access
// reads the page again as any first access does. Caller holds the page's
// window and the region shared.
func (r *MemoryRegion) giveBackToVolume(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	ps := h.pageSize
	// The guest's mapping goes first, so nothing it stores can land in the
	// copy while it is compared.
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
	// The copy's bytes cannot change from the comparison to its drop: the
	// guest maps it no more, and a fault that would map it again waits for
	// the window. An eviction may spill it meanwhile, which moves the same
	// bytes, and lockedPage and endDirty below find whichever holds them.
	if err := sim.Admit(ctx, "vmmemory/give-back-volume"); err != nil {
		return false, err
	}
	page, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		h.mu.Lock()
		r.host.unaliasLocked(b)
		h.mu.Unlock()
		// The copy's lock is held until it has gone back, and it is the
		// region's own, which nothing else maps.
		r.layer.RemovePage(b.index*ps, page)
		r.host.releaseFrame(page)
		r.host.unlockPage(page)
	}
	if spill := r.endDirty(b); !spill.none() {
		h.releaseSpill(spill)
	}
	h.mu.Lock()
	h.stats.GivenBackPages++
	h.signal()
	h.mu.Unlock()
	return true, nil
}

// giveBackVictim gives an eviction's victim that is a cold copy the guest has
// not changed back to its origin instead of the spill. A reclaim holds none of
// the locks a give-back takes before the victim's, so it takes each without
// waiting, and gives up where one is held: the copy is then spilled, and
// compared from there. Caller holds page's lock.
func (h *Host) giveBackVictim(ctx context.Context, page *zirconvm.VmPage) (bool, error) {
	f := frameOf(page)
	h.mu.Lock()
	var b *binding
	if f.layer != nil && f.aliases.len() == 1 {
		for alias := range f.aliases.all() {
			b = alias
		}
	}
	h.mu.Unlock()
	if b == nil {
		return false, nil
	}
	r := b.region
	if !r.isCold(b) || h.clock.Since(r.coldSince(b)) < coldCopyAge {
		return false, nil
	}
	// Until the region is held, a seal may join a checkpoint's copy to the
	// page and take b's dirty state, or a fault may bind b another page, so
	// what this looked at is looked at again once each lock is taken: b being
	// cold under the region, b's page under h.mu, and the origin under its
	// lock (comparable). The caller holds the page's lock, so nothing but a
	// seal adds an alias to it, and a seal leaves b not cold.
	if err := sim.Admit(ctx, "vmmemory/give-back-victim"); err != nil {
		return false, err
	}
	if !r.live.TryRLock() {
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
	if origin == nil || !frameOf(origin).mu.TryLock() {
		return false, nil
	}
	defer h.unlockPage(origin)
	h.mu.Lock()
	current := b.page == page
	h.mu.Unlock()
	if !current || !r.comparable(origin) {
		return false, nil
	}
	var buffers settler
	return r.giveBackCopy(ctx, b, origin, page, &buffers)
}

// mapInPlace maps to, read-only, where b is mapped, in one MAP, installed in
// the region's page tables, so the guest reads on without a fault. It reports
// ErrMappingRefused where the client refused the MAP, having changed nothing.
// Caller holds b's page and to.
func (r *MemoryRegion) mapInPlace(ctx context.Context, b *binding, to *zirconvm.VmPage) error {
	h := r.host
	return r.underProtection(ctx, func() error {
		if !r.isMapped(b) {
			return nil
		}
		if err := r.mapRun(ctx, r.runAt(b.index, frameOf(to).fileSlot, 1), false); err != nil {
			return err
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
