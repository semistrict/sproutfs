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
