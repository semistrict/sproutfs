package vmmemory

import (
	"fmt"
	"testing"
)

// SetCheckpointBatchPages bounds the pages one seal or retire transition holds
// the memory region for, so a test can observe a batch boundary without a dirty set of
// production size. It is restored when the test ends.
func SetCheckpointBatchPages(t *testing.T, pages int) {
	previous := checkpointBatchPages
	checkpointBatchPages = pages
	t.Cleanup(func() { checkpointBatchPages = previous })
}

// SetEvictionSeam installs what a reclaim runs between reading a victim's
// aliases and reading their reservations, so a test can take a seal in the one
// moment the two transitions can be interleaved. It is restored when the test
// ends.
func SetEvictionSeam(t *testing.T, seam func(slot int)) {
	previous := evictionSeam
	evictionSeam = seam
	t.Cleanup(func() { evictionSeam = previous })
}

// SetReclaimSeam installs what a reclaim for a private page runs while the
// memory region is given up, so a test can end that page's dirty epoch in the one
// window a fault serving it cannot see. It is restored when the test ends.
func SetReclaimSeam(t *testing.T, seam func(index uint64)) {
	previous := reclaimSeam
	reclaimSeam = seam
	t.Cleanup(func() { reclaimSeam = previous })
}

// SetSealSeam installs what a seal runs between write-protecting the dirty set
// and recording the checkpoint on the memory region. It is restored when the test ends.
func SetSealSeam(t *testing.T, seam func()) {
	previous := sealSeam
	sealSeam = seam
	t.Cleanup(func() { sealSeam = previous })
}

// SetPopulationRuns bounds the mapping runs one attach installs, so a test can
// observe the bound without a memory region of production size. It is restored when
// the test ends.
func SetPopulationRuns(t *testing.T, runs int) {
	previous := populationRuns
	populationRuns = runs
	t.Cleanup(func() { populationRuns = previous })
}

// SetPopulationWindowBytes bounds the window one populate walks volume metadata
// in, so a test can cross a window boundary without a memory region of production size.
// It is restored when the test ends.
func SetPopulationWindowBytes(t *testing.T, bytes uint64) {
	previous := populationWindowBytes
	populationWindowBytes = bytes
	t.Cleanup(func() { populationWindowBytes = previous })
}

// SetSealWalkSeam installs what the walk behind a seal's pause runs before it
// takes its first page, so a test can hold the walk there and look at what the
// pause itself cost. It is restored when the test ends.
func SetSealWalkSeam(t *testing.T, seam func()) {
	previous := sealWalkSeam
	sealWalkSeam = seam
	t.Cleanup(func() { sealWalkSeam = previous })
}

// Signal wakes every store waiting on the host, as any change to a page does.
func Signal(h *Host) {
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
}

// SetPopulationPages bounds the pages one attach installs, so a test can
// observe the bound without a memory region of production size. It is restored when
// the test ends.
func SetPopulationPages(t *testing.T, pages uint64) {
	previous := populationPages
	populationPages = pages
	t.Cleanup(func() { populationPages = previous })
}

// PressMappings is what a memory region's first refused mapping command does: from
// then on its stores close gaps. The rules' own tests start there.
func (r *MemoryRegion) PressMappings() { r.pressed.Store(true) }

// Unreachable describes every resident page that no memory region maps and
// that is not idle either: memory nothing will ever give back. A host whose
// memory regions have all released what they held has none. It also reports a
// slot two pages claim, and a recency list whose length is not the slots held.
func (h *Host) Unreachable() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []string
	claimed := map[fileSlot]bool{}
	listed := 0
	for pg := h.lru.front(); pg != nil; pg = h.lru.next(pg) {
		listed++
		if claimed[pg.fileSlot] {
			found = append(found, fmt.Sprintf("slot %d is claimed twice", pg.slot))
		}
		claimed[pg.fileSlot] = true
		if pg.aliases.len() > 0 || h.idle.contains(pg) {
			continue
		}
		found = append(found, fmt.Sprintf("slot %d key %+v private %t replacing %d dropped %t indexed %t free %t",
			pg.slot, pg.key.id, pg.private, pg.replacing, pg.dropped, h.clean[pg.key] == pg, pg.slot >= 0 && pg.file.slots.IsFree(pg.slot)))
	}
	for _, f := range h.files {
		for slot, entry := range f.leases {
			if !claimed[fileSlot{f, slot}] {
				found = append(found, fmt.Sprintf("slot %d is held for no page, in an extent %t", slot, entry.extent != nil))
			}
		}
	}
	if held := h.heldLocked(); listed != held {
		found = append(found, fmt.Sprintf("%d pages are listed and %d slots held", listed, held))
	}
	return found
}
