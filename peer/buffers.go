package peer

import (
	"math/bits"
	"sync"

	"github.com/semistrict/sproutfs/platform"
)

// A payload is read into a buffer of exactly the length its frame states, taken
// from a pool of size classes: powers of two from 4 KiB to the largest frame. A
// reply of one 2 MiB page then costs no allocation once the pool is warm, and
// none of the doubling copies a reader that grows its buffer makes.
const (
	smallestBuffer = 12 // 4 KiB
	largestBuffer  = 24 // 16 MiB
)

var bufferClasses [largestBuffer - smallestBuffer + 1]sync.Pool

// poisonReleased fills a buffer with released as it goes back to the pool. This
// package's tests set it, so that bytes used after their buffer is released
// are wrong every time, not only when the pool next hands that buffer to a
// reader: a pool keeps a buffer per processor and drops it at a collection, so
// whether the next reply lands in the same buffer is the runtime's choice.
var poisonReleased bool

// released is what a poisoned buffer is filled with.
const released = 0xa5

// payloadBuffer is one pooled buffer, sliced to the length it was asked for.
type payloadBuffer struct {
	bytes []byte
	class int
}

// takeBuffer returns a buffer of size bytes. One larger than the largest frame
// is allocated and never pooled; no frame can need one.
func takeBuffer(size int) *payloadBuffer {
	class := max(bits.Len(uint(max(size, 1)-1)), smallestBuffer)
	if class > largestBuffer || size > platform.MaxFrameBytes {
		return &payloadBuffer{bytes: make([]byte, size), class: -1}
	}
	pool := &bufferClasses[class-smallestBuffer]
	if pooled, ok := pool.Get().(*payloadBuffer); ok {
		pooled.bytes = pooled.bytes[:size]
		return pooled
	}
	return &payloadBuffer{bytes: make([]byte, size, 1<<class), class: class}
}

// release gives the buffer back. Its bytes must not be used after.
func (b *payloadBuffer) release() {
	if b == nil || b.class < 0 {
		return
	}
	if poisonReleased {
		for i := range b.bytes {
			b.bytes[i] = released
		}
	}
	b.bytes = b.bytes[:0]
	bufferClasses[b.class-smallestBuffer].Put(b)
}
