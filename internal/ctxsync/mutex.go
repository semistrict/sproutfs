// Package ctxsync provides context-aware synchronization primitives built on
// channels. Channels created inside a testing/synctest bubble are recognized
// as durable blocking points, so the same primitives work in production and
// deterministic simulation.
package ctxsync

import "context"

// Mutex is a context-aware mutual exclusion lock. A Mutex must be constructed
// with NewMutex and must not be copied after first use.
type Mutex struct {
	_     noCopy
	token chan struct{}
}

func NewMutex() *Mutex {
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return &Mutex{token: token}
}

// Lock acquires m or returns the cancellation cause of ctx. If ctx is already
// canceled, Lock never acquires m.
func (m *Mutex) Lock(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-m.token:
		return nil
	}
}

// TryLock acquires m without waiting and reports whether it succeeded.
func (m *Mutex) TryLock() bool {
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}

func (m *Mutex) Unlock() {
	select {
	case m.token <- struct{}{}:
	default:
		panic("ctxsync: unlock of unlocked Mutex")
	}
}

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
