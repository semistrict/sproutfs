package vmmemory

import (
	"context"
	"time"
)

// Serving a migration's destination and handing a region off, over the
// zircon core, as serve.go does for the current core: the pages a region
// holds are its bindings' pages, resident or spilled, and those that are its
// own dirty state are what no checkpoint has.

// readResident is MemoryRegion.ReadResident over the zircon core: one page's
// current bytes for a peer, whether this host holds them, and whether they
// are the region's own state rather than the volume's. A page the guest
// still shares with a checkpoint is served from the checkpoint's copy, the
// same page the guest reads.
func (z *zirconRegion) readResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error) {
	r := z.region
	h := r.host
	if uint64(len(dst)) != h.pageSize {
		return false, false, ErrRange
	}
	if err := r.live.RLock(ctx); err != nil {
		return false, false, err
	}
	defer r.live.RUnlock()
	if page >= uint64(r.pageCount) {
		return false, false, ErrRange
	}
	// The stripe orders this against the faults that change who owns a
	// page's bytes, and comes before the region, as in a fault.
	if err := r.stripe(page).Lock(ctx); err != nil {
		return false, false, err
	}
	defer r.stripe(page).Unlock()
	if err := r.mu.RLock(ctx); err != nil {
		return false, false, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return false, false, err
	}
	b := z.lookupBinding(page)
	if b == nil {
		return false, false, nil
	}
	if err := h.beginIO(ctx); err != nil {
		return false, false, err
	}
	defer h.endIO()
	p, err := z.host.lockedPage(ctx, b)
	if err != nil {
		return false, false, err
	}
	z.mu.Lock()
	dirty, copied := b.dirty, b.checkpoint
	spill := b.spill
	if copied != nil {
		spill = copied.spill
	}
	z.mu.Unlock()
	if p != nil {
		defer z.host.unlockPage(p)
		f := frameOf(p)
		if err := f.file.Read(ctx, f.slot, dst); err != nil {
			return false, false, err
		}
		return true, dirty, nil
	}
	if !dirty || (copied == nil && !h.spillHolds(spill)) {
		// Evicted since it was listed, or never held at all.
		return false, false, nil
	}
	if err := h.readSpill(ctx, spill, dst); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// resident is MemoryRegion.Resident over the zircon core: the pages holding
// host memory or private state, in ascending order.
func (z *zirconRegion) resident() ([]uint64, error) {
	r := z.region
	if err := r.mu.RLock(context.Background()); err != nil {
		return nil, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return nil, err
	}
	var result []uint64
	z.eachBinding(func(b *zbinding) {
		if b.page != nil || b.dirty {
			result = append(result, b.index)
		}
	})
	return result, nil
}

// unpublished is MemoryRegion.Unpublished over the zircon core: the pages
// the region holds that no checkpoint of its VM has, in ascending order.
func (z *zirconRegion) unpublished() ([]uint64, error) {
	r := z.region
	if err := r.mu.RLock(context.Background()); err != nil {
		return nil, err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return nil, err
	}
	var result []uint64
	z.eachBinding(func(b *zbinding) {
		if b.dirty {
			result = append(result, b.index)
		}
	})
	return result, nil
}

// handoff is MemoryRegion.Handoff over the zircon core: the region gives up
// its volume and keeps its pages, and reports how long it has held its
// oldest unpublished write. A sealed region keeps its volume.
func (z *zirconRegion) handoff(ctx context.Context) (time.Duration, error) {
	r := z.region
	if err := r.mu.Lock(ctx); err != nil {
		return 0, err
	}
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return 0, err
	}
	if r.currentCheckpoint() != nil {
		return 0, ErrSealed
	}
	r.handed = true
	return r.unpublishedAge(), nil
}
