package ctxsync

import (
	"context"
	"sync"
)

// LazyMutex is a context-aware mutual exclusion lock whose zero value is
// unlocked. It allocates nothing until a Lock has to wait, so it can be a
// field of a value there are millions of, where a Mutex is two allocations
// each. Waiting happens on a channel the first waiter makes, which
// testing/synctest recognizes as a durable blocking point. Unlike a Mutex, an
// Unlock wakes every waiter and the first to look takes the lock, so a
// TryLock can take it before a waiter does. It must not be copied after first
// use.
type LazyMutex struct {
	_  noCopy
	mu sync.Mutex
	// locked is whether the lock is held, and changed is closed by the next
	// Unlock, nil while nothing waits. Both are guarded by mu.
	locked  bool
	changed chan struct{}
}

// Lock acquires m or returns the cancellation cause of ctx. If ctx is already
// canceled, Lock never acquires m.
func (m *LazyMutex) Lock(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for {
		m.mu.Lock()
		if !m.locked {
			m.locked = true
			m.mu.Unlock()
			return nil
		}
		if err := m.wait(ctx); err != nil {
			return err
		}
	}
}

// WaitFree returns once m is unlocked, without taking it, or with the
// cancellation cause of ctx. It may return after an Unlock that another
// caller's Lock or TryLock has already followed. It is for a caller that
// takes m by TryLock when it chooses, rather than as whichever of the waiters
// an Unlock wakes runs first.
func (m *LazyMutex) WaitFree(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	if !m.locked {
		m.mu.Unlock()
		return nil
	}
	return m.wait(ctx)
}

// wait blocks until the next Unlock or the end of ctx. It must be called with
// mu held and returns with mu released.
func (m *LazyMutex) wait(ctx context.Context) error {
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	changed := m.changed
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-changed:
		return nil
	}
}

// TryLock acquires m without waiting and reports whether it succeeded.
func (m *LazyMutex) TryLock() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return false
	}
	m.locked = true
	return true
}

func (m *LazyMutex) Unlock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.locked {
		panic("ctxsync: unlock of unlocked LazyMutex")
	}
	m.locked = false
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
}
