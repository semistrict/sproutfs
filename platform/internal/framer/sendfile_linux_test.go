package framer_test

import "testing"

// A file range sent over TCP goes to the kernel whole: no byte of the file is
// read into this process.
func TestAFileRangeOverTCPIsSentWithoutReadingTheFile(t *testing.T) {
	t.Parallel()
	if reads := sendFileRange(t, 3<<20, 4096); reads != 0 {
		t.Fatalf("sending a file range read the file %d times, want 0", reads)
	}
}
