package checkpoint_test

import (
	"context"
	"encoding/binary"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// A segment's page table is decoded once and kept in the page cache's memory
// under the segment's identity, charged to its budget and evicted least
// recently used with the pages (checkpoint/pagetable.go). These price the
// decoding, so a simulation counts it and sees it take time.

const (
	// tablePages is how many pages of a 4 KiB-page volume one segment
	// covers, and tableSegments how many segments the volumes here have.
	tablePages    = 16 << 10
	tableSegments = 4
	// tableDecodeRate prices decoding at a byte a microsecond.
	tableDecodeRate = 1_000_000
)

// tableSource publishes a page naming itself in its first bytes.
type tableSource struct{}

func (tableSource) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	clear(dst)
	binary.LittleEndian.PutUint64(dst, page+1)
	return nil
}

// tableWorld is a store over a runtime that prices decoding a segment,
// reading through a cache of the given budget, and the checkpoint it
// published: a 4 KiB-page volume of tableSegments segments, the same eight
// pages of each published.
type tableWorld struct {
	runtime *sim.Runtime
	ctx     context.Context
	store   *checkpoint.Store
	cache   *checkpoint.Cache
	budget  *resource.Budget
	root    *checkpoint.Index
	ref     control.Ref
	// base is what the cache had done when the world was made: the tables
	// the publication kept, and the clear that dropped them.
	base checkpoint.CacheStats
}

// tablePagesOf is the pages of segment number the checkpoint publishes.
func tablePagesOf(number uint64) []uint64 {
	var pages []uint64
	for page := range uint64(8) {
		pages = append(pages, number*tablePages+page*3)
	}
	return pages
}

func newTableWorld(t *testing.T, budget int64) *tableWorld {
	t.Helper()
	runtime := sim.New(sim.Config{Compute: map[string]int64{checkpoint.WorkPageTable: tableDecodeRate}})
	ctx := sim.WithRuntime(t.Context(), runtime)
	limit, err := resource.New(budget)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(ctx, limit, checkpoint.CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	store := mustStore(t, checkpoint.Config{ObjectStore: runtime.ObjectStore(), Cache: cache})
	root, err := store.Root(ctx, control.Ref{VM: "tables", Sequence: 1},
		volumes4KiB(map[string]uint64{"ram": tableSegments * tablePages * checkpoint.PageSize4KiB}))
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "tables", Sequence: 2}
	publication := store.Begin(root, ref)
	for number := range uint64(tableSegments) {
		for _, page := range tablePagesOf(number) {
			publication.Dirty("ram", page)
		}
	}
	published, err := publication.Commit(ctx, tableSource{})
	if err != nil {
		t.Fatal(err)
	}
	// The publication left the tables it wrote in the cache; the readers here
	// are a host that did not publish the checkpoint.
	cache.Clear()
	return &tableWorld{runtime: runtime, ctx: ctx, store: store, cache: cache, budget: limit, root: published, ref: ref,
		base: cache.Stats()}
}

// since is what the cache has done since the world was made.
func (w *tableWorld) since() checkpoint.CacheStats {
	stats := w.cache.Stats()
	stats.Evictions -= w.base.Evictions
	stats.Tables.Kept -= w.base.Tables.Kept
	return stats
}

