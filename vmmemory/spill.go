package vmmemory

import (
	"context"
	"errors"

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
