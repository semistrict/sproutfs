package vmmemory

import (
	"context"
	"errors"
	"hash/crc32"
	"io"
	"sort"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// spillHolds reports whether a reservation's slot holds its page's bytes. It is
// the slot's state, not the binding's: an eviction publishes those bytes while
// a seal may be handing the reservation to the checkpoint's copy of the page,
// and only the slot is named by both.
func (h *Host) spillHolds(slot int) bool {
	if slot < 0 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spillWritten[slot]
}

// spillChecksums is what every spilled page is checked against when it comes
// back. The spill file is scratch on a local device, so the authority for what
// it should hold lives in this process and not in the file: a page that comes
// back short, zeroed or holding bytes nobody wrote is a page the device lost,
// and handing it to the guest would be handing the guest silently wrong memory.
var spillChecksums = crc32.MakeTable(crc32.Castagnoli)

// spillDigest reports the checksum a slot's bytes must have, and whether the
// slot holds any.
func (h *Host) spillDigest(slot int) (uint32, bool) {
	if slot < 0 {
		return 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spillSum[slot], h.spillWritten[slot]
}

// ErrSpillCorrupt reports a spilled page whose bytes are not the ones that were
// written to its reservation.
var ErrSpillCorrupt = errors.New("vmmemory: spilled page does not match its checksum")

// releaseSpill returns a dirty page's spill slot to the host and wakes waiters.
func (h *Host) releaseSpill(slot int) {
	h.mu.Lock()
	written := h.spillWritten[slot]
	h.mu.Unlock()
	if written {
		// Returning the slot's blocks is a courtesy to the node's filesystem,
		// not accounting: the spill file's whole extent is already this pager's
		// fixed cap, so a failed or unsupported punch costs nothing.
		if file, ok := h.spill.(platform.SparseFile); ok {
			_ = file.PunchHole(context.Background(), int64(slot)*int64(PageSize), int64(PageSize))
		}
	}
	h.mu.Lock()
	h.spillWritten[slot] = false
	h.freeSpill = append(h.freeSpill, slot)
	h.dirty--
	if h.dirty < h.highWater {
		// Back under the mark: the next store to cross it asks again.
		h.asked = false
	}
	h.signal()
	h.mu.Unlock()
}

// tryTakeSpill admits one more private page to the dirty budget without
// waiting. A load that cannot have one fails rather than holding resident locks
// and an I/O permit while the budget frees up.
func (h *Host) tryTakeSpill() (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return 0, h.err
	}
	if n := len(h.freeSpill); n > 0 {
		slot := h.freeSpill[n-1]
		h.freeSpill = h.freeSpill[:n-1]
		h.dirty++
		h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
		return slot, nil
	}
	return 0, ErrCapacity
}

// takeFreeSpill admits up to want more private pages to the dirty budget
// without waiting: it takes only reservations that are free now, which is all
// write-ahead may use.
func (h *Host) takeFreeSpill(want int) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil || want <= 0 {
		return nil
	}
	slots := make([]int, 0, min(want, len(h.freeSpill)))
	for len(slots) < want && len(h.freeSpill) > 0 {
		n := len(h.freeSpill)
		slots = append(slots, h.freeSpill[n-1])
		h.freeSpill = h.freeSpill[:n-1]
	}
	h.dirty += len(slots)
	h.stats.PeakDirtyPages = max(h.stats.PeakDirtyPages, h.dirty)
	return slots
}

// evictionSeam runs in a reclaim between reading one victim's aliases and
// reading the reservations those aliases name, which is the one instant a seal
// can move a page's reservation to the checkpoint's copy of it without the
// reclaim seeing either state. Production leaves it nil; a test installs it to
// take a seal exactly there.
var evictionSeam func(slot int)

