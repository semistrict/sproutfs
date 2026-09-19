package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/resource"
)

// identityOfPage reports the identity one whole page of a volume reads under.
func identityOfPage(t *testing.T, index *checkpoint.Index, volume string, page uint64) control.Identity {
	t.Helper()
	extents, err := index.Locate(t.Context(), volume, page*checkpoint.PageSize2MiB, checkpoint.PageSize2MiB)
	if err != nil {
		t.Fatal(err)
	}
	if len(extents) != 1 {
		t.Fatalf("page %d of %s resolved to %d extents, want one", page, volume, len(extents))
	}
	return extents[0].Identity
}

// compactionFixture publishes a VM whose parts for sequence 2 hold four pages,
// overwrites one of them in sequence 3, and returns the store and the index of
// sequence 3. Page 3 is the cold page still living in the parts of sequence 2.
func compactionFixture(t *testing.T, cache *checkpoint.Cache, vm string) (*checkpoint.Store, *checkpoint.Index, *model) {
	t.Helper()
	objects := sim.New(sim.Config{}).ObjectStore()
	store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects, Cache: cache}, vm)
	first := store.Begin(root, control.Ref{VM: vm, Sequence: 2})
	for page := range uint64(checkpointPages) {
		write(first, m, "first", page)
	}
	second, err := first.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	p := store.Begin(second, control.Ref{VM: vm, Sequence: 3})
	write(p, m, "third", 0)
	third, err := p.Commit(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(third.Checkpoints(), second.Ref()) {
		t.Fatalf("the fixture expected sequence 2 to survive: %v", third.Checkpoints())
	}
	return store, third, m
}

// Compaction moves a page's bytes into another checkpoint's parts. It must not change the
// page's identity: the identity names the checkpoint the page was first
// published under, which is what a fork of the older view goes on reporting and
// what the page cache keys those bytes by.
func TestCompactionKeepsPageIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, third, m := compactionFixture(t, nil, "identity")
		before := identityOfPage(t, third, "root", 3)

		// Three of four pages rewritten leaves a quarter of sequence 2 live, so
		// compaction rewrites the cold page 3.
		q := store.Begin(third, control.Ref{VM: "identity", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if after := identityOfPage(t, fourth, "root", 3); after != before {
			t.Fatalf("compaction changed page 3's identity from %v to %v", before, after)
		}
		checkRead(t, store, fourth, m)
	})
}

// A page whose bytes compaction moved is the same page, so the checkpoint that
// moved it and a fork that still reads the old parts share one cache entry.
func TestCompactionKeepsOnePageCacheEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget, err := resource.New(64 << 20)
		if err != nil {
			t.Fatal(err)
		}
		cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{MaxConcurrentLoads: 4})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cache.Close)
		store, third, m := compactionFixture(t, cache, "shared")

		q := store.Begin(third, control.Ref{VM: "shared", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		readCachedPageOf(t, store, third, m, 3)
		before := cache.Stats()
		readCachedPageOf(t, store, fourth, m, 3)
		after := cache.Stats()
		if after.Misses != before.Misses || after.Hits != before.Hits+1 {
			t.Fatalf("the moved page cost a second entry: %+v then %+v", before, after)
		}
	})
}

func readCachedPageOf(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model, page uint64) {
	t.Helper()
	readCachedPage(t, store, index, m, page)
}

// A pinned checkpoint's index names the checkpoints a fork reads through. Compaction
// must leave every one of them alone, not only the pinned sequence itself:
// rewriting them copies bytes reclamation can never free, because the fork goes
// on reading the parts they were copied out of.
func TestCompactionSparesThePacksAPinnedIndexNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, third, m := compactionFixture(t, nil, "pinnedpacks")
		older := control.Ref{VM: "pinnedpacks", Sequence: 2}

		q := store.Begin(third, control.Ref{VM: "pinnedpacks", Sequence: 4})
		q.Protect([]uint64{third.Ref().Sequence})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fourth.Checkpoints(), older) {
			t.Fatalf("compaction rewrote a checkpoint the pinned index names: %v", fourth.Checkpoints())
		}
		checkRead(t, store, fourth, m)
	})
}

// A reader that captured a view goes on reading it while it holds it. The
// checkpoint that replaces it may compact another empty, but reclamation must
// leave that one for one checkpoint: the reader's entries still name it, and
// deleting it turns a healthy VM's read into an I/O error.
func TestReclamationDefersPacksCompactionEmptied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "grace")
		first := store.Begin(root, control.Ref{VM: "grace", Sequence: 2})
		for page := range uint64(checkpointPages) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		p := store.Begin(second, control.Ref{VM: "grace", Sequence: 3})
		write(p, m, "third", 0)
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		// The view a reader captured before the next checkpoint was published,
		// and the bytes it expects to go on reading through it.
		view, held := third, m.clone()

		q := store.Begin(third, control.Ref{VM: "grace", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), third, fourth, nil); err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, view, held)

		// One checkpoint later nothing holds the replaced view, and the
		// checkpoint compaction emptied goes.
		r := store.Begin(fourth, control.Ref{VM: "grace", Sequence: 5})
		write(r, m, "fifth", 0)
		fifth, err := r.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), fourth, fifth, nil); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, objects, "grace", 2); got != 0 {
			t.Fatalf("the emptied checkpoint kept %d objects a checkpoint after its grace", got)
		}
		checkRead(t, store, fifth, m)
	})
}

// Reclamation works from the index it is handed, which after a takeover is one
// read back from the store rather than the one Commit returned. The checkpoints
// a compaction emptied must therefore survive the round trip: a root that
// publishes only what it reads loses their grace, and the next sweep deletes a
// checkpoint the view it replaced still reads.
func TestEmptiedPackGraceSurvivesTheIndexObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "roundtrip")
		first := store.Begin(root, control.Ref{VM: "roundtrip", Sequence: 2})
		for page := range uint64(checkpointPages) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		p := store.Begin(second, control.Ref{VM: "roundtrip", Sequence: 3})
		write(p, m, "third", 0)
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		view, held := third, m.clone()

		q := store.Begin(third, control.Ref{VM: "roundtrip", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(fourth.Checkpoints(), second.Ref()) {
			t.Fatalf("the fixture expected sequence 2 to be compacted empty: %v", fourth.Checkpoints())
		}
		reopened, err := store.Open(t.Context(), fourth.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), third, reopened, nil); err != nil {
			t.Fatal(err)
		}
		if !openable(t, objects, "roundtrip", 2) {
			t.Fatal("the emptied checkpoint was swept before the grace it owes the replaced view")
		}
		checkRead(t, store, view, held)
	})
}
