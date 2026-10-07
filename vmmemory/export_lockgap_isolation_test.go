package vmmemory

import "testing"

// SetForkCopySeam installs what a fork copy runs between its making the copy
// and the copy's taking the lent page's place, given the page it copies.
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
