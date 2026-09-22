package vmmemory

import "testing"

// SetCheckpointBatchPages bounds the pages one seal or retire transition holds
// the region for, so a test can observe a batch boundary without a dirty set of
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
// region is given up, so a test can end that page's dirty epoch in the one
// window a fault serving it cannot see. It is restored when the test ends.
func SetReclaimSeam(t *testing.T, seam func(index uint64)) {
	previous := reclaimSeam
	reclaimSeam = seam
	t.Cleanup(func() { reclaimSeam = previous })
}

// SetSealSeam installs what a seal runs between write-protecting the dirty set
// and recording the checkpoint on the region. It is restored when the test ends.
func SetSealSeam(t *testing.T, seam func()) {
	previous := sealSeam
	sealSeam = seam
	t.Cleanup(func() { sealSeam = previous })
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
