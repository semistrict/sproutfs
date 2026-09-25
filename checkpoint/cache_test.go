package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// cacheStore counts every get of a checkpoint object and optionally suspends
// the fetches the cache issues for part members, which is where a page comes
// from.
type cacheStore struct {
	platform.ObjectStore
	gets      atomic.Int64
	canceled  atomic.Int64
	block     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	suspended string
}

func (s *cacheStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.gets.Add(1)
	if strings.Contains(request.Key.String(), s.suspended) {
		if s.block.Load() {
			s.entered <- struct{}{}
			select {
			case <-s.release:
			case <-ctx.Done():
				s.canceled.Add(1)
				return platform.GetResult{}, context.Cause(ctx)
			}
		}
	}
	return s.ObjectStore.Get(ctx, request)
}

const cachedPages = 4

// fixtureSegments is how many page-table segments the fixture's volume has: one
// segment covers far more than four pages. A handle that publishes a checkpoint
// has its segments in hand; one that opens a published checkpoint fetches them,
// which is one range get however many pages they name.
const fixtureSegments = 1

// opensOne is what opening a published checkpoint costs: one get of its index
// object.
const opensOne = 1

// cachedFixture publishes one VM whose volume is four fully written pages and
// returns a store reading it through a shared cache.
func cachedFixture(t *testing.T, capacity int64, concurrency int) (*checkpoint.Store, *checkpoint.Index, *model, *checkpoint.Cache, *cacheStore) {
	t.Helper()
	budget, err := resource.New(capacity)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{MaxConcurrentLoads: concurrency})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	objects := &cacheStore{ObjectStore: sim.New(sim.Config{}).ObjectStore(), suspended: "/part/",
		entered: make(chan struct{}, 64), release: make(chan struct{})}
	store := mustStore(t, checkpoint.Config{ObjectStore: objects, Cache: cache})
	sizes := map[string]uint64{"root": cachedPages * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: "cached", Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(volumes2MiB(sizes))
	p := store.Begin(root, control.Ref{VM: "cached", Sequence: 2})
	for page := range uint64(cachedPages) {
		for sector := range uint32(sectorsPerPage) {
			m.dirty(p, "root", page, sector, sectorData("cached", page, sector))
		}
	}
	index, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	objects.gets.Store(0)
	return store, index, m, cache, objects
}

func readCachedPage(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model, page uint64) {
	t.Helper()
	got := make([]byte, checkpoint.PageSize2MiB)
	if err := store.Read(t.Context(), index, "root", page*checkpoint.PageSize2MiB, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, m.contents["root"][page*checkpoint.PageSize2MiB:(page+1)*checkpoint.PageSize2MiB]) {
		t.Fatalf("page %d differs from the model", page)
	}
}

func TestCacheSharesInheritedPagesAndAccountsHits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, index, m, cache, objects := cachedFixture(t, 16<<20, 4)
		for page := range uint64(cachedPages) {
			readCachedPage(t, store, index, m, page)
		}
		if stats := cache.Stats(); stats.Misses != cachedPages || stats.Hits != 0 || objects.gets.Load() != cachedPages {
			t.Fatalf("cold reads: %+v, gets=%d", stats, objects.gets.Load())
		}
		// A second handle on the same checkpoint reads no page bytes from
		// storage: it opens the checkpoint, which is one get of its index
		// object, fetches the segment that locates the pages, and every page of
		// it is already cached. The open is not a cache fetch, so it counts as a
		// get and not as a miss.
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		for page := range uint64(cachedPages) {
			readCachedPage(t, store, reopened, m, page)
		}
		if stats := cache.Stats(); stats.Hits != cachedPages || stats.Misses != cachedPages+fixtureSegments ||
			objects.gets.Load() != cachedPages+fixtureSegments+opensOne {
			t.Fatalf("reopened reads: %+v, gets=%d", stats, objects.gets.Load())
		}

		// A fork inherits its parent's object keys, so only the page it wrote
		// costs a fetch.
		forkModel := m.clone()
		p := store.Begin(index, control.Ref{VM: "forked", Sequence: 1})
		forkModel.dirty(p, "root", 0, 9, sectorData("forked", 0, 9))
		fork, err := p.Commit(t.Context(), forkModel)
		if err != nil {
			t.Fatal(err)
		}
		for page := uint64(1); page < cachedPages; page++ {
			readCachedPage(t, store, fork, forkModel, page)
		}
		// The fork's own publication read its parent's segment to edit it, which
		// the reopened handle had already cached, so that is a hit too.
		if stats := cache.Stats(); stats.Hits != 2*cachedPages-1+fixtureSegments ||
			objects.gets.Load() != cachedPages+fixtureSegments+opensOne {
			t.Fatalf("untouched fork pages: %+v, gets=%d", stats, objects.gets.Load())
		}
		// The page the fork wrote is a new object of its own, one fetch and no
		// inherited object to read beside it.
		readCachedPage(t, store, fork, forkModel, 0)
		if stats := cache.Stats(); stats.Hits != 2*cachedPages-1+fixtureSegments ||
			stats.Misses != cachedPages+fixtureSegments+1 ||
			objects.gets.Load() != cachedPages+fixtureSegments+opensOne+1 {
			t.Fatalf("forked page: %+v, gets=%d", stats, objects.gets.Load())
		}
	})
}

