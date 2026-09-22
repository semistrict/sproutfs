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
	h.mu.Unlock()
	if slot >= 0 {
		return slot, nil
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
		var candidates []*resident
		busy := false
		for e := h.lru.Front(); e != nil; e = e.Next() {
			pg := e.Value.(*resident)
			if !pg.mu.TryLock() {
				busy = true
				continue
			}
			// A page a store is replacing is one the guest still reads through a
			// mapping that names this offset, and the command that stops it
			// naming it has not landed. It is not this reclaim's to take; the
			// store gives it up itself once its mapping is in.
			usable := pg.replacing == 0
			for b := range pg.aliases {
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
		if h.lru.Len() < h.cfg.ResidentPages {
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
