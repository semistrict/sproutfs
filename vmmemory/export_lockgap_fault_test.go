package vmmemory

import (
	"testing"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// SetAllocateOwnSeam installs what a reclaim of a page's own place runs once
// it has chosen the idle page it will give up and before it takes that page's
// lock. It is restored when the test ends.
func SetAllocateOwnSeam(t *testing.T, seam func(index uint64)) {
	previous := allocateOwnSeam
	allocateOwnSeam = seam
	t.Cleanup(func() { allocateOwnSeam = previous })
}

// AgeEveryPage ages the page queues by as many generations as they hold, as
// an eviction's look for a victim does when it finds none old enough: every
// page is then old enough to give up, the ones a fault just touched too.
func (h *Host) AgeEveryPage() {
	for range zirconvm.NumReclaim {
		h.node.PageQueues().RotateReclaimQueues()
	}
}
