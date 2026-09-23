package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/resource"
)

// residentSlot is what one arena offset holding a page costs: the resource
// reservation it was admitted under, and the extent it belongs to where the
// placement rule put it there rather than the allocator. An offset of an extent
// stays that extent's when its page goes; an ordinary one goes back to the
// offset space with it.
type residentSlot struct {
	lease  *resource.Lease
	extent *extent
}

// takeFree takes count consecutive free slots starting at slot, against the
// host budget: the pages they will hold are what that budget bounds, so a
// refused reservation is a refused allocation. Caller holds h.mu.
func (h *Host) takeFree(slot, count int) bool {
	if count > h.slots.Free() {
		// The arena's addresses are not its capacity: an offset run this long
		// exists, and the memory behind it does not.
		return false
	}
	lease, err := h.resources.TryAcquire(context.Background(), int64(count)*int64(h.pageSize))
	if err != nil {
		return false
	}
	h.slots.Take(slot, count)
	for s := slot; s < slot+count; s++ {
		h.residentLeases[s] = residentSlot{lease: lease}
	}
	h.stats.PeakResidentPages = max(h.stats.PeakResidentPages, h.slots.Held())
	return true
}

// putFree returns one slot and the reservation it held. A slot with no
// reservation, or a reservation that will not take its bytes back, is an
// accounting invariant this host has broken: it makes the host terminal, which
// stops the VMs it runs deliberately, rather than killing the process they run
// in. The slot is not returned either, because nothing knows what it holds.
// Caller holds h.mu.
func (h *Host) putFree(slot int) {
	entry := h.residentLeases[slot]
	if entry.lease == nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: slot %d freed without a resource reservation", slot))
		return
	}
	if err := entry.lease.Release(int64(h.pageSize)); err != nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: releasing slot %d: %w", slot, err))
		return
	}
	if entry.lease.Bytes() == 0 {
		entry.lease.Close()
	}
	delete(h.residentLeases, slot)
	if e := entry.extent; e != nil {
		// The memory leaves and the address stays the range's, until the last
		// page of the extent goes and the extent itself does.
		h.slots.Empty()
		e.held--
		h.dropExtent(e)
		return
	}
	h.slots.Put(slot)
}

// allocateFree takes up to want consecutive free slots without evicting. It
// returns the first slot and the count taken, which may be zero. Contiguous
// slots let consecutive pages become one mapping; the scan is bounded so a
// fragmented arena costs a shorter run, not a long search. A run the budget
// will not reserve whole is halved rather than abandoned.
func (h *Host) allocateFree(want int) (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bestStart, bestLen := h.slots.LongestRun(want)
	for bestLen > 0 {
		if h.takeFree(bestStart, bestLen) {
			return bestStart, bestLen
		}
		bestLen /= 2
	}
	return 0, 0
}

// allocateFreeFrom is allocateFree preferring the whole run to start at
// prefer, so that a new mapping continues the slots of the one before it.
func (h *Host) allocateFreeFrom(prefer, want int) (int, int) {
	h.mu.Lock()
	if prefer >= 0 && prefer <= h.slots.Offsets()-want && want <= h.slots.Free() {
		count := 0
		for count < want && h.slots.IsFree(prefer+count) {
			count++
		}
		if count == want && h.takeFree(prefer, count) {
			h.mu.Unlock()
			return prefer, count
		}
	}
	h.mu.Unlock()
	return h.allocateFree(want)
}

// allocatePrivate takes the arena offset a private page of this index goes at:
// the offset the placement rule gives it within its range's extent, evicting
// where the page budget rather than the address is what is missing. A page the
// rule has no offset for — a pager that places nothing, no extent left, or an
// offset already holding the bytes a checkpoint froze — falls back to an
// ordinary one beside its neighbours.
func (r *Region) allocatePrivate(ctx context.Context, index uint64) (int, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	h := r.host
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return 0, err
	}
	slot, placeable := h.place(r, index)
	noExtent := !placeable && h.placing() && h.slots.FreeExtents() == 0
	h.mu.Unlock()
	if slot >= 0 {
		return slot, nil
	}
	if noExtent {
		// The offset space's extents are held by the idle pages of regions
		// that have gone; one is given back for this range.
		freed, err := h.reclaimExtent(ctx)
		if err != nil {
			return 0, err
		}
		if freed {
			h.mu.Lock()
			slot, placeable = h.place(r, index)
			h.mu.Unlock()
			if slot >= 0 {
				return slot, nil
			}
		}
	}
	if !placeable {
		return r.allocateNear(ctx, index)
	}
	return h.allocate(ctx, func() int {
		slot, _ := h.place(r, index)
		return slot
	}, sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5))
}

