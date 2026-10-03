//go:build !linux

package framer_test

import "testing"

// Off Linux there is no sendfile with an offset of its own, so a file range is
// read and copied a chunk at a time: twelve chunks of 256 KiB for 3 MiB.
func TestAFileRangeOverTCPIsCopiedWhereTheKernelCannotSendIt(t *testing.T) {
	t.Parallel()
	if reads := sendFileRange(t, 3<<20, 4096); reads != 12 {
		t.Fatalf("sending a file range read the file %d times, want 12", reads)
	}
}
