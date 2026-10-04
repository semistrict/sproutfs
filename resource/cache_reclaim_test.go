package resource_test

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/resource"
)

func TestNonCacheAdmissionEvictsBeforeRefusal(t *testing.T) {
	b := budget(t, 100)
	cached, err := b.TryAcquireCache(t.Context(), 60)
	if err != nil {
		t.Fatal(err)
	}
	evictions := 0
	unregister := b.RegisterCache(func(ctx context.Context, requested int64) (bool, error) {
		if evictions != 0 {
			return false, nil
		}
		if requested != 60 {
			t.Fatalf("wrong amount requested: %d", requested)
		}
		if _, err := b.TryAcquireCache(ctx, 1); !errors.Is(err, resource.ErrCapacity) {
			t.Fatalf("cache refilled during non-cache admission: %v", err)
		}
		cached.Close()
		evictions++
		return true, nil
	})
	defer unregister()
	normal, err := b.TryAcquire(t.Context(), 60)
	if err != nil {
		t.Fatalf("non-cache admission failed despite evictable bytes: %v", err)
	}
	defer normal.Close()
	if evictions != 1 || b.Stats().Used != 60 {
		t.Fatalf("incorrect eviction: %d %+v", evictions, b.Stats())
	}
}

func TestCacheEvictionPrecedesGrowth(t *testing.T) {
	b := budget(t, 100)
	owner := take(t, b, 30)
	defer owner.Close()
	cached, err := b.TryAcquireCache(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	evicted := false
	defer b.RegisterCache(func(context.Context, int64) (bool, error) {
		if evicted {
			return false, nil
		}
		cached.Close()
		evicted = true
		return true, nil
	})()
	if err := owner.TryGrow(t.Context(), 40); err != nil || !evicted {
		t.Fatalf("growth failed to reclaim cache first: %v, evicted=%v", err, evicted)
	}
}

func TestFailedCacheEvictionRetainsCharge(t *testing.T) {
	b := budget(t, 100)
	cached, err := b.TryAcquireCache(t.Context(), 80)
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	failure := errors.New("cache eviction failed")
	defer b.RegisterCache(func(context.Context, int64) (bool, error) { return false, failure })()
	if _, err := b.TryAcquire(t.Context(), 50); !errors.Is(err, resource.ErrCapacity) || !errors.Is(err, failure) {
		t.Fatalf("unproven eviction admitted new bytes: %v", err)
	}
	if got := b.Stats().Used; got != 80 {
		t.Fatalf("refused admission changed ownership: %d", got)
	}
}

// A reservation larger than the whole allotment is refused without asking a
// cache to give anything back: nothing it gave back would make room, and a
// cache read whose entry can never be kept would empty the cache every time.
func TestAReservationLargerThanTheAllotmentEvictsNothing(t *testing.T) {
	b := budget(t, 100)
	cached, err := b.TryAcquireCache(t.Context(), 60)
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	defer b.RegisterCache(func(context.Context, int64) (bool, error) {
		t.Fatal("a reservation past the allotment asked the cache to give something back")
		return false, nil
	})()
	if _, err := b.TryAcquire(t.Context(), 101); !errors.Is(err, resource.ErrCapacity) {
		t.Fatalf("a reservation of 101 bytes of 100 returned %v, want %v", err, resource.ErrCapacity)
	}
	if used := b.Stats().Used; used != 60 {
		t.Fatalf("the budget holds %d bytes, want the cache's 60", used)
	}
}

// pressed reports whether pressure was signalled since the channel was taken.
func pressed(pressure <-chan struct{}) bool {
	select {
	case <-pressure:
		return true
	default:
		return false
	}
}

// The allotment is under pressure only once its caches cannot give back
// enough: a reservation a cache's eviction makes room for is no pressure, one
// refused after every cache gave back what it could is, and so is a growth
// refused that way and a reservation that has to wait. A reservation larger
// than the whole allotment is refused before any cache is asked, and is none.
func TestPressureIsAShortageNoCacheCanGiveBack(t *testing.T) {
	b := budget(t, 100)
	cached, err := b.TryAcquireCache(t.Context(), 60)
	if err != nil {
		t.Fatal(err)
	}
	defer b.RegisterCache(func(context.Context, int64) (bool, error) {
		if cached == nil {
			return false, nil
		}
		cached.Close()
		cached = nil
		return true, nil
	})()
	pressure := b.Pressure()
	held := take(t, b, 70)
	defer held.Close()
	if pressed(pressure) {
		t.Fatal("a reservation a cache made room for signalled pressure")
	}
	if _, err := b.TryAcquire(t.Context(), 101); !errors.Is(err, resource.ErrCapacity) || pressed(pressure) {
		t.Fatalf("a reservation larger than the allotment: %v, pressed %v; want refused with no pressure", err,
			pressed(pressure))
	}
	if _, err := b.TryAcquire(t.Context(), 31); !errors.Is(err, resource.ErrCapacity) || !pressed(pressure) {
		t.Fatalf("a reservation no cache could make room for: %v, pressed %v; want refused under pressure", err,
			pressed(pressure))
	}
	pressure = b.Pressure()
	if pressed(pressure) {
		t.Fatal("pressure stays signalled on the next channel")
	}
	if err := held.TryGrow(t.Context(), 31); !errors.Is(err, resource.ErrCapacity) || !pressed(pressure) {
		t.Fatalf("a growth no cache could make room for: %v, pressed %v; want refused under pressure", err,
			pressed(pressure))
	}
	pressure = b.Pressure()
	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() {
		_, err := b.Acquire(ctx, 31)
		waited <- err
	}()
	<-pressure
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiting reservation ended with %v", err)
	}
}
