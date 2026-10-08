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

// SetEvictedSeam installs what an eviction runs once it has taken every alias
// off its page and before it gives the page's slot back, so a test can put a
// fault of that page there. It is restored when the test ends.
func SetEvictedSeam(t *testing.T, seam func(slot int)) {
	previous := evictedSeam
	evictedSeam = seam
	t.Cleanup(func() { evictedSeam = previous })
}

// AgeEveryPage ages the page queues by as many generations as they hold, as
// an eviction's look for a victim does when it finds none old enough: every
// page is then old enough to give up, the ones a fault just touched too.
func (h *Host) AgeEveryPage() {
	for range zirconvm.NumReclaim {
		h.node.PageQueues().RotateReclaimQueues()
	}
}

// SetLocateSeam installs what a region's resolver runs once it has found the
// root holding a page and let the root's lock go, before the lookup goes down
// into the root. It is restored when the test ends.
func SetLocateSeam(t *testing.T, seam func(page uint64)) {
	previous := locateSeam
	locateSeam = seam
	t.Cleanup(func() { locateSeam = previous })
}

// SetLookupSentSeam installs what a lookup that sent a READ request runs
// before the request is looked at under h.mu. It is restored when the test
// ends.
func SetLookupSentSeam(t *testing.T, seam func(page uint64)) {
	previous := lookupSentSeam
	lookupSentSeam = seam
	t.Cleanup(func() { lookupSentSeam = previous })
}

// SetPrefetchCheckedSeam installs what a prefetch runs with h.mu held, once it
// has looked for the reads under way and before it sends its own. It is
// restored when the test ends.
func SetPrefetchCheckedSeam(t *testing.T, seam func(start uint64)) {
	previous := prefetchCheckedSeam
	prefetchCheckedSeam = seam
	t.Cleanup(func() { prefetchCheckedSeam = previous })
}

// SetLoadUnboundSeam installs what a fault runs once it found its page bound
// and not mapped and gave the page's lock back, before it looks the page up.
// It is restored when the test ends.
func SetLoadUnboundSeam(t *testing.T, seam func(index uint64)) {
	previous := loadUnboundSeam
	loadUnboundSeam = seam
	t.Cleanup(func() { loadUnboundSeam = previous })
}