// Prefer extending a neighboring mapping's physical run before using the
// first free slot. This consumes no speculative reservation and never waits
// for a preferred slot; pressure falls back to ordinary bounded reclamation.
func (r *Region) allocateNear(ctx context.Context, index uint64) (int, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	h := r.host
	// Taking a victim while free slots are there is legal and merely wasteful,
	// and it is the only way an arena that is not full reaches the eviction
	// paths at all: a pager sized to hold its whole guest never evicts, so
	// nothing ever overlaps an eviction with a publication or a seal.
	if preferEviction := sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5); preferEviction {
		return h.allocate(ctx, nil, true)
	}
	for _, delta := range []int64{-1, 1} {
		neighbor := int64(index) + delta
		if neighbor < 0 || neighbor >= int64(r.pageCount) {
			continue
		}
		b := r.lookupBinding(uint64(neighbor))
		if b == nil {
			continue
		}
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return 0, err
		}
		if pg := b.resident; pg != nil && pg.slot >= 0 {
			slot := pg.slot - int(delta)
			if slot >= 0 && slot < h.slots.Offsets() && h.slots.IsFree(slot) && h.takeFree(slot, 1) {
				h.mu.Unlock()
				return slot, nil
			}
		}
		h.mu.Unlock()
	}
	return h.allocate(ctx, nil, false)
}

// allocate returns one slot, evicting the least recently used unlocked page
// when the arena is full. It waits for progress rather than failing while
// every candidate is temporarily busy.
//
// place, where it is not nil, is where the slot must be: the placement rule has
// already decided this page's offset, so only the page budget is at stake and
// only an eviction anywhere can make room for it. It is called under the host
// lock and reports the offset it took, or -1 while that room is not there yet.
//
// preferEviction takes a victim even where a free slot would do, for one pass:
// the iteration after a fruitless preference takes the free slot, so a
// buggified allocation cannot wait on a victim that will not come.
func (h *Host) allocate(ctx context.Context, place func() int, preferEviction bool) (int, error) {
	for {
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return 0, err
		}
		resourceChanged := h.resources.Changed()
		capacityBlocked := false
		if place != nil {
			if !preferEviction {
				if slot := place(); slot >= 0 {
					h.mu.Unlock()
					return slot, nil
				}
			}
			// The address is this page's whatever happens; what is missing is a
			// page of the budget, which every eviction gives back.
			capacityBlocked = true
		} else if slot := h.slots.First(); slot >= 0 && !preferEviction && h.takeFree(slot, 1) {
			h.mu.Unlock()
			return slot, nil
		} else if slot >= 0 {
			capacityBlocked = true
		}
		// An idle page is the first thing given up: no region maps it, so its
		// slot costs no revocation and no spill, and nothing a guest is using.
		if !preferEviction {
			if pg := h.takeIdleLocked(); pg != nil {
				h.mu.Unlock()
				if err := h.dropIdle(ctx, pg); err != nil {
					return 0, err
				}
				continue
			}
		}
		var candidates []*resident
		busy := false
		for pg := h.lru.front(); pg != nil; pg = h.lru.next(pg) {
			if !pg.mu.TryLock() {
				busy = true
				continue
			}
			// A page a store is replacing is one the guest still reads through a
			// mapping that names this offset, and the command that stops it
			// naming it has not landed. It is not this reclaim's to take; the
			// store gives it up itself once its mapping is in.
			usable := pg.replacing == 0
			for b := range pg.aliases.all() {
				if b.region.terminal.Load() != nil {
					usable = false
					break
				}
			}
			if usable {
				// One victim per reclaim: a page is the whole scratch budget one
				// eviction may read out of the arena.
				candidates = append(candidates, pg)
				break
			}
			pg.mu.Unlock()
		}
		changed := h.changed
		// Slots reserved by a concurrent load have no LRU entry yet.
		if h.lru.len() < h.cfg.ResidentPages {
			busy = true
		}
		h.mu.Unlock()
		if len(candidates) > 0 {
			preferEviction = false
			err := h.evictBatch(ctx, candidates)
			h.unlockAll(candidates)
			// A victim another region will not give up is not this allocation's
			// failure: the next pass skips it, because that region is terminal
			// from here, and takes another page. The arena is finite, so every
			// such pass removes one page from what this loop will consider,
			// and an arena made entirely of them reports ErrCapacity rather
			// than spinning.
			if err != nil && !errors.Is(err, errVictimHeld) {
				return 0, err
			}
			continue
		}
		if preferEviction {
			// Nothing was evictable this pass. Take the free slot next one
			// rather than wait for a victim.
			preferEviction = false
			continue
		}
		if !busy && !capacityBlocked {
			return 0, ErrCapacity
		}
		select {
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		case <-changed:
		case <-resourceChanged:
		}
	}
}

