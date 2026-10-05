package vmmemory

import (
	"context"
	"errors"
	"sort"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// reservation is the dirty reservation a page was admitted under: a reference
// of the pager's spill storage (zirconvm.SpillStorage), which is where the
// page's bytes go when it is spilled, or none. A page takes it before it is
// dirty, so a spill never needs room (D5 of the Zircon port). Whether the
// reference holds the page's bytes is the storage's state, not the binding's:
// an eviction writes those bytes while a seal may be handing the reservation
// to the checkpoint's copy of the page, and only the reference is named by
// both.
type reservation struct {
	ref   zirconvm.ReferenceValue
	taken bool
}

// noReservation is no reservation at all.
var noReservation reservation

// none reports whether this is no reservation.
func (r reservation) none() bool { return !r.taken }

// spillHolds reports whether a reservation holds its page's bytes.
func (h *Host) spillHolds(spill reservation) bool {
	return !spill.none() && h.spill.Holds(spill.ref)
}

// ErrSpillCorrupt reports a spilled page whose bytes are not the ones that were
// written to its reservation. The spill file is scratch on a local device, so
// the authority for what it should hold lives in this process and not in the
// file: a page that comes back short, zeroed or holding bytes nobody wrote is a
// page the device lost, and handing it to the guest would be handing the guest
// silently wrong memory.
var ErrSpillCorrupt = errors.New("vmmemory: spilled page does not match its checksum")

// readSpill reads the bytes a reservation holds into dst.
func (h *Host) readSpill(ctx context.Context, spill reservation, dst []byte) error {
	if spill.none() {
		return errors.New("private page has no current backing")
	}
	if _, _, err := h.spill.CompressedData(ctx, spill.ref, dst); err != nil {
		if errors.Is(err, zirconvm.ErrIODataIntegrity) {
			return errors.Join(ErrSpillCorrupt, err)
		}
		return err
	}
	return nil
}

// releaseSpill returns a dirty page's reservation to the host and wakes
// waiters.
func (h *Host) releaseSpill(spill reservation) {
	h.mu.Lock()
	h.spill.Free(spill.ref)
	h.dirty--
	if h.dirty < h.highWater {
		// Back under the mark: the next store to cross it asks again.
		h.asked = false
	}
	h.signal()
	h.mu.Unlock()
}

// takeReservationLocked admits one more private page to the dirty budget if
// the budget has room. Caller holds h.mu.
func (h *Host) takeReservationLocked() (reservation, bool) {
	ref, ok := h.spill.Reserve()
	if !ok {
		return noReservation, false
	}
	h.dirty++
	h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
	return reservation{ref: ref, taken: true}, true
}

// tryTakeSpill admits one more private page to the dirty budget without
// waiting. A load that cannot have one fails rather than holding resident locks
// and an I/O permit while the budget frees up.
func (h *Host) tryTakeSpill() (reservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return noReservation, h.err
	}
	if spill, ok := h.takeReservationLocked(); ok {
		return spill, nil
	}
	return noReservation, ErrCapacity
}

// takeFreeSpill admits up to want more private pages to the dirty budget
// without waiting: it takes only reservations that are free now, which is all
// write-ahead may use.
func (h *Host) takeFreeSpill(want int) []reservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil || want <= 0 {
		return nil
	}
	spills := make([]reservation, 0, min(want, h.spill.Available()))
	for len(spills) < want {
		spill, ok := h.takeReservationLocked()
		if !ok {
			break
		}
		spills = append(spills, spill)
	}
	return spills
}

// evictionSeam runs in a reclaim between reading one victim's aliases and
// reading the reservations those aliases name, which is the one moment a seal
// can move a page's reservation to the checkpoint's copy of it without the
// reclaim seeing either state. Production leaves it nil; a test installs it to
// take a seal exactly there.
var evictionSeam func(slot int)

