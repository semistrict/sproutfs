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

// WaitReadable returns once a reader may take m, without taking it, or with
// the cancellation cause of ctx. It may return after a change that another
// caller has already followed, so a reader then takes m by TryRLock, when it
// chooses, rather than as whichever of the waiters a change wakes runs first.
func (m *RWMutex) WaitReadable(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	if !m.writer && m.waiting == 0 {
		m.mu.Unlock()
		return nil
	}
	return m.wait(ctx)
}

// Writer is a writer waiting for an RWMutex, which takes it when it chooses
// rather than as soon as it comes free: from Writer until it takes the lock
// or leaves, new readers wait behind it, as they wait behind Lock. Nothing
// changes when it wakes, so the goroutine that freed the lock goes on beside
// it on the state it left.
type Writer struct {
	m    *RWMutex
	done bool
}

// Writer makes a writer waiting for m.
func (m *RWMutex) Writer() *Writer {
	m.mu.Lock()
	m.waiting++
	m.mu.Unlock()
	return &Writer{m: m}
}

// Wait returns once m is free, without taking it, or with the cancellation
// cause of ctx. Another writer may take m before w does.
func (w *Writer) Wait(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m := w.m
	for {
		m.mu.Lock()
		if !m.writer && m.readers == 0 {
			m.mu.Unlock()
			return nil
		}
		if err := m.wait(ctx); err != nil {
			return err
		}
	}
}

// TryLock takes m where it is free and reports whether it did. Once it has,
// w waits no more.
func (w *Writer) TryLock() bool {
	m := w.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.done || m.writer || m.readers > 0 {
		return false
	}
	m.writer, w.done = true, true
	m.waiting--
	return true
}

// Leave stops w waiting where it has not taken m, which lets the readers
// held behind it go on.
func (w *Writer) Leave() {
	m := w.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.done {
		return
	}
	w.done = true
	m.waiting--
	m.signal()
}

// TryRLock acquires a shared lock without waiting and reports whether it did.
// Like RLock, it defers to a writer that is waiting.
func (m *RWMutex) TryRLock() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writer || m.waiting > 0 {
		return false
	}
	m.readers++
	return true
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

// TryLock acquires the exclusive lock without waiting and reports whether it
// did.
func (m *RWMutex) TryLock() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writer || m.readers > 0 {
		return false
	}
	m.writer = true
	return true
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