// open is an index on the checkpoint of its own, as a restore opens it.
func (w *tableWorld) open(t *testing.T) *checkpoint.Index {
	t.Helper()
	index, err := w.store.Open(w.ctx, w.ref)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// identityOf is the identity index gives one page of the volume.
func (w *tableWorld) identityOf(t *testing.T, index *checkpoint.Index, page uint64) control.Identity {
	t.Helper()
	extents, err := index.Locate(w.ctx, "ram", page*checkpoint.PageSize4KiB, checkpoint.PageSize4KiB)
	if err != nil {
		t.Fatal(err)
	}
	return extents[0].Identity
}

// decodes is how many segments have been decoded.
func (w *tableWorld) decodes() uint64 { return w.runtime.Work(checkpoint.WorkPageTable).Pieces }

// A segment is decoded once while the cache keeps its table, however many
// indexes read it and however often. Two restores of one checkpoint and a
// child of it that rewrote one segment look up both segments' pages again and
// again: the first lookup of each segment fetches and decodes it, the child's
// publication left the table it wrote, and every other lookup takes no time.
// Each index finds its own checkpoint's pages: the tables are keyed by the
// checkpoint that wrote each segment, so the child's never answers for its
// parent. Decoding for every lookup (checkpoint-decode-every-lookup) decodes
// for each of them.
func TestASegmentIsDecodedOncePerCheckpointWhileCached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newTableWorld(t, 64<<20)
		first, second := w.open(t), w.open(t)
		childRef := control.Ref{VM: "tables-child", Sequence: 1}
		publication := w.store.Begin(first, childRef)
		rewritten := tablePagesOf(1)[0] + 1
		publication.Dirty("ram", rewritten)
		child, err := publication.Commit(w.ctx, tableSource{})
		if err != nil {
			t.Fatal(err)
		}
		if decodes := w.decodes(); decodes != 1 {
			t.Fatalf("the child's publication decoded %d segments, want the one it rewrote", decodes)
		}
		inherited := tablePagesOf(0)[1]
		for round := range 3 {
			began := time.Now()
			for _, index := range []*checkpoint.Index{first, second, child} {
				if got, want := w.identityOf(t, index, inherited),
					(control.Identity{Ref: w.ref, Volume: "ram", Page: inherited}); got != want {
					t.Fatalf("round %d: page %d has the identity %v, want %v", round, inherited, got, want)
				}
				want := control.ZeroIdentity
				if index == child {
					want = control.Identity{Ref: childRef, Volume: "ram", Page: rewritten}
				}
				if got := w.identityOf(t, index, rewritten); got != want {
					t.Fatalf("round %d: page %d has the identity %v, want %v", round, rewritten, got, want)
				}
			}
			if took := time.Since(began); round > 0 && took != 0 {
				t.Fatalf("round %d of lookups took %v, want no time: every table is held", round, took)
			}
		}
		if decodes := w.decodes(); decodes != 2 {
			t.Fatalf("the segments were decoded %d times, want each of the two looked up once", decodes)
		}
		if tables := w.since().Tables; tables.Loads != 2 || tables.Kept != 1 || tables.Entries != 3 {
			t.Fatalf("the cache's tables %+v, want two loaded, the child's kept, and the three held", tables)
		}
	})
}

// The tables are bounded by the cache's budget and evicted least recently used.
// A budget of two and a half tables holds two: a third evicts the one looked
// up least recently, a lookup of a held one costs nothing, and a lookup of an
// evicted one fetches and decodes it again. What the cache holds is never more
// than the budget.
func TestThePageTableCacheStaysWithinItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Every segment has the same pages of the same checkpoint, so every
		// table is charged the same; an unbounded cache says how much.
		measure := newTableWorld(t, 64<<20)
		measure.identityOf(t, measure.open(t), 0)
		charge := measure.cache.Stats().Tables.Bytes
		if charge == 0 {
			t.Fatal("a table is charged nothing")
		}
		w := newTableWorld(t, 2*charge+charge/2)
		index := w.open(t)
		check := func(step string, loads, hits, entries, evictions uint64) {
			t.Helper()
			stats := w.since()
			tables := stats.Tables
			if tables.Loads != loads || tables.Hits != hits || uint64(tables.Entries) != entries ||
				stats.Evictions != evictions || tables.Bytes != int64(entries)*charge {
				t.Fatalf("%s: the cache %+v, want %d loads, %d hits, %d tables held and %d evictions",
					step, stats, loads, hits, entries, evictions)
			}
			if used := w.budget.Stats().Used; used != stats.ResidentBytes || used > 2*charge+charge/2 {
				t.Fatalf("%s: the budget holds %d bytes, the cache %d, past the bound of %d",
					step, used, stats.ResidentBytes, 2*charge+charge/2)
			}
			if decodes := w.decodes(); decodes != loads {
				t.Fatalf("%s: %d decodes, want one a load, %d", step, decodes, loads)
			}
		}
		lookup := func(number uint64) { w.identityOf(t, index, tablePagesOf(number)[0]) }
		lookup(0)
		lookup(1)
		check("two segments", 2, 0, 2, 0)
		lookup(2)
		check("a third evicts the first", 3, 0, 2, 1)
		lookup(1)
		check("the second is held", 3, 1, 2, 1)
		lookup(0)
		check("the first is decoded again and evicts the third", 4, 1, 2, 2)
		lookup(1)
		lookup(0)
		check("the two held", 4, 3, 2, 2)
	})
}
