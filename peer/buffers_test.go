package peer

import (
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// Every test of this package, its external tests too, runs with released
// buffers poisoned, so a test that holds bytes past their release fails every
// run rather than when the pool happens to reuse the buffer.
func init() { poisonReleased = true }

// A payload buffer is exactly the length asked for, in the size class above it,
// and a released one is what the next request of its class gets.
func TestAPayloadBufferIsItsLengthInItsClass(t *testing.T) {
	for _, test := range []struct{ size, capacity int }{
		{0, 4 << 10}, {1, 4 << 10}, {4 << 10, 4 << 10}, {4<<10 + 1, 8 << 10},
		{2<<20 + 64, 4 << 20}, {platform.MaxFrameBytes, platform.MaxFrameBytes},
	} {
		buffer := takeBuffer(test.size)
		if len(buffer.bytes) != test.size || cap(buffer.bytes) != test.capacity {
			t.Fatalf("a buffer of %d bytes has length %d and capacity %d, want %d and %d",
				test.size, len(buffer.bytes), cap(buffer.bytes), test.size, test.capacity)
		}
		buffer.release()
	}
	oversized := takeBuffer(platform.MaxFrameBytes + 1)
	if len(oversized.bytes) != platform.MaxFrameBytes+1 || oversized.class != -1 {
		t.Fatalf("a buffer past the largest frame is %d bytes in class %d, want %d bytes unpooled",
			len(oversized.bytes), oversized.class, platform.MaxFrameBytes+1)
	}
	oversized.release()
}
