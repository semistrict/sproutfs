package vmmemory

import (
	"context"
	"testing"
)

// SetReadInSeam runs seam in every store's read of the page it copies, just
// before the lookup, until the test ends.
func SetReadInSeam(t *testing.T, seam func(index uint64)) {
	previous := readInSeam
	readInSeam = seam
	t.Cleanup(func() { readInSeam = previous })
}

// HoldPage takes the lock of the page r maps at index, as an eviction or a
// fault of r holds it, and reports how to let it go.
func HoldPage(ctx context.Context, r *MemoryRegion, index uint64) (func(), error) {
	page, err := r.host.lockedPage(ctx, r.lookupBinding(index))
	if err != nil {
		return nil, err
	}
	return func() { r.host.unlockPage(page) }, nil
}
