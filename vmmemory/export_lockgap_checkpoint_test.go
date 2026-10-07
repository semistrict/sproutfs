package vmmemory

import "testing"

// SetEndForkFileSeam installs what the end of a seal runs between taking its
// fork file's holders and dropping the file from their processes. It is
// restored when the test ends.
func SetEndForkFileSeam(t *testing.T, seam func()) {
	previous := endForkFileSeam
	endForkFileSeam = seam
	t.Cleanup(func() { endForkFileSeam = previous })
}

// SetReadSpillSeam installs what a read of a spilled checkpoint copy runs
// between taking its reservation and reading it. It is restored when the
// test ends.
func SetReadSpillSeam(t *testing.T, seam func()) {
	previous := readSpillSeam
	readSpillSeam = seam
	t.Cleanup(func() { readSpillSeam = previous })
}
