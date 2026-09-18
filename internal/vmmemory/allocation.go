package vmmemory

import (
	"context"

	"errors"
	"fmt"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// takeFree takes count consecutive free slots starting at slot, against the
// host budget: the pages they will hold are what that budget bounds, so a
// refused reservation is a refused allocation. Caller holds h.mu.
func (h *Host) takeFree(slot, count int) bool {
	lease, err := h.resources.TryAcquire(context.Background(), int64(count)*int64(PageSize))
	if err != nil {
		return false
	}
	h.slots.Take(slot, count)
	for s := slot; s < slot+count; s++ {
		h.residentLeases[s] = lease
	}
	h.stats.PeakResidentPages = max(h.stats.PeakResidentPages, h.slots.Total()-h.slots.Free())
	return true
}

// putFree returns one slot and the reservation it held. A slot with no
// reservation, or a reservation that will not take its bytes back, is an
// accounting invariant this host has broken: it makes the host terminal, which
// stops the VMs it runs deliberately, rather than killing the process they run
// in. The slot is not returned either, because nothing knows what it holds.
// Caller holds h.mu.
func (h *Host) putFree(slot int) {
	lease := h.residentLeases[slot]
	if lease == nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: slot %d freed without a resource reservation", slot))
		return
	}
	if err := lease.Release(int64(PageSize)); err != nil {
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: releasing slot %d: %w", slot, err))
		return
	}
	if lease.Bytes() == 0 {
		lease.Close()
	}
	h.residentLeases[slot] = nil
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
	if prefer >= 0 && prefer <= h.cfg.ResidentPages-want {
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
		return h.allocate(ctx, true)
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
			if slot >= 0 && slot < h.cfg.ResidentPages && h.slots.IsFree(slot) && h.takeFree(slot, 1) {
				h.mu.Unlock()
				return slot, nil
			}
		}
		h.mu.Unlock()
	}
	return h.allocate(ctx, false)
}

// allocate returns one slot, evicting the least recently used unlocked page
// when the arena is full. It waits for progress rather than failing while
// every candidate is temporarily busy.
//
// preferEviction takes a victim even where a free slot would do, for one pass:
// the iteration after a fruitless preference takes the free slot, so a
// buggified allocation cannot wait on a victim that will not come.
func (h *Host) allocate(ctx context.Context, preferEviction bool) (int, error) {
	for {
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return 0, err
		}
		resourceChanged := h.resources.Changed()
		capacityBlocked := false
		if slot := h.slots.First(); slot >= 0 && !preferEviction && h.takeFree(slot, 1) {
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
			usable := true
			for b := range pg.aliases {
				if b.region.terminal.Load() != nil {
					usable = false
					break
				}
			}
			if usable {
				// One victim per reclaim: a page is 2 MiB, which is the whole
				// scratch budget one eviction may read out of the arena.
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
