package main

import "sync"

// buffers hands out byte slices by exact length, so the stripes and objects
// a run moves are reused rather than collected. A run has a few lengths.
type buffers struct {
	mu    sync.Mutex
	pools map[int]*sync.Pool
}

func (b *buffers) pool(n int) *sync.Pool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pools == nil {
		b.pools = make(map[int]*sync.Pool)
	}
	p, ok := b.pools[n]
	if !ok {
		p = &sync.Pool{New: func() any { return make([]byte, n) }}
		b.pools[n] = p
	}
	return p
}

func (b *buffers) get(n int) []byte { return b.pool(n).Get().([]byte) }

func (b *buffers) put(s []byte) {
	if s != nil {
		b.pool(len(s)).Put(s)
	}
}
