package vmmemory

import "testing"

// SetCaptureSeam runs seam in every capture between its taking the pages and
// its holding the region's protection, until the test ends.
func SetCaptureSeam(t *testing.T, seam func(taken []uint64)) {
	previous := captureSeam
	captureSeam = seam
	t.Cleanup(func() { captureSeam = previous })
}
