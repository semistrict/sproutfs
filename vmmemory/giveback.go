package vmmemory

import (
	"context"
	"errors"
	"slices"

	"github.com/semistrict/sproutfs/platform/sim"
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

// GiveBack compares up to limit of this memory region's private copies with the
// pages they were copied from, and gives back each one whose bytes are still
// its origin's: the guest maps the origin again, and the copy and its dirty
// reservation are freed. It reports how many it gave back.
//
// A copy found changed forgets its origin, so it is compared once and never
// again. A copy that could not be compared yet keeps it: one the pager has
// spilled, one whose range is a single whole mapping, one the guest does not map.
// Successive calls start where the last one stopped, so a copy left behind is
// not the only one ever looked at. A sealed page is not a candidate: the settle
// behind its checkpoint compares it.
func (r *MemoryRegion) GiveBack(ctx context.Context, limit int) (int, error) {
	return r.givingBack(ctx, func() []uint64 { return r.copies(limit) })
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
	if r.fixed {
		// A fixed region's page may be one KVM writes behind the page tables,
		// which the write-protect this compares under would not stop. See
		// fixed.go.
		return 0, nil
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

// copies is up to limit pages of this memory region that hold a private copy
// with an origin, in page order from where the last pass stopped.
func (r *MemoryRegion) copies(limit int) []uint64 {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	var pages []uint64
	for index, b := range r.dirtyBindings {
		if b.origin != nil {
			pages = append(pages, index)
		}
	}
	slices.Sort(pages)
	from, _ := slices.BinarySearch(pages, r.givenBackTo)
	pages = append(pages[from:], pages[:from]...)
	pages = pages[:min(len(pages), max(limit, 0))]
	if len(pages) > 0 {
		r.givenBackTo = pages[len(pages)-1] + 1
	}
	return pages
}

// giveBack is one page of a pass. It reports whether the copy went back.
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
	origin := r.originOf(b)
	if dirty, held := r.privateEpoch(b); origin == nil || !dirty || held != nil || h.wholeRange(r, index) {
		// Stored into and compared since the list was made, or a range the
		// rules made one mapping, which a page given back would break in three.
		return false, nil
	}
	// Origin first, as the settle takes them: clean before private.
	if err := origin.mu.Lock(ctx); err != nil {
		return false, err
	}
	defer h.unlock(origin)
	if !r.comparable(origin) {
		// Evicted, moved out of reach, or going back once a store's mapping
		// command lands: there is nothing to compare with, and there never will
		// be again.
		r.forgetOrigin(b, origin)
		return false, nil
	}
	pg, err := h.current(ctx, b)
	if err != nil || pg == nil {
		// Spilled. Reading it back is I/O this does not do; a later pass may
		// find it resident again.
		return false, err
	}
	defer h.unlock(pg)
	return r.giveBackCopy(ctx, b, origin, pg, buffers)
}

// comparable reports whether a copy can still be compared with origin: it is
// resident, still holds its identity, is not going back once a store's mapping
// command lands, and is in a file this memory region's session was given.
// Caller holds origin's lock.
func (r *MemoryRegion) comparable(origin *resident) bool {
	h := r.host
	h.mu.Lock()
	going := origin.dropped
	h.mu.Unlock()
	return origin.published() && origin.slot >= 0 && !going && r.fileNumber(origin.file) >= 0
}

// giveBackCopy is one page of a give-back once its locks are held: the page's
// window, the memory region shared, the origin and then the copy pg. It reports
// whether the copy went back.
func (r *MemoryRegion) giveBackCopy(ctx context.Context, b *binding, origin, pg *resident, buffers *settler) (bool, error) {
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
	same, err := buffers.equal(ctx, h, origin.fileSlot, pg.fileSlot)
	if err == nil && !same && sim.Bug(ctx, "pager-give-back-changed-copy") {
		// The copy goes back although the guest stored into it, which loses
		// what it stored.
		same = true
	}
	if err != nil || !same {
		if !same && err == nil {
			// The guest stored into it, so it is its own page for good.
			r.forgetOrigin(b, origin)
		}
		return false, errors.Join(err, r.liftProtection(ctx, index))
	}
	// Mapped and installed read-only, as a move maps the owner's page, so the
	// guest's next read maps it without a fault. That read is what would
	// otherwise come back as a write.
	if err := r.mapInPlace(ctx, b, origin); err != nil {
		if !errors.Is(err, ErrMappingRefused) {
			return false, err
		}
		// The client is out of mapping budget and changed nothing, so the
		// guest still maps its copy. It takes stores again, and a later pass
		// tries again. Revoking instead would free budget, but it would leave
		// the next read to a cold fault, which copies again.
		return false, r.liftProtection(ctx, index)
	}
	if err := r.shareOrigin(ctx, b, pg, origin); err != nil {
		return false, err
	}
	return true, nil
}

// liftProtection takes off the write-protection a give-back put on a copy it keeps,
// which also wakes a store that trapped on it.
func (r *MemoryRegion) liftProtection(ctx context.Context, index uint64) error {
	if err := r.resolvePages(ctx, index, 1, true); err != nil {
		return r.fail(err)
	}
	return nil
}

// shareOrigin makes the origin this page's resident page again, now that the
// guest maps it: the binding becomes clean, and the copy and the reservation it
// was admitted under go back. Caller holds the page's window, the memory
// region shared and both pages.
func (r *MemoryRegion) shareOrigin(ctx context.Context, b *binding, pg, origin *resident) error {
	h := r.host
	note(r, b.index, "give-back", pg.slot, origin.slot)
	// The pager hands a guest back an older page on purpose here, as the settle
	// does, so the audit compares the bytes itself. See probe_on.go.
	if found := h.probe.reshared(ctx, h, pg, origin); found != "" {
		panic(found)
	}
	if err := h.unlink(ctx, b, pg); err != nil {
		return err
	}
	h.bind(b, origin)
	if slot := r.endDirty(b); slot >= 0 {
		h.releaseSpill(slot)
	}
	h.probe.retired(b)
	h.touch(origin)
	h.mu.Lock()
	h.stats.GivenBackPages++
	// A store waiting for the dirty budget has one more reservation.
	h.signal()
	h.mu.Unlock()
	return nil
}
