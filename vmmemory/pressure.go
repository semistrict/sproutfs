package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// highWater is the dirty occupancy at which a store asks for a checkpoint
// instead of waiting until it is refused one: three quarters of the budget,
// leaving the last quarter to carry the guest's stores while the checkpoint it
// asked for is taken and published.
func highWater(dirty int) int { return max(1, dirty-dirty/4) }

// SetPressure installs the callbacks this host answers a full dirty budget
// with. A supervisor sets them once, before any memory region is attached, and clears
// them by installing a zero Pressure before it stops answering.
func (h *Host) SetPressure(p Pressure) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pressure = p
}

// takeSpill admits one more private page to the dirty budget, waiting for room
// rather than failing the store that needs it: a guest that dirties faster than
// its checkpoints drain is a guest to stall, and failing its fault instead
// closes the session and kills its VMM.
//
// The budget counts the pages a checkpoint still holds as well as live private
// pages, and it is the host's, not the memory region's: whichever checkpoint lands
// next releases reservations this store can have, so the wait is for all of
// them. With none in flight the host asks for one, which is what a dirty set
// that grows between intervals needs. A budget no checkpoint can relieve is
// taken back from the memory region holding the most of it, which its owner
// stops. The store waits for that stop, or fails as ErrDirtyStalled when its own
// memory region is the one stopped.
//
// The loss window is the other reason to wait, and it comes first: a VM that has
// held a write no checkpoint covers for longer than the window admits no further
// dirty page however much room the budget has. The wait is the same wait — ask
// for the checkpoint, sleep on the host's own signal, stall where none is coming
// — because what ends it is the same thing, a checkpoint of this VM landing.
func (h *Host) takeSpill(ctx context.Context, r *MemoryRegion) (int, error) {
	for {
		// The signal this attempt will wait on is taken before anything is
		// decided, because deciding takes locks of its own: a checkpoint that
		// lands between reading the window and reading the budget would
		// otherwise close a signal this store is not listening to yet, and the
		// store would wait on the next one with nothing left to give it.
		changed := h.changes()
		over := h.overWindow(r)
		h.mu.Lock()
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return 0, err
		}
		if !over {
			if slot, ok := h.reservations.take(); ok {
				h.dirty++
				h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
				h.mu.Unlock()
				return slot, nil
			}
		}
		h.mu.Unlock()
		if over {
			if !h.windowRelief(r) {
				h.stall(r, ErrWindowStalled)
				return 0, ErrWindowStalled
			}
		} else if !h.relief() && !h.takeBack(r) {
			return 0, ErrDirtyStalled
		}
		h.mu.Lock()
		if over {
			h.stats.WindowWaits++
		} else {
			h.stats.DirtyWaits++
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		case <-changed:
		}
	}
}

// askAtHighWater asks for the checkpoint that relieves a dirty budget which has
// crossed its high-water mark, so that it is already being taken when the last
// quarter runs out instead of being asked for by a store already waiting. Every
// path that admits a private page ends in a fault returning, which is where
// this runs: the reservation a store waits for, the one a peer-served load
// takes, and the run of them write-ahead takes without waiting, any of which
// can cross the mark. Once per crossing is enough — a store that waits asks
// again for as long as it waits — and it holds no memory region, page or reservation
// when it asks.
func (h *Host) askAtHighWater() {
	h.mu.Lock()
	crossed := h.dirty >= h.highWater && !h.asked
	h.asked = h.asked || crossed
	h.mu.Unlock()
	if crossed {
		h.relief()
	}
}

