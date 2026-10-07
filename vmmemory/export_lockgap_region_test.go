package vmmemory

import (
	"context"
	"errors"
)

// WaitOnPrefetchOf waits, as a fault that waits for its own prefetch does, on
// the requests of r's prefetch of the window that begins at start. It reports
// whether it found one under way to wait on.
func WaitOnPrefetchOf(ctx context.Context, r *MemoryRegion, start uint64) (bool, error) {
	h := r.host
	h.mu.Lock()
	var found *prefetch
	for pf := range h.prefetches {
		if pf.region == r && pf.start == start {
			found = pf
		}
	}
	h.mu.Unlock()
	if found == nil {
		return false, errors.New("vmmemory: no prefetch of the window is holding its slots")
	}
	w := found.waiter()
	if w == nil {
		return false, nil
	}
	return true, w.wait(ctx)
}
