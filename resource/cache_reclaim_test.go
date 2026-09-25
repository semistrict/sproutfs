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
