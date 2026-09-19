package vmmemory

import (
	"context"
	"errors"
	"maps"
	"slices"
)

// highWater is the dirty occupancy at which a store asks for a checkpoint
// instead of waiting until it is refused one: three quarters of the budget,
// leaving the last quarter to carry the guest's stores while the checkpoint it
// asked for is taken and published.
func highWater(dirty int) int { return max(1, dirty-dirty/4) }

// SetPressure installs the callbacks this host answers a full dirty budget
// with. A supervisor sets them once, before any region is attached, and clears
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
// pages, and it is the host's, not the region's: whichever checkpoint lands
// next releases reservations this store can have, so the wait is for all of
// them. With none in flight the host asks for one, which is what a dirty set
// that grows between intervals needs. Only a budget no checkpoint can relieve
// fails the store, as ErrDirtyStalled.
//
// The loss window is the other reason to wait, and it comes first: a VM that has
// held a write no checkpoint covers for longer than the window admits no further
// dirty page however much room the budget has. The wait is the same wait — ask
// for the checkpoint, sleep on the host's own signal, stall where none is coming
// — because what ends it is the same thing, a checkpoint of this VM landing.
func (h *Host) takeSpill(ctx context.Context, r *Region) (int, error) {
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
		if n := len(h.freeSpill); n > 0 && !over {
			slot := h.freeSpill[n-1]
			h.freeSpill = h.freeSpill[:n-1]
			h.dirty++
			h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
			h.mu.Unlock()
			return slot, nil
		}
		h.mu.Unlock()
		if over {
			if !h.windowRelief(r) {
				h.stall(r, ErrWindowStalled)
				return 0, ErrWindowStalled
			}
		} else if !h.relief() {
			h.stall(r, ErrDirtyStalled)
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
// again for as long as it waits — and it holds no region, page or reservation
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
// offering the regions holding the largest dirty sets first, since those
// release the most. It holds no lock while it asks: the callback runs on the
// waiting store's goroutine and must not reach back into the host.
func (h *Host) relief() bool {
	h.mu.Lock()
	attached := slices.Collect(maps.Keys(h.regions))
	request := h.pressure.Checkpoint
	h.mu.Unlock()
	type candidate struct {
		region *Region
		dirty  int
	}
	var candidates []candidate
	relieving := false
	for _, region := range attached {
		if sealing, c := region.sealState(); sealing || c != nil {
			// Sealed, or being sealed, so no further checkpoint of it can be
			// asked for. It answers the wait only when retiring it gives
			// reservations back: a fork hold lasts as long as the children it
			// named, and a checkpoint whose pages own none releases nothing
			// either. A seal still in progress is waited for, and asked about
			// again once it has recorded what it took.
			relieving = relieving || sealing || c.relieves()
			continue
		}
		if dirty := region.dirtyCount(); dirty > 0 {
			candidates = append(candidates, candidate{region, dirty})
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
		if request(c.region) {
			h.mu.Lock()
			h.stats.CheckpointRequests++
			h.mu.Unlock()
			return true
		}
	}
	return false
}

// stall reports a store nothing can admit to the region's owner, which stops
// that VM deliberately. The store still fails, because the guest cannot be left
// waiting on a checkpoint nothing will take; what the report buys is a logged
// reason and a stop that publishes, in place of a fault failure that only kills
// the VMM. cause says which bound the store ran into, since a deployment answers
// the two differently: a budget too small for its guests, or a VM whose writes
// cannot be published at all.
func (h *Host) stall(r *Region, cause error) {
	h.mu.Lock()
	stop := h.pressure.Stop
	if errors.Is(cause, ErrWindowStalled) {
		h.stats.WindowStalls++
	} else {
		h.stats.DirtyStalls++
	}
	h.mu.Unlock()
	if stop != nil {
		stop(r, cause)
	}
}
