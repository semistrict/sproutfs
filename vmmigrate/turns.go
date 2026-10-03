package vmmigrate

import (
	"context"
	"sync"
)

// turns puts the faults of one memory region's post-copy stream in page order
// up to the moment each one's read is under way.
//
// The stream keeps several faults in flight, so that the link to the source
// never waits on a round trip. But everything a fault decides before its read
// is issued is visible to everything after it: the arena slot it takes, the
// page it evicts for that slot, the draws of the fault-injection sites on
// that path, and where its request sits on the link, which decides when its
// reply arrives and so where the page enters the pager's recency order. When
// the faults made those decisions at once, the Go scheduler chose their order,
// and a seed could not reproduce a run. So each fault takes a turn in the order
// the stream reached its page, and gives it up once its read has been issued:
// its first request is on the wire, or its read of the volume has begun. The
// reads and the replies still overlap as much as the backing allows.
type turns struct {
	mu sync.Mutex
	// next is the lowest turn not given up, and given holds the turns above
	// it that were given up first.
	next  uint64
	given map[uint64]bool
	// moved is closed whenever next moves.
	moved chan struct{}
}

func newTurns() *turns {
	return &turns{given: make(map[uint64]bool), moved: make(chan struct{})}
}

// take waits until every turn below turn has been given up, and returns the
// function that gives turn up. The caller calls it once the fault is over,
// whatever happened; giving a turn up again does nothing. A take that ends
// with ctx's cancellation gives its turn up at once, so the turns after it are
// not held by a fault that never ran.
func (t *turns) take(ctx context.Context, turn uint64) (func(), error) {
	give := sync.OnceFunc(func() { t.giveUp(turn) })
	for {
		t.mu.Lock()
		reached, moved := t.next == turn, t.moved
		t.mu.Unlock()
		if reached {
			return give, nil
		}
		select {
		case <-ctx.Done():
			give()
			return give, context.Cause(ctx)
		case <-moved:
		}
	}
}

// giveUp records turn as given up and moves next past every turn that has
// been.
func (t *turns) giveUp(turn uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.given[turn] = true
	if !t.given[t.next] {
		return
	}
	for t.given[t.next] {
		delete(t.given, t.next)
		t.next++
	}
	close(t.moved)
	t.moved = make(chan struct{})
}

type turnKey struct{}

// withTurn hands the backing a fault's turn, which it gives up once the
// fault's read has been issued.
func withTurn(ctx context.Context, give func()) context.Context {
	return context.WithValue(ctx, turnKey{}, give)
}

// turnOf is the turn a fault under ctx holds, as the function that gives it
// up, or nil where it holds none: a guest's own fault.
func turnOf(ctx context.Context) func() {
	give, _ := ctx.Value(turnKey{}).(func())
	return give
}