// takeIdleLocked locks and returns the oldest idle page it can take without
// waiting, or nil where there is none. Caller holds h.mu.
func (h *Host) takeIdleLocked() *resident { return h.takeIdleWhereLocked(nil) }

// takeIdleWhereLocked is takeIdleLocked for the idle pages want accepts, or
// any where want is nil. Caller holds h.mu.
func (h *Host) takeIdleWhereLocked(want func(*resident) bool) *resident {
	for pg := h.idle.front(); pg != nil; pg = h.idle.next(pg) {
		if want != nil && !want(pg) {
			continue
		}
		if !pg.mu.TryLock() {
			continue
		}
		if pg.aliases.len() == 0 && pg.replacing == 0 {
			return pg
		}
		pg.mu.Unlock()
	}
	return nil
}

// dropIdle gives up one idle page takeIdleLocked returned locked: its identity
// stops naming it, so the next region that inherits it reads it again, and its
// slot is free.
func (h *Host) dropIdle(ctx context.Context, pg *resident) error {
	err := h.release(ctx, pg)
	if err == nil {
		h.mu.Lock()
		h.stats.IdleDrops++
		h.mu.Unlock()
	}
	h.unlockAll([]*resident{pg})
	return err
}

// makeRoom gives up idle pages until want slots are free, or no idle page is
// left. The allocations that take free slots only — a store's write-ahead run,
// a load's read-ahead — never evict, so an arena full of idle pages would
// otherwise shrink every one of them to the single page that may.
func (h *Host) makeRoom(ctx context.Context, want int) error {
	for {
		h.mu.Lock()
		if h.slots.Free() >= want {
			h.mu.Unlock()
			return nil
		}
		pg := h.takeIdleLocked()
		h.mu.Unlock()
		if pg == nil {
			return nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return err
		}
	}
}

// reclaimIdle is the host budget's cache eviction for this pager: it gives up
// one idle page, reporting whether it did. The budget calls it for whichever
// consumer is short, and that may be this pager, from inside an allocation
// that holds h.mu, or the other pager of this host from inside one of its own,
// which is why it never waits for h.mu: an allocation of this pager gives up
// idle pages itself, and another consumer that finds the lock taken waits for
// the budget's next release, which a pager this busy is about to make.
func (h *Host) reclaimIdle(ctx context.Context, _ int64) (bool, error) {
	if !h.mu.TryLock() {
		return false, nil
	}
	pg := h.takeIdleLocked()
	h.mu.Unlock()
	if pg == nil {
		return false, nil
	}
	return true, h.dropIdle(ctx, pg)
}

// DropIdle gives up every idle page this host can take without waiting, and
// reports how many it gave up. A host that wants its memory back rather than
// kept for the next VM to inherit calls it; so does a test whose machine must
// fault every page from scratch.
func (h *Host) DropIdle(ctx context.Context) (int, error) {
	dropped := 0
	for {
		h.mu.Lock()
		pg := h.takeIdleLocked()
		h.mu.Unlock()
		if pg == nil {
			return dropped, nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return dropped, err
		}
		dropped++
	}
}

// reclaimExtent gives up idle pages until an extent of the offset space is
// free, where none is, and reports whether it freed one. A published page
// stays at the offset the placement rule gave it when it goes idle, so it keeps
// that offset's extent from going back after the region that placed it has
// gone: an arena full of the idle pages of stopped VMs would otherwise have no
// extent left for a running one, and every private page of it would be a page
// of its own somewhere in the arena.
func (h *Host) reclaimExtent(ctx context.Context) (bool, error) {
	orphaned := func(pg *resident) bool {
		e := h.residentLeases[pg.slot].extent
		return e != nil && h.extents[e.key] != e
	}
	for {
		h.mu.Lock()
		if !h.placing() || h.slots.FreeExtents() > 0 {
			freed := h.placing() && h.slots.FreeExtents() > 0
			h.mu.Unlock()
			return freed, nil
		}
		pg := h.takeIdleWhereLocked(orphaned)
		h.mu.Unlock()
		if pg == nil {
			return false, nil
		}
		if err := h.dropIdle(ctx, pg); err != nil {
			return false, err
		}
	}
}
