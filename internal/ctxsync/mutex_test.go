package ctxsync_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
)

func TestMutexWaitIsContextAwareAndAdvancesFakeTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mutex := ctxsync.NewMutex()
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

func TestMutexLockReturnsContextCause(t *testing.T) {
	t.Parallel()
	mutex := ctxsync.NewMutex()
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
