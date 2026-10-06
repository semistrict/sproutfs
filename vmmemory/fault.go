package vmmemory

import (
	"context"
	"errors"
)

// Fault orders operations only within this memory region's read-ahead window. Shared
// page transitions additionally take that page's lock. Different windows and
// volumes can load, spill, and flush concurrently, bounded by ConcurrentIO and
// the resident/dirty budgets. A read fault loads and maps as much of its window
// as free slots allow; pages of the window that are already resident under the
// same stored identity are mapped without loading anything.
//
// A read's lookup is the region layer's lookup cursor, Zircon's
// RequireReadPage, which falls through to the identity root of the page. A
// page no object holds yet is a READ request, which the fault answers by
// reading the page into a frame and supplying it. A store makes a page of the
// layer Dirty: a copy of the page it maps, at the offset the placement rule
// gives it, or fresh zeros (store.go). Which pages a fault reads and in what
// order is faultfirst.go's: its page first, the rest of its window prefetched
// behind it, a fault at random alone, and a post-copy stream's whole run at
// once.
func (r *MemoryRegion) Fault(ctx context.Context, index uint64, write bool) error {
	if index >= uint64(r.pageCount) {
		return ErrRange
	}
	// Whatever this fault admitted to the dirty budget is measured against
	// the high-water mark here, where it holds nothing.
	defer r.host.askAtHighWater()
	reserve := false
	for range faultAttempts {
		// A store that needs a page of its own takes its dirty reservation
		// before any region, page or I/O resource, as in the current core,
		// and so does a read of a page only another host holds, which the
		// load makes the region's own dirty state.
		var spill reservation
		if reserve || (write && r.needsPrivatePage(index)) {
			taken, err := r.host.takeSpill(ctx, r)
			if err != nil {
				return err
			}
			spill = taken
		}
		retry, err := r.faultOnce(ctx, index, write, &spill)
		if !spill.none() {
			r.host.releaseSpill(spill)
		}
		if errors.Is(err, errUnpublishedReservation) {
			reserve, retry, err = true, true, nil
		}
		if !retry {
			return err
		}
	}
	return ErrContended
}

// errUnpublishedReservation reports a fault whose window turned out to hold
// pages no checkpoint has, which the load takes as this memory region's dirty state.
// It never leaves the package: the fault releases everything it holds, takes a
// dirty reservation through the waiting path, and tries again.
var errUnpublishedReservation = errors.New("managed-memory fault needs a dirty reservation")

// faultAttempts bounds how often a fault re-decides whether it needs a private
// page. Only a seal taken between that decision and the memory region lock can force
// another attempt, so one repetition is enough in every observed case.
const faultAttempts = 8

// around narrows [first, last) to at most n pages that still hold index,
// preferring the pages after it: access tends to continue forward.
func around(index, first, last uint64, n int) (uint64, uint64) {
	if last-first <= uint64(n) {
		return first, last
	}
	start := max(first, min(index, last-uint64(n)))
	return start, start + uint64(n)
}

// loadAttempts bounds how often a fault retries after losing a publication race
// for its identity. Each retry begins by waiting for the winner while holding
// no other resident lock, so one is enough in every observed case.
const loadAttempts = 64

// end is the window end that names no faulting page: a plan for it resolves
// nothing.
func (r *MemoryRegion) end(index uint64) uint64 {
	_, end := r.window(index)
	return end
}