func TestCacheLRUEvictionUnderSharedPressureAndClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Three pages of budget hold exactly two entries once the per-entry
		// bookkeeping charge is counted.
		store, index, m, cache, objects := cachedFixture(t, 3*checkpoint.PageSize2MiB, 2)
		readCachedPage(t, store, index, m, 0)
		readCachedPage(t, store, index, m, 1)
		readCachedPage(t, store, index, m, 0)
		readCachedPage(t, store, index, m, 2)
		if cache.Stats().Entries != 2 {
			t.Fatalf("resident entries %d, want 2", cache.Stats().Entries)
		}
		readCachedPage(t, store, index, m, 0)
		readCachedPage(t, store, index, m, 2)
		if objects.gets.Load() != 3 {
			t.Fatalf("cache evicted recently used pages: gets=%d", objects.gets.Load())
		}
		readCachedPage(t, store, index, m, 1)
		if objects.gets.Load() != 4 {
			t.Fatalf("cold page was not refetched after eviction: gets=%d", objects.gets.Load())
		}
		cache.Clear()
		if stats := cache.Stats(); stats.ResidentBytes != 0 || stats.Entries != 0 || stats.Evictions != 4 {
			t.Fatalf("clear did not release cache ownership: %+v", stats)
		}
		readCachedPage(t, store, index, m, 1)
		readCachedPage(t, store, index, m, 1)
		if objects.gets.Load() != 5 || cache.Stats().Entries != 1 {
			t.Fatalf("new reads did not repopulate the cleared cache: gets=%d", objects.gets.Load())
		}
	})
}

func TestCacheCoalescesMissesAndSurvivesOneCallerLeaving(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, index, m, cache, objects := cachedFixture(t, 16<<20, 1)
		readCachedPage(t, store, index, m, 0)
		objects.block.Store(true)
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("reader left")
		first := make(chan error, 1)
		second := make(chan error, 1)
		go func() { first <- store.Read(ctx, index, "root", checkpoint.PageSize2MiB, make([]byte, 1)) }()
		<-objects.entered
		go func() { second <- store.Read(t.Context(), index, "root", checkpoint.PageSize2MiB, make([]byte, 1)) }()
		synctest.Wait()
		cancel(cause)
		if err := <-first; !errors.Is(err, cause) {
			t.Fatalf("canceled caller: %v", err)
		}
		synctest.Wait()
		if stats := cache.Stats(); objects.canceled.Load() != 0 || stats.ActiveLoads != 1 || stats.CoalescedLoads != 1 {
			t.Fatalf("one caller canceled another caller's shared fetch: %+v", stats)
		}
		close(objects.release)
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if objects.gets.Load() != 2 {
			t.Fatalf("duplicate load for the same object: gets=%d", objects.gets.Load())
		}
		readCachedPage(t, store, index, m, 1)
		if objects.gets.Load() != 2 {
			t.Fatalf("coalesced load was not retained: gets=%d", objects.gets.Load())
		}
	})
}

func TestCacheBoundsLoadsAndCancelsAbandonedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, index, m, cache, objects := cachedFixture(t, 16<<20, 2)
		readCachedPage(t, store, index, m, 0)
		objects.block.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, cachedPages-1)
		for page := uint64(1); page < cachedPages; page++ {
			go func() { done <- store.Read(ctx, index, "root", page*checkpoint.PageSize2MiB, make([]byte, 1)) }()
		}
		synctest.Wait()
		if stats := cache.Stats(); stats.ActiveLoads != 2 || stats.PeakLoads != 2 || objects.gets.Load() != 3 {
			t.Fatalf("unbounded cache miss concurrency: %+v, gets=%d", stats, objects.gets.Load())
		}
		cancel()
		for range cachedPages - 1 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("abandoned reader: %v", err)
			}
		}
		synctest.Wait()
		if cache.Stats().ActiveLoads != 0 || objects.canceled.Load() != 2 {
			t.Fatalf("abandoned fetches retained their slots: %+v", cache.Stats())
		}
		objects.block.Store(false)
		readCachedPage(t, store, index, m, cachedPages-1)
	})
}

func TestCacheClearDoesNotRepopulateFromOldLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, index, m, cache, objects := cachedFixture(t, 16<<20, 1)
		readCachedPage(t, store, index, m, 0)
		objects.block.Store(true)
		done := make(chan error, 1)
		go func() { done <- store.Read(t.Context(), index, "root", checkpoint.PageSize2MiB, make([]byte, 1)) }()
		<-objects.entered
		cache.Clear()
		close(objects.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if stats := cache.Stats(); stats.Entries != 0 || stats.ResidentBytes != 0 {
			t.Fatalf("an old fill repopulated the cleared cache: %+v", stats)
		}
		readCachedPage(t, store, index, m, 1)
		if objects.gets.Load() != 3 {
			t.Fatalf("clear retained the old load: gets=%d", objects.gets.Load())
		}
	})
}

// A member is read by the extent the index recorded for it, so bytes that no
// longer decode under that extent are corrupt however valid the part around
// them still is.
func TestCachedReadsRejectAMemberThatDoesNotDecode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, index, _, _, objects := cachedFixture(t, 16<<20, 1)
		key := partKey(t, "cached", 2, 0)
		part, _, err := platform.ReadObject(t.Context(), objects.ObjectStore, key, 0, 1<<30, errors.New("unreadable part"))
		if err != nil {
			t.Fatal(err)
		}
		// The first member's payload starts right after its envelope header.
		part[blob.HeaderSize] ^= 1
		replace(t, objects.ObjectStore, key, part)
		if err := store.Read(t.Context(), index, "root", 0, make([]byte, 1)); !errors.Is(err, checkpoint.ErrCorrupt) {
			t.Fatalf("member whose bytes disagree with its envelope: %v", err)
		}
	})
}

// replace overwrites one published object, which is otherwise immutable.
func replace(t *testing.T, objects platform.ObjectStore, key platform.ObjectKey, data []byte) {
	t.Helper()
	if err := objects.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(t.Context(), platform.PutRequest{
		Key: key, Body: bytes.NewReader(data), Size: int64(len(data)),
	}); err != nil {
		t.Fatal(err)
	}
}
