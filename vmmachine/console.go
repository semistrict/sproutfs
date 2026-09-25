package vmmachine

import "sync"

// maxConsoleBytes is how much of one VMM's console output a process retains.
// Console output is disposable diagnostics: it lives in this process's memory
// only, nothing about it survives the process, and the guest is never blocked
// by it.
const maxConsoleBytes = 1 << 20

// consoleRing retains the newest maxConsoleBytes of a VMM's console output.
// Offsets are positions in the whole output, so a reader's offset keeps its
// meaning after the bytes it named have fallen out of the ring; a read from
// before the ring's start is answered from the oldest byte still retained and
// reports the offset it actually starts at.
type consoleRing struct {
	mu   sync.Mutex
	data []byte
	// next indexes the byte the next write lands on, and is the oldest
	// retained byte once the ring has wrapped.
	next int
	full bool
	// end is the offset just past the newest byte ever written.
	end int64
}

func newConsoleRing() *consoleRing { return &consoleRing{data: make([]byte, maxConsoleBytes)} }

// Write is the VMM's stdout and stderr. It never fails and never blocks: output
// past the ring's capacity displaces the oldest bytes.
func (c *consoleRing) Write(data []byte) (int, error) {
	written := len(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) > len(c.data) {
		data = data[len(data)-len(c.data):]
	}
	for len(data) != 0 {
		n := copy(c.data[c.next:], data)
		data = data[n:]
		c.next += n
		if c.next == len(c.data) {
			c.next, c.full = 0, true
		}
	}
	c.end += int64(written)
	return written, nil
}

// start is the offset of the oldest retained byte. The caller holds c.mu.
func (c *consoleRing) start() int64 {
	if c.full {
		return c.end - int64(len(c.data))
	}
	return c.end - int64(c.next)
}

// read copies up to limit bytes of retained output beginning at offset, or at
// the oldest byte still retained when offset names output already discarded.
// It reports the offset the returned bytes begin at, which a reader compares
// with the offset it asked for to see that output was dropped, and the offset
// to ask for next.
func (c *consoleRing) read(offset int64, limit int) (data []byte, from, next int64) {
	if offset < 0 || limit < 0 {
		return nil, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := c.start()
	from = min(max(offset, start), c.end)
	length := int(min(c.end-from, int64(limit)))
	data = make([]byte, length)
	oldest := 0
	if c.full {
		oldest = c.next
	}
	begin := (oldest + int(from-start)) % len(c.data)
	n := copy(data, c.data[begin:])
	copy(data[n:], c.data)
	return data, from, from + int64(length)
}

// tail is the newest retained output, for reporting what a VMM printed before
// it died.
func (c *consoleRing) tail(limit int) []byte {
	c.mu.Lock()
	end := c.end
	c.mu.Unlock()
	data, _, _ := c.read(max(0, end-int64(limit)), limit)
	return data
}