// All victims are already locked. Revoke every alias before reading its bytes.
// A scratch write provides live read-after-write backing; no Sync is needed
// because only the volume quorum acknowledges durability. No arena slot is
// released until the entire bounded scratch batch has succeeded.
func (h *Host) evictBatch(ctx context.Context, victims []*resident) error {
	// The reservation each frame's bytes go to is read once, here: a seal taken
	// while this runs hands a page's reservation to the checkpoint's copy of it
	// and joins that copy to the frame, so a reservation can be named by the
	// page before this walk and by the copy after it, and is written once
	// either way.
	type spillPage struct {
		slot     int
		resident *resident
	}
	var pages []spillPage
	taken := make(map[int]bool)
	byRegion := make(map[*Region][]*binding)
	for _, pg := range victims {
		// The seal joins the checkpoint's copy to the frame before it hands
		// that copy the page's reservation, so an alias set that has not grown
		// since its reservations were read names every reservation the frame's
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
				byRegion[b.region] = append(byRegion[b.region], b)
				if b.region.Checkpoint() != nil {
					// The region is sealed: a publication is reading its frames
					// while this eviction punches one of them. Nothing may lose
					// bytes here, and nothing reaches it without arena pressure
					// at exactly the wrong moment.
					sim.Probe(ctx, ProbeEvictionDuringPublication)
				}
				if !pg.private {
					continue
				}
				slot, elsewhere := b.spillTarget()
				if slot < 0 {
					// A page sharing a checkpoint's frame owns no reservation of its own:
					// the checkpoint's copy is the alias that spills those bytes, and so
					// does a machine that inherited the name a seal gave the frame, whose
					// page is not its own state at all. Any other private page without one
					// would lose them here.
					if !elsewhere {
						return errors.New("private page has no spill reservation")
					}
					continue
				}
				if taken[slot] {
					continue
				}
				taken[slot] = true
				pages = append(pages, spillPage{slot, pg})
			}
		}
	}
	for r, bindings := range byRegion {
		if err := r.revokeBindings(ctx, bindings); err != nil {
			// A region this frame is also reachable from cannot take the mapping
			// away, which is what a machine whose memory session has stopped
			// answering looks like from here. The frame therefore stays mapped
			// there and is not this host's to reuse — but that is a fact about
			// that region, which the failed revocation has just made terminal,
			// and not about whoever is evicting. A fan-out's children share
			// every frame they inherited, so returning this to the caller ends
			// one machine for another machine's death and then the next for
			// that one's. The caller takes another victim instead; this frame
			// is excluded from every later pass by the region it could not be
			// taken from.
			r.heldFrames(ctx, err)
			return errors.Join(errVictimHeld, err)
		}
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].slot < pages[j].slot })
	if len(pages) > 0 {
		data := make([]byte, len(pages)*PageSize)
		for i, page := range pages {
			if err := h.arena.Read(ctx, page.resident.slot, data[i*PageSize:(i+1)*PageSize]); err != nil {
				return err
			}
		}
		for start := 0; start < len(pages); {
			end := start + 1
			for end < len(pages) && pages[end].slot == pages[end-1].slot+1 {
				end++
			}
			bytes := data[start*PageSize : end*PageSize]
			h.mu.Lock()
			for i, page := range pages[start:end] {
				// The bytes are written to scratch and the slot is not recorded
				// as holding them, so a refault reads whatever the slot held
				// before instead of the guest's private page.
				h.spillWritten[page.slot] = !sim.Bug(ctx, "pager-forget-spill")
				h.spillSum[page.slot] = crc32.Checksum(bytes[i*PageSize:(i+1)*PageSize], spillChecksums)
			}
			h.mu.Unlock()
			n, err := h.spill.WriteAt(ctx, bytes, int64(pages[start].slot)*int64(PageSize))
			if err == nil && n != len(bytes) {
				err = io.ErrShortWrite
			}
			if err != nil {
				return err
			}
			h.mu.Lock()
			h.stats.SpillWrites++
			h.stats.SpillWriteBytes += uint64(len(bytes))
			h.mu.Unlock()
			start = end
		}
		h.mu.Lock()
		h.stats.Spills += uint64(len(pages))
		h.mu.Unlock()
	}
	for _, pg := range victims {
		if err := h.release(ctx, pg); err != nil {
			return err
		}
		h.mu.Lock()
		h.stats.Evictions++
		for b := range pg.aliases {
			b.resident = nil
		}
		clear(pg.aliases)
		h.mu.Unlock()
	}
	return nil
}
