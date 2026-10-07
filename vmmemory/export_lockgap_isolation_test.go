package vmmemory

import "testing"

// SetForkFileSeam installs what a fork copy runs between its look at the
// point and its making the point's file, given the page it copies.
func SetForkFileSeam(t *testing.T, seam func(page uint64)) {
	previous := forkFileSeam
	forkFileSeam = seam
	t.Cleanup(func() { forkFileSeam = previous })
}

// SetForkCopySeam installs what a fork copy runs between its making the copy
// and the point's keeping it, given the page it copies.
func SetForkCopySeam(t *testing.T, seam func(page uint64)) {
	previous := forkCopySeam
	forkCopySeam = seam
	t.Cleanup(func() { forkCopySeam = previous })
}

// SetUnindexSeam installs what an unindex runs between its taking a page from
// its root and its look at the owner's mapping of it.
func SetUnindexSeam(t *testing.T, seam func(page uint64)) {
	previous := unindexSeam
	unindexSeam = seam
	t.Cleanup(func() { unindexSeam = previous })
}
