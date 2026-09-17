package ctxsync

import (
	"context"
	"sync"
)

// RWMutex is a context-aware reader/writer lock. Writers are preferred: once a
// writer waits, new readers wait behind it, so a stream of readers cannot
// starve it. Waiting happens on channels, which testing/synctest recognizes as
// durable blocking points. Construct it with NewRWMutex; do not copy it.
type RWMutex struct {
	_       noCopy
	mu      sync.Mutex
	readers int
	writer  bool
	waiting int
	changed chan struct{}
}

func NewRWMutex() *RWMutex { return &RWMutex{changed: make(chan struct{})} }

// wait blocks until the lock state changes or ctx is canceled. It must be
// called with mu held and returns with mu released.
func (m *RWMutex) wait(ctx context.Context) error {
	changed := m.changed
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-changed:
		return nil
	}
}

func (m *RWMutex) signal() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// RLock acquires a shared lock or returns the cancellation cause of ctx.
func (m *RWMutex) RLock(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for {
		m.mu.Lock()
		if !m.writer && m.waiting == 0 {
			m.readers++
			m.mu.Unlock()
			return nil
		}
		if err := m.wait(ctx); err != nil {
			return err
		}
	}
}

func (m *RWMutex) RUnlock() {
	m.mu.Lock()
	if m.readers == 0 {
		panic("ctxsync: RUnlock of unlocked RWMutex")
	}
	m.readers--
	m.signal()
	m.mu.Unlock()
}

// Lock acquires the exclusive lock or returns the cancellation cause of ctx.
func (m *RWMutex) Lock(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	registered := false
	for {
		m.mu.Lock()
		if !m.writer && m.readers == 0 {
			if registered {
				m.waiting--
			}
			m.writer = true
			m.mu.Unlock()
			return nil
		}
		if !registered {
			registered = true
			m.waiting++
		}
		if err := m.wait(ctx); err != nil {
			m.mu.Lock()
			m.waiting--
			m.signal()
			m.mu.Unlock()
			return err
		}
	}
}

func (m *RWMutex) Unlock() {
	m.mu.Lock()
	if !m.writer {
		panic("ctxsync: Unlock of unlocked RWMutex")
	}
	m.writer = false
	m.signal()
	m.mu.Unlock()
}
