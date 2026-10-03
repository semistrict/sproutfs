package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// TestCompactionReadsTheRescuedPagesAsExtents compacts a checkpoint of 1,024
// adjacent 4 KiB pages after its successor overwrote the first 600 of them.
// The 424 pages compaction rescues lie next to each other in one part, so
// fetching them is one ranged read, as a read of the same run would be, and
// nothing at all once the page cache holds them.
func TestCompactionReadsTheRescuedPagesAsExtents(t *testing.T) {
	for _, test := range []struct {
		name   string
		cached bool
		gets   int64
	}{
		{name: "uncached", gets: 1},
		{name: "cached", cached: true, gets: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				objects := &countingStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
				config := checkpoint.Config{ObjectStore: objects}
				if test.cached {
					budget, err := resource.New(64 << 20)
					if err != nil {
						t.Fatal(err)
					}
					cache, err := checkpoint.NewCache(t.Context(), budget, checkpoint.CacheConfig{})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(cache.Close)
					config.Cache = cache
				}
				store := mustStore(t, config)
				specs := volumes4KiB(map[string]uint64{"ram0": 8 << 20})
				root, err := store.Root(t.Context(), control.Ref{VM: "rescue", Sequence: 1}, specs)
				if err != nil {
					t.Fatal(err)
				}
				m := newModel(specs)
				written := control.Ref{VM: "rescue", Sequence: 2}
				p := store.Begin(root, written)
				for page := range uint64(1024) {
					m.dirty(p, "ram0", page, 0, sectorData("written", page, 0))
				}
				second, err := p.Commit(t.Context(), m)
				if err != nil {
					t.Fatal(err)
				}
				if test.cached {
					// A reader of the pages that survive leaves them in the cache.
					got := make([]byte, 424*checkpoint.PageSize4KiB)
					if err := store.Read(t.Context(), second, "ram0", 600*checkpoint.PageSize4KiB, got); err != nil {
						t.Fatal(err)
					}
				}
				p = store.Begin(second, control.Ref{VM: "rescue", Sequence: 3})
				for page := range uint64(600) {
					m.dirty(p, "ram0", page, 0, sectorData("overwritten", page, 0))
				}
				objects.gets.Store(0)
				third, err := p.Commit(t.Context(), m)
				if err != nil {
					t.Fatal(err)
				}
				if got := objects.gets.Load(); got != test.gets {
					t.Fatalf("compacting 424 adjacent pages made %d GETs, want %d", got, test.gets)
				}
				if slices.Contains(third.Checkpoints(), written) {
					t.Fatalf("%v was not compacted: the successor still reads it (%v)", written, third.Checkpoints())
				}
				reopened, err := store.Open(t.Context(), third.Ref())
				if err != nil {
					t.Fatal(err)
				}
				checkRead(t, store, reopened, m)
			})
		})
	}
}
