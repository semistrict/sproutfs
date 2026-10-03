package sim

import (
	"runtime"
	"sync"
)

// shaker is Config.Shake: a stream of how many times a goroutine yields
// before it goes on.
type shaker struct {
	mu    sync.Mutex
	state uint64
}

// shakeYields bounds the yields of one shake. A handful is enough to let any
// other goroutine ready at the same instant overtake the one that yields.
const shakeYields = 8

// yield gives the processor up a drawn number of times. A nil shaker yields
// nothing.
func (s *shaker) yield() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.state = mix64(s.state)
	n := s.state % shakeYields
	s.mu.Unlock()
	for range n {
		runtime.Gosched()
	}
}
