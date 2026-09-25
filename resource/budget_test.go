package resource_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/resource"
)

func budget(t *testing.T, limit int64) *resource.Budget {
	t.Helper()
	b, err := resource.New(limit)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func take(t *testing.T, b *resource.Budget, amount int64) *resource.Lease {
	t.Helper()
	l, err := b.TryAcquire(t.Context(), amount)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReservationsShareOneTotal(t *testing.T) {
	b := budget(t, 100)
	cache := take(t, b, 30)
	pages := take(t, b, 70)
	if _, err := b.TryAcquire(t.Context(), 1); !errors.Is(err, resource.ErrCapacity) {
		t.Fatalf("allocation exceeded the total: %v", err)
	}
	if got := b.Stats().Used; got != 100 {
		t.Fatal(got)
	}
	cache.Close()
	pages.Close()
	pages.Close()
	if got := b.Stats().Used; got != 0 {
		t.Fatal(got)
	}
}

func TestGrowthIsRefusedWholeAndKeepsWhatIsHeld(t *testing.T) {
	b := budget(t, 16)
	held := take(t, b, 8)
	other := take(t, b, 8)
	if err := held.TryGrow(t.Context(), 8); !errors.Is(err, resource.ErrCapacity) {
		t.Fatal(err)
	}
	if got := held.Bytes(); got != 8 {
		t.Fatalf("refused growth changed ownership: %d", got)
	}
	other.Close()
	if err := held.TryGrow(t.Context(), 8); err != nil {
		t.Fatal(err)
	}
	if got := b.Stats().Used; got != 16 {
		t.Fatal(got)
	}
	if err := held.Release(8); err != nil {
		t.Fatal(err)
	}
	if got := b.Stats().Used; got != 8 {
		t.Fatal(got)
	}
	held.Close()
}

func TestWaitingAdmissionPreservesFIFO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budget(t, 10)
		resident := take(t, b, 8)
		large := make(chan *resource.Lease, 1)
		go func() {
			l, err := b.Acquire(t.Context(), 8)
			if err != nil {
				t.Error(err)
			}
			large <- l
		}()
		synctest.Wait()
		if err := resident.Release(1); err != nil {
			t.Fatal(err)
		}
		if _, err := b.TryAcquire(t.Context(), 1); !errors.Is(err, resource.ErrCapacity) {
			t.Fatalf("small allocation bypassed older waiter: %v", err)
		}
		resident.Close()
		synctest.Wait()
		l := <-large
		if l == nil {
			t.Fatal("waiter failed")
		}
		l.Close()
		if s := b.Stats(); s.Waiting != 0 || s.Used != 0 {
			t.Fatal(s)
		}
	})
}

func TestCancellationReturnsQueuedAndCrossingGrants(t *testing.T) {
	for _, releaseFirst := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			b := budget(t, 1)
			held := take(t, b, 1)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				lease, err := b.Acquire(ctx, 1)
				if lease != nil {
					lease.Close()
				}
				done <- err
			}()
			synctest.Wait()
			if releaseFirst {
				held.Close()
			}
			cancel()
			held.Close()
			synctest.Wait()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if s := b.Stats(); s.Waiting != 0 || s.Used != 0 {
				t.Fatalf("canceled or released grant leaked: %+v", s)
			}
		})
	}
}

func TestInvalidAndImpossibleReservationsCannotWrapOrWait(t *testing.T) {
	b := budget(t, math.MaxInt64)
	l := take(t, b, math.MaxInt64-1)
	if err := l.TryGrow(t.Context(), 2); !errors.Is(err, resource.ErrCapacity) {
		t.Fatalf("overflowing reservation admitted: %v", err)
	}
	if got := b.Stats().Used; got != math.MaxInt64-1 {
		t.Fatal("refused growth escaped atomic admission")
	}
	small := budget(t, 4)
	if _, err := small.Acquire(t.Context(), 5); !errors.Is(err, resource.ErrCapacity) {
		t.Fatalf("impossible request waited: %v", err)
	}
	if err := l.Release(math.MaxInt64); !errors.Is(err, resource.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := b.TryAcquire(t.Context(), -1); !errors.Is(err, resource.ErrInvalid) {
		t.Fatal(err)
	}
	l.Close()
	if err := l.TryGrow(t.Context(), 1); !errors.Is(err, resource.ErrClosed) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
