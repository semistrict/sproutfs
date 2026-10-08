package ctxsync_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
)

func TestRWMutexReadersShareAndWriterExcludes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		for range 3 {
			if err := m.RLock(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		acquired := make(chan time.Time, 1)
		go func() {
			if err := m.Lock(t.Context()); err != nil {
				t.Error(err)
				return
			}
			defer m.Unlock()
			acquired <- time.Now()
		}()
		start := time.Now()
		for i := range 3 {
			time.AfterFunc(time.Duration(i+1)*time.Second, m.RUnlock)
		}
		if got := <-acquired; got.Sub(start) != 3*time.Second {
			t.Fatalf("writer acquired after %v, want 3s (after the last reader)", got.Sub(start))
		}
	})
}

func TestRWMutexWaitingWriterBlocksNewReaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.RLock(t.Context()); err != nil {
			t.Fatal(err)
		}
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			if err := m.Lock(t.Context()); err != nil {
				t.Error(err)
				return
			}
			m.Unlock()
		}()
		synctest.Wait()
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			if err := m.RLock(t.Context()); err != nil {
				t.Error(err)
				return
			}
			m.RUnlock()
		}()
		synctest.Wait()
		select {
		case <-readerDone:
			t.Fatal("reader bypassed a waiting writer")
		default:
		}
		m.RUnlock()
		<-writerDone
		<-readerDone
	})
}

func TestRWMutexLockReturnsContextCauseAndReleasesWaitingSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.RLock(t.Context()); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() { done <- m.Lock(ctx) }()
		synctest.Wait()
		cancel(cause)
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("Lock error = %v, want cancellation cause", err)
		}
		// The canceled writer no longer holds new readers back.
		if err := m.RLock(t.Context()); err != nil {
			t.Fatal(err)
		}
		m.RUnlock()
		m.RUnlock()
	})
}

// TryRLock shares with readers and fails, without waiting, while a writer holds
// the lock or waits for it.
func TestRWMutexTryRLockNeverWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if !m.TryRLock() || !m.TryRLock() {
			t.Fatal("TryRLock of a lock only readers hold failed")
		}
		locked := make(chan struct{})
		go func() {
			if err := m.Lock(t.Context()); err != nil {
				t.Error(err)
				return
			}
			close(locked)
		}()
		synctest.Wait()
		if m.TryRLock() {
			t.Fatal("TryRLock succeeded while a writer waited")
		}
		m.RUnlock()
		m.RUnlock()
		<-locked
		if m.TryRLock() {
			t.Fatal("TryRLock succeeded while a writer held the lock")
		}
		m.Unlock()
		if !m.TryRLock() {
			t.Fatal("TryRLock of a free lock failed")
		}
		m.RUnlock()
	})
}

// WaitReadable waits while a writer holds the lock or waits for it, and takes
// nothing.
func TestRWMutexWaitReadableTakesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.WaitReadable(t.Context()); err != nil {
			t.Fatalf("WaitReadable on an unlocked RWMutex: %v", err)
		}
		if err := m.Lock(t.Context()); err != nil {
			t.Fatal(err)
		}
		readable := make(chan time.Time, 1)
		go func() {
			if err := m.WaitReadable(t.Context()); err != nil {
				t.Error(err)
				return
			}
			readable <- time.Now()
		}()
		start := time.Now()
		time.AfterFunc(time.Second, m.Unlock)
		if got := (<-readable).Sub(start); got != time.Second {
			t.Fatalf("the reader woke after %v, want 1s", got)
		}
		if err := m.Lock(t.Context()); err != nil {
			t.Fatalf("a reader took the lock: %v", err)
		}
		m.Unlock()
	})
}

func TestRWMutexWaitReadableReturnsContextCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.Lock(t.Context()); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(time.Second, func() { cancel(cause) })
		if err := m.WaitReadable(ctx); !errors.Is(err, cause) {
			t.Fatalf("WaitReadable error = %v, want cancellation cause", err)
		}
		m.Unlock()
	})
}

func TestRWMutexTryLockNeverWaits(t *testing.T) {
	t.Parallel()
	m := ctxsync.NewRWMutex()
	if err := m.RLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.TryLock() {
		t.Fatal("TryLock took the lock from a reader")
	}
	m.RUnlock()
	if !m.TryLock() {
		t.Fatal("TryLock refused an unlocked RWMutex")
	}
	if m.TryLock() || m.TryRLock() {
		t.Fatal("a held RWMutex was taken again")
	}
	m.Unlock()
}

// A waiting Writer holds new readers off, wakes once the readers have gone,
// takes nothing until it tries, and lets the readers go on once it leaves.
func TestRWMutexWriterHoldsReadersOffAndTakesTheLockWhenItTries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.RLock(t.Context()); err != nil {
			t.Fatal(err)
		}
		w := m.Writer()
		if m.TryRLock() {
			t.Fatal("a new reader went ahead of a waiting writer")
		}
		if w.TryLock() {
			t.Fatal("the writer took the lock from a reader")
		}
		free := make(chan time.Time, 1)
		go func() {
			if err := w.Wait(t.Context()); err != nil {
				t.Error(err)
				return
			}
			free <- time.Now()
		}()
		start := time.Now()
		time.AfterFunc(time.Second, m.RUnlock)
		if got := (<-free).Sub(start); got != time.Second {
			t.Fatalf("the writer woke after %v, want 1s", got)
		}
		if m.TryRLock() {
			t.Fatal("a reader went ahead of a writer that woke and has not tried")
		}
		if !w.TryLock() {
			t.Fatal("the writer could not take the free lock")
		}
		if w.TryLock() || m.TryRLock() {
			t.Fatal("a held RWMutex was taken again")
		}
		m.Unlock()
		if !m.TryRLock() {
			t.Fatal("the readers are held off once the writer gave the lock back")
		}
		m.RUnlock()
	})
}

func TestRWMutexWriterThatLeavesLetsReadersGoOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := ctxsync.NewRWMutex()
		if err := m.RLock(t.Context()); err != nil {
			t.Fatal(err)
		}
		w := m.Writer()
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(time.Second, func() { cancel(cause) })
		if err := w.Wait(ctx); !errors.Is(err, cause) {
			t.Fatalf("Wait error = %v, want cancellation cause", err)
		}
		w.Leave()
		w.Leave()
		if !m.TryRLock() {
			t.Fatal("a writer that left still holds readers off")
		}
		m.RUnlock()
		m.RUnlock()
	})
}
