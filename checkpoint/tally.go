package checkpoint

import (
	"context"
	"sync"
)

// tally counts work in flight, and lets a caller wait until none is. Its zero
// value counts none.
type tally struct {
	mu sync.Mutex
	n  int
	// none is closed and forgotten each time n falls to zero, for the
	// callers waiting then.
	none chan struct{}
}

// add counts change more pieces of work in flight, fewer when negative.
func (t *tally) add(change int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n += change
	if t.n < 0 {
		panic("checkpoint: a tally counted more work done than begun")
	}
	if t.n == 0 && t.none != nil {
		close(t.none)
		t.none = nil
	}
}

// wait returns once no work is in flight.
func (t *tally) wait(ctx context.Context) error {
	for {
		t.mu.Lock()
		if t.n == 0 {
			t.mu.Unlock()
			return nil
		}
		if t.none == nil {
			t.none = make(chan struct{})
		}
		none := t.none
		t.mu.Unlock()
		select {
		case <-none:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}