// relief reports whether some checkpoint will release dirty reservations. One
// already in flight will when it retires; otherwise the host asks for one,
// offering the memory regions holding the largest dirty sets first, since those
// release the most. It holds no lock while it asks: the callback runs on the
// waiting store's goroutine and must not reach back into the host.
//
// A memory region whose owner is stopping it relieves the budget too: it gives
// every page it holds back when it detaches.
func (h *Host) relief() bool {
	h.mu.Lock()
	attached := slices.Collect(maps.Keys(h.memoryRegions))
	stopping := slices.ContainsFunc(attached, func(r *MemoryRegion) bool { return r.stopping })
	request := h.pressure.Checkpoint
	h.mu.Unlock()
	if stopping {
		return true
	}
	type candidate struct {
		memoryRegion *MemoryRegion
		dirty        int
	}
	var candidates []candidate
	relieving := false
	for _, memoryRegion := range attached {
		if sealing, c := memoryRegion.sealState(); sealing || c != nil {
			// Sealed, or being sealed, so no further checkpoint of it can be
			// asked for. It answers the wait only when retiring it gives
			// reservations back: a fork hold lasts as long as the children it
			// named, and a checkpoint whose pages own none releases nothing
			// either. A seal still in progress is waited for, and asked about
			// again once it has recorded what it took.
			relieving = relieving || sealing || c.relieves()
			continue
		}
		if dirty := memoryRegion.dirtyCount(); dirty > 0 {
			candidates = append(candidates, candidate{memoryRegion, dirty})
		}
	}
	if relieving {
		return true
	}
	if request == nil {
		return false
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return b.dirty - a.dirty })
	for _, c := range candidates {
		if request(c.memoryRegion) {
			h.mu.Lock()
			h.stats.CheckpointRequests++
			h.mu.Unlock()
			return true
		}
	}
	return false
}

// takeBack ends a full dirty budget that no checkpoint can relieve, for a store
// into r. The budget is taken back from the memory region that holds the most of
// it. So the owner is asked to stop the memory regions holding more of it than r,
// largest first, and r last. A guest that holds little of a budget is never
// stopped for one that holds much. A guest that stores into all of its RAM
// holds most of a RAM budget, and no checkpoint the interval takes gives it
// back, so this is where such a guest ends and its neighbour does not.
//
// It reports whether the store may wait: another memory region is being
// stopped, and every page it holds comes back when it detaches. The store fails
// when its own memory region is the one stopped, or when no owner will stop any
// of them.
func (h *Host) takeBack(r *MemoryRegion) bool {
	h.mu.Lock()
	attached := slices.Collect(maps.Keys(h.memoryRegions))
	h.mu.Unlock()
	type holder struct {
		memoryRegion *MemoryRegion
		dirty        int
	}
	own := r.dirtyCount()
	var larger []holder
	for _, memoryRegion := range attached {
		if dirty := memoryRegion.dirtyCount(); memoryRegion != r && dirty > own {
			larger = append(larger, holder{memoryRegion, dirty})
		}
	}
	slices.SortFunc(larger, func(a, b holder) int { return b.dirty - a.dirty })
	for _, c := range larger {
		cause := fmt.Errorf("%w: this memory region holds %d of the budget's %d pages, more than the store waiting for one",
			ErrDirtyStalled, c.dirty, h.cfg.DirtyPages)
		if h.stop(c.memoryRegion, cause) {
			return true
		}
	}
	h.stall(r, fmt.Errorf("%w: a store into this memory region needs one of the budget's %d pages, and it holds %d of them",
		ErrDirtyStalled, h.cfg.DirtyPages, own))
	return false
}

// stall ends a store that nothing can admit. It asks the owner to stop the
// store's own VM, and the store fails either way.
func (h *Host) stall(r *MemoryRegion, cause error) {
	if !h.stop(r, cause) {
		h.mu.Lock()
		h.countStall(cause)
		h.mu.Unlock()
	}
}

// stop asks a memory region's owner to stop its VM deliberately, because a bound
// nothing else can relieve has run out. What that buys is a logged reason and a
// stop that publishes, in place of a fault failure that only kills the VMM.
// cause says which bound it was, since a deployment answers the two
// differently: a budget too small for its guests, or a VM whose writes cannot be
// published at all.
//
// It reports whether the owner will stop it, now or because an earlier store
// asked. An owner that does not run the memory region's VM declines. Each
// memory region is stopped, and counted, once.
func (h *Host) stop(r *MemoryRegion, cause error) bool {
	h.mu.Lock()
	stopping, stop := r.stopping, h.pressure.Stop
	h.mu.Unlock()
	if stopping {
		return true
	}
	if stop == nil || !stop(r, cause) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !r.stopping {
		r.stopping = true
		h.countStall(cause)
	}
	return true
}

// countStall counts one bound that ran out. Caller holds h.mu.
func (h *Host) countStall(cause error) {
	if errors.Is(cause, ErrWindowStalled) {
		h.stats.WindowStalls++
	} else {
		h.stats.DirtyStalls++
	}
}
