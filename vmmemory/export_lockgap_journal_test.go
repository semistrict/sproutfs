package vmmemory

import "testing"

// SetCaptureSeam runs seam in every capture between its taking the pages and
// its holding the region's protection, until the test ends.
func SetCaptureSeam(t *testing.T, seam func(taken []uint64)) {
	previous := captureSeam
	captureSeam = seam
	t.Cleanup(func() { captureSeam = previous })
}

// SetSettleComparedSeam installs what a settle runs once it has compared its
// copies and before it reshares them. It is restored when the test ends.
func SetSettleComparedSeam(t *testing.T, seam func()) {
	previous := settleComparedSeam
	settleComparedSeam = seam
	t.Cleanup(func() { settleComparedSeam = previous })
}
