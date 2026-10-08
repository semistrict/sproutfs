package vmmemory

import (
	"fmt"
	"testing"
	"time"
)

// Layered reports whether this memory region's pages are served by the
// ported region layer, which every memory region must be.
func (r *MemoryRegion) Layered() bool { return r.layer != nil }

// ColdCopyAge is how old a cold copy is before its session gives it back and
// before an eviction may.
func ColdCopyAge() time.Duration { return coldCopyAge }

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

// SetPrefetchSettleSeam installs what a prefetch runs once its read is over
// and before its slots are given back or filled, so a test can put an
// allocation in that moment. It is restored when the test ends.
func SetPrefetchSettleSeam(t *testing.T, seam func()) {
	previous := prefetchSettleSeam
	prefetchSettleSeam = seam
	t.Cleanup(func() { prefetchSettleSeam = previous })
}

// SetPrefetchUnlockSeam installs what a prefetch that landed runs as it gives
// each of its pages up, once that page is given up and before the next is. It
// is restored when the test ends.
func SetPrefetchUnlockSeam(t *testing.T, seam func(page uint64)) {
	previous := prefetchUnlockSeam
	prefetchUnlockSeam = seam
	t.Cleanup(func() { prefetchUnlockSeam = previous })
}

// SetPrefetchSendSeam installs what a fault runs as it splits a prefetch off
// the window that begins at start, before it looks for reads under way and
// sends its own. It is restored when the test ends.
func SetPrefetchSendSeam(t *testing.T, seam func(start uint64)) {
	previous := prefetchSendSeam
	prefetchSendSeam = seam
	t.Cleanup(func() { prefetchSendSeam = previous })
}

// SetAllocateSeam installs what an allocation runs between its look for a
// free slot and its eviction step. It is restored when the test ends.
func SetAllocateSeam(t *testing.T, seam func()) {
	previous := allocateSeam
	allocateSeam = seam
	t.Cleanup(func() { allocateSeam = previous })
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

// HoldHostLock takes the host lock and reports what gives it back, so a test can
// hold it the way a store applying the rules does while it reads a binding.
func HoldHostLock(h *Host) (release func()) {
	h.mu.Lock()
	return h.mu.Unlock
}

// BindingsHeld reports whether something holds a memory region's binding map
// lock at this moment.
func BindingsHeld(r *MemoryRegion) bool {
	if r.bindingsMu.TryLock() {
		r.bindingsMu.Unlock()
		return false
	}
	return true
}

// RepeatBurst and RepeatInterval are each session's budget of repeated faults.
const (
	RepeatBurst    = repeatBurst
	RepeatInterval = repeatInterval
)

// Repeated reports whether a fault on page for this access would be a repeated
// fault, which the Linux transport paces.
func Repeated(r *MemoryRegion, page uint64, write bool) bool { return r.repeated(page, write) }

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
// slot two pages claim, and page queues that hold other than the slots held.
func (h *Host) Unreachable() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []string
	claimed := map[fileSlot]bool{}
	listed := 0
	for p := range h.node.PageQueues().Pages() {
		if isLent(p) {
			continue
		}
		f := frameOf(p)
		listed++
		if claimed[f.fileSlot] {
			found = append(found, fmt.Sprintf("slot %d is claimed twice", f.slot))
		}
		claimed[f.fileSlot] = true
		if f.aliases.len() > 0 || f.idle {
			continue
		}
		found = append(found, fmt.Sprintf("slot %d in a layer %t replacing %d free %t",
			f.slot, f.layer != nil, f.replacing, f.slot >= 0 && f.file.slots.IsFree(f.slot)))
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

// The package's tests check every mapping command against what the pager
// has installed (mappingaudit.go).
func init() { auditMappings = true }

// Failed is why a memory region is terminal, nil while it is not.
func Failed(r *MemoryRegion) error {
	if failed := r.terminal.Load(); failed != nil {
		return failed.err
	}
	return nil
}

// WithoutMappingAudit attaches the test's memory regions with no mapping
// audit, for a test that measures what the pager itself costs. It is
// restored when the test ends.
func WithoutMappingAudit(t *testing.T) {
	auditMappings = false
	t.Cleanup(func() { auditMappings = true })
}
