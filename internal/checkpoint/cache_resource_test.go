package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/resource"
)

func sharedCache(t *testing.T, limit int64) (*Cache, *resource.Budget) {
	t.Helper()
	b, err := resource.New(limit)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(b, CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache, b
}

func cacheRead(t *testing.T, cache *Cache, key cacheKey) ([]byte, func()) {
	t.Helper()
	data, release, err := cache.get(t.Context(), key, func(context.Context) ([]byte, error) { return bytes.Repeat([]byte("r"), 128), nil })
	if err != nil {
		t.Fatal(err)
	}
	return data, release
}

func TestSharedAdmissionEvictsOnlyNecessaryLRUEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, b := sharedCache(t, 1600)
		_, release := cacheRead(t, cache, cacheKeyOf("old"))
		release()
		_, release = cacheRead(t, cache, cacheKeyOf("recent"))
		release()
		lease, err := b.TryAcquire(t.Context(), 900)
		if err != nil {
			t.Fatalf("non-cache admission failed before cache eviction: %v", err)
		}
		defer lease.Close()
		if cache.Stats().Entries != 1 || cache.Stats().Evictions != 1 || b.Stats().Used != 1540 {
			t.Fatalf("wrong shared occupancy after eviction: cache=%+v budget=%+v", cache.Stats(), b.Stats())
		}
		if cache.entries[cacheKeyOf("old")] != nil || cache.entries[cacheKeyOf("recent")] == nil {
			t.Fatal("shared pressure evicted the most recent entry first")
		}
	})
}

func TestPinnedCacheBytesStayChargedUntilReaderFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, b := sharedCache(t, 800)
		data, release := cacheRead(t, cache, cacheKeyOf("pinned"))
		cache.Clear()
		if _, err := b.TryAcquire(t.Context(), 300); !errors.Is(err, resource.ErrCapacity) {
			t.Fatalf("clearing cache released an active reader's memory: %v", err)
		}
		if !bytes.Equal(data, bytes.Repeat([]byte("r"), 128)) || b.Stats().Used != 640 {
			t.Fatal("active cache borrow lost data or accounting")
		}
		release()
		lease, err := b.TryAcquire(t.Context(), 300)
		if err != nil {
			t.Fatal(err)
		}
		lease.Close()
		if b.Stats().Used != 0 {
			t.Fatalf("finished reader retained a charge: %+v", b.Stats())
		}
	})
}

func TestFinishingReaderYieldsCacheBytesToWaitingNonCacheWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, b := sharedCache(t, 800)
		_, release := cacheRead(t, cache, cacheKeyOf("pinned"))
		done := make(chan error, 1)
		go func() {
			lease, err := b.Acquire(t.Context(), 300)
			if err == nil {
				lease.Close()
			}
			done <- err
		}()
		synctest.Wait()
		if b.Stats().Waiting != 1 {
			t.Fatal("non-cache request did not wait for pinned bytes")
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != 0 || cache.Stats().Entries != 0 {
			t.Fatalf("finishing read retained cache ahead of waiting work: %+v %+v", b.Stats(), cache.Stats())
		}
	})
}

func TestCacheMissReclaimsUnusedEntriesBeforeFailingRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, b := sharedCache(t, 800)
		_, release := cacheRead(t, cache, cacheKeyOf("old"))
		release()
		data, release := cacheRead(t, cache, cacheKeyOf("new"))
		defer release()
		if len(data) != 128 || cache.Stats().Evictions != 1 || b.Stats().Used != 640 {
			t.Fatalf("new read failed to replace unused cache: %+v %+v", cache.Stats(), b.Stats())
		}
	})
}

func TestCacheUsesMemoryReturnedByOtherHostConsumers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const charge = 128 + cacheEntryCharge
		budget, err := resource.New(4 * charge)
		if err != nil {
			t.Fatal(err)
		}
		cache, err := NewCache(budget, CacheConfig{})
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		pages, err := budget.TryAcquire(t.Context(), 2*charge)
		if err != nil {
			t.Fatal(err)
		}
		defer pages.Close()
		for _, key := range []cacheKey{cacheKeyOf("a"), cacheKeyOf("b")} {
			_, release := cacheRead(t, cache, key)
			release()
		}
		if cache.Stats().Entries != 2 {
			t.Fatalf("unused host memory was not retained: %+v", cache.Stats())
		}
		pages.Close()
		for _, key := range []cacheKey{cacheKeyOf("c"), cacheKeyOf("d")} {
			_, release := cacheRead(t, cache, key)
			release()
		}
		if cache.Stats().Entries != 4 || budget.Stats().Used != 4*charge {
			t.Fatalf("cache did not use returned host memory: %+v %+v", cache.Stats(), budget.Stats())
		}
		pages, err = budget.TryAcquire(t.Context(), 3*charge)
		if err != nil {
			t.Fatalf("cache refused to yield to guest pages: %v", err)
		}
		defer pages.Close()
		if cache.Stats().Entries != 1 || cache.entries[cacheKeyOf("d")] == nil || budget.Stats().Used != 4*charge {
			t.Fatalf("wrong reclamation: %+v %+v", cache.Stats(), budget.Stats())
		}
	})
}

func TestCacheMissStillReadsWhenGuestPagesUseTheMemoryAllotment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, budget := sharedCache(t, 640)
		pages, err := budget.TryAcquire(t.Context(), 640)
		if err != nil {
			t.Fatal(err)
		}
		defer pages.Close()
		data, release := cacheRead(t, cache, cacheKeyOf("page"))
		if string(data) != string(bytes.Repeat([]byte("r"), 128)) {
			t.Fatal("transient read changed bytes")
		}
		release()
		if budget.Stats().Used != 640 || cache.Stats().Entries != 0 {
			t.Fatalf("transient read retained cache or changed page ownership: %+v %+v", cache.Stats(), budget.Stats())
		}
		pages.Close()
		_, release = cacheRead(t, cache, cacheKeyOf("page"))
		release()
		if cache.Stats().Entries != 1 || budget.Stats().Used != 640 {
			t.Fatal("cache did not resume when page memory was returned")
		}
	})
}

// cacheKeyOf builds the key the cache holds one page's bytes under, so a test
// can name entries without publishing anything.
func cacheKeyOf(name string) cacheKey {
	return pageKey(control.Identity{Ref: control.Ref{VM: "vm", Sequence: 1}, Volume: name})
}
