package ctxsync_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
)

func TestLazyMutexWaitIsContextAwareAndAdvancesFakeTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mutex ctxsync.LazyMutex
		if err := mutex.Lock(t.Context()); err != nil {
			t.Fatal(err)
		}
		acquired := make(chan time.Time, 1)
		go func() {
			if err := mutex.Lock(t.Context()); err != nil {
				t.Error(err)
				return
			}
			defer mutex.Unlock()
			acquired <- time.Now()
		}()
		start := time.Now()
		time.AfterFunc(time.Second, mutex.Unlock)
		if got := <-acquired; got.Sub(start) != time.Second {
			t.Fatalf("acquired after %v, want 1s", got.Sub(start))
		}
	})
}

func TestLazyMutexWakesEveryWaiterInTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mutex ctxsync.LazyMutex
		if err := mutex.Lock(t.Context()); err != nil {
			t.Fatal(err)
		}
		const waiters = 3
		held := make(chan int, waiters)
		for i := range waiters {
			go func() {
				if err := mutex.Lock(t.Context()); err != nil {
					t.Error(err)
					return
				}
				held <- i
				time.Sleep(time.Second)
				mutex.Unlock()
			}()
		}
		synctest.Wait()
		start := time.Now()
		mutex.Unlock()
		for range waiters {
			<-held
		}
		if got := time.Since(start); got != (waiters-1)*time.Second {
			t.Fatalf("the last waiter took the lock after %v, want %v", got, (waiters-1)*time.Second)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !mutex.TryLock() {
			t.Fatal("the lock is held after every waiter gave it back")
		}
	})
}

func TestLazyMutexLockReturnsContextCause(t *testing.T) {
	t.Parallel()
	var mutex ctxsync.LazyMutex
	if err := mutex.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("stop waiting")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	if err := mutex.Lock(ctx); !errors.Is(err, cause) {
		t.Fatalf("Lock error = %v, want cancellation cause", err)
	}
	mutex.Unlock()
}

func TestLazyMutexWaiterGivesUpWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mutex ctxsync.LazyMutex
		if !mutex.TryLock() {
			t.Fatal("an unlocked LazyMutex refused TryLock")
		}
		if mutex.TryLock() {
			t.Fatal("a held LazyMutex granted TryLock")
		}
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(time.Second, func() { cancel(cause) })
		start := time.Now()
		if err := mutex.Lock(ctx); !errors.Is(err, cause) {
			t.Fatalf("Lock error = %v, want cancellation cause", err)
		}
		if got := time.Since(start); got != time.Second {
			t.Fatalf("gave up after %v, want 1s", got)
		}
		mutex.Unlock()
		if !mutex.TryLock() {
			t.Fatal("the lock is held after its holder gave it back")
		}
	})
}

func TestLazyMutexAllocatesNothingUncontended(t *testing.T) {
	var mutex ctxsync.LazyMutex
	ctx := t.Context()
	allocs := testing.AllocsPerRun(100, func() {
		if err := mutex.Lock(ctx); err != nil {
			t.Fatal(err)
		}
		mutex.Unlock()
		if !mutex.TryLock() {
			t.Fatal("an unlocked LazyMutex refused TryLock")
		}
		mutex.Unlock()
	})
	if allocs != 0 {
		t.Fatalf("an uncontended Lock, TryLock and Unlock allocate %v times, want 0", allocs)
	}
}

func TestLazyMutexUnlockOfUnlockedPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if got := recover(); got != "ctxsync: unlock of unlocked LazyMutex" {
			t.Fatalf("recovered %v, want the unlock panic", got)
		}
	}()
	var mutex ctxsync.LazyMutex
	mutex.Unlock()
}

// WaitFree waits for the holder's Unlock and takes nothing: every waiter
// wakes to an unlocked mutex, and the one that tries first takes it.
func TestLazyMutexWaitFreeTakesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mutex ctxsync.LazyMutex
		if err := mutex.WaitFree(t.Context()); err != nil {
			t.Fatalf("WaitFree on an unlocked LazyMutex: %v", err)
		}
		if !mutex.TryLock() {
			t.Fatal("WaitFree took the lock")
		}
		const waiters = 3
		freed := make(chan time.Time, waiters)
		for range waiters {
			go func() {
				if err := mutex.WaitFree(t.Context()); err != nil {
					t.Error(err)
					return
				}
				freed <- time.Now()
			}()
		}
		start := time.Now()
		time.AfterFunc(time.Second, mutex.Unlock)
		for range waiters {
			if got := (<-freed).Sub(start); got != time.Second {
				t.Fatalf("a waiter woke after %v, want 1s", got)
			}
		}
		if !mutex.TryLock() {
			t.Fatal("a waiter took the lock")
		}
		mutex.Unlock()
	})
}

func TestLazyMutexWaitFreeReturnsContextCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mutex ctxsync.LazyMutex
		if !mutex.TryLock() {
			t.Fatal("an unlocked LazyMutex refused TryLock")
		}
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(time.Second, func() { cancel(cause) })
		if err := mutex.WaitFree(ctx); !errors.Is(err, cause) {
			t.Fatalf("WaitFree error = %v, want cancellation cause", err)
		}
		mutex.Unlock()
	})
}
