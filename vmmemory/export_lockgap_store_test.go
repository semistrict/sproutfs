package vmmemory

import (
	"context"
	"testing"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// SetRuleSeam installs what a rule runs once it has decided to take one page
// into a store's run and before it takes it, so a test can put another fault
// or an eviction of that page there. It is restored when the test ends.
func SetRuleSeam(t *testing.T, seam func(page uint64)) {
	previous := ruleSeam
	ruleSeam = seam
	t.Cleanup(func() { ruleSeam = previous })
}

// EvictPage evicts the page a memory region maps at index as the evictor's
// step does, waiting for that page's lock first. It reports whether the page
// went.
func EvictPage(ctx context.Context, r *MemoryRegion, index uint64) (bool, error) {
	h := r.host
	b := r.lookupBinding(index)
	if b == nil {
		return false, nil
	}
	page, err := h.lockedPage(ctx, b)
	if err != nil || page == nil {
		return false, err
	}
	h.mu.Lock()
	usable := h.usableVictimLocked(frameOf(page))
	h.mu.Unlock()
	if !usable {
		h.unlockPage(page)
		return false, nil
	}
	attempt, _, err := h.reclaimVictim(ctx, page)
	return attempt.Success.NumPages > 0, err
}

// HarvestPage harvests the page a memory region maps at index as the evictor's
// step does, waiting for that page's lock first: every mapping of it is taken
// away, and it stays. It reports whether the region bound a page there.
func HarvestPage(ctx context.Context, r *MemoryRegion, index uint64) (bool, error) {
	h := r.host
	b := r.lookupBinding(index)
	if b == nil {
		return false, nil
	}
	page, err := h.lockedPage(ctx, b)
	if err != nil || page == nil {
		return false, err
	}
	_, err = h.harvestPages(ctx, []*zirconvm.VmPage{page})
	return true, err
}