// All victims are already locked. Revoke every alias before reading its bytes.
// A scratch write provides live read-after-write backing; no Sync is needed
// because only the volume quorum acknowledges durability. No arena slot is
// released until the entire bounded scratch batch has succeeded.
func (h *Host) evictBatch(ctx context.Context, victims []*resident) error {
	// The reservation each page's bytes go to is read once, here: a seal taken
	// while this runs hands a page's reservation to the checkpoint's copy of it
	// and joins that copy to the page, so a reservation can be named by the
	// page before this walk and by the copy after it, and is written once
	// either way.
	type spillPage struct {
		spill    reservation
		resident *resident
	}
	var pages []spillPage
	taken := make(map[reservation]bool)
	byMemoryRegion := make(map[*MemoryRegion][]*binding)
	for _, pg := range victims {
		// The seal joins the checkpoint's copy to the page before it hands
		// that copy the page's reservation, so an alias set that has not grown
		// since its reservations were read names every reservation the page's
		// bytes can be in. One that has grown is walked again: the alias the
		// reservation moved to is in it, and a page whose reservation this walk
		// already read is not read again, because the bytes are the same bytes
		// whichever alias owns them by the time they are written.
		walked := make(map[*binding]bool)
		for grown := true; grown; {
			grown = false
			aliases := h.aliases(pg)
			if evictionSeam != nil {
				evictionSeam(pg.slot)
			}
			for _, b := range aliases {
				if walked[b] {
					continue
				}
				walked[b], grown = true, true
				byMemoryRegion[b.memoryRegion] = append(byMemoryRegion[b.memoryRegion], b)
				if b.memoryRegion.Checkpoint() != nil {
					// The memory region is sealed: a publication is reading its pages
					// while this eviction punches one of them. Nothing may lose
					// bytes here, and nothing reaches it without arena pressure
					// at exactly the wrong moment.
					sim.Probe(ctx, ProbeEvictionDuringPublication)
				}
				if !pg.private {
					continue
				}
				spill, elsewhere := b.spillTarget()
				if spill.none() {
					// A page sharing a checkpoint's copy owns no reservation of its own:
					// the checkpoint's copy is the alias that spills those bytes, and so
					// does a machine that inherited the name a seal gave the page, whose
					// page is not its own state at all. Any other private page without one
					// would lose them here.
					if !elsewhere {
						return errors.New("private page has no spill reservation")
					}
					continue
				}
				if taken[spill] {
					continue
				}
				taken[spill] = true
				pages = append(pages, spillPage{spill, pg})
			}
		}
	}
	for r, bindings := range byMemoryRegion {
		if err := r.revokeBindings(ctx, bindings); err != nil {
			// A memory region this page is also reachable from cannot take the mapping
			// away, which is what a machine whose memory session has stopped
			// answering looks like from here. The page therefore stays mapped
			// there and is not this host's to reuse — but that is a fact about
			// that memory region, which the failed revocation has just made terminal,
			// and not about whoever is evicting. A fan-out's children share
			// every page they inherited, so returning this to the caller ends
			// one machine for another machine's death and then the next for
			// that one's. The caller takes another victim instead; this page
			// is excluded from every later pass by the memory region it could not be
			// taken from.
			r.heldPages(ctx, err)
			return errors.Join(errVictimHeld, err)
		}
	}
	// The storage writes each run of consecutive reservations in one write, so
	// the pages go to it in the order of their reservations.
	sort.Slice(pages, func(i, j int) bool { return pages[i].spill.ref.Value() < pages[j].spill.ref.Value() })
	if len(pages) > 0 {
		ps := int(h.pageSize)
		data := make([]byte, len(pages)*ps)
		refs := make([]zirconvm.ReferenceValue, len(pages))
		for i, page := range pages {
			if err := page.resident.file.Read(ctx, page.resident.slot, data[i*ps:(i+1)*ps]); err != nil {
				return err
			}
			refs[i] = page.spill.ref
		}
		writes, err := h.spill.StoreReserved(ctx, refs, data)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.stats.SpillWrites += uint64(writes)
		h.stats.SpillWriteBytes += uint64(len(data))
		h.stats.Spills += uint64(len(pages))
		h.mu.Unlock()
	}
	for _, pg := range victims {
		if err := h.release(ctx, pg); err != nil {
			return err
		}
		h.mu.Lock()
		h.stats.Evictions++
		if pg.aliases.len() > 0 {
			h.displaced++
		}
		unaliasAllLocked(pg)
		h.mu.Unlock()
	}
	return nil
}
