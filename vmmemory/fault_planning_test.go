package vmmemory_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A fault plans its own page first and the rest of its window behind its read
// (faultfirst.go), and a segment's page table is decoded once for every index
// that reads it while the page cache keeps it (checkpoint/pagetable.go). These
// run a pager at 4 KiB over a real checkpoint store whose reads take virtual
// time, with planning and decoding priced, as a processor costs them.

const (
	// chainPages is one whole segment of a 4 KiB-page volume, and chainWindow
	// the read-ahead run these fault: 2 MiB, 32 windows of the segment.
	chainPages  = 16 << 10
	chainWindow = checkpoint.PageSize2MiB / checkpoint.PageSize4KiB
	// chainGet is what a read of the store takes, whatever it carries.
	chainGet = time.Millisecond
	// planRate prices planning at a microsecond a page: a page alone is
	// planPage, and a window, half a read, is planWindow.
	planRate   = 1_000_000
	planPage   = time.Microsecond
	planWindow = chainWindow * planPage
	// tableRate prices decoding a segment at a byte a microsecond.
	tableRate = 1_000_000
)

// chainSource publishes every page of the segment, each naming itself in its
// first bytes.
type chainSource struct{}

func (chainSource) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	clear(dst)
	binary.LittleEndian.PutUint64(dst, page+1)
	return nil
}

// chainStore is a store over the runtime's object store, reading through a
// page cache, holding one checkpoint of one 4 KiB-page volume of chainPages
// pages, all published.
type chainStore struct {
	store *checkpoint.Store
	cache *checkpoint.Cache
	ref   control.Ref
}

func newChainStore(t *testing.T, ctx context.Context, runtime *sim.Runtime) *chainStore {
	t.Helper()
	cache, err := checkpoint.NewCache(ctx, testresource.New(), checkpoint.CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	prefix, err := platform.NewObjectPrefix("chain")
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix,
		Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	ref := control.Ref{VM: "chain", Sequence: 1}
	root, err := store.Root(ctx, ref, map[string]checkpoint.VolumeSpec{
		"ram": {Size: chainPages * checkpoint.PageSize4KiB, PageSize: checkpoint.PageSize4KiB}})
	if err != nil {
		t.Fatal(err)
	}
	ref.Sequence++
	publication := store.Begin(root, ref)
	for page := range uint64(chainPages) {
		publication.Dirty("ram", page)
	}
	if _, err := publication.Commit(ctx, chainSource{}); err != nil {
		t.Fatal(err)
	}
	// The publication left the segment's table in the cache. The pager reads
	// as a host that restores the checkpoint, which holds none of it.
	cache.Clear()
	return &chainStore{store: store, cache: cache, ref: ref}
}

// chainBacking is the checkpoint's volume as a pager's backing, opened by
// itself, reading through the store as *volume.Volume does.
type chainBacking struct {
	store *checkpoint.Store
	index *checkpoint.Index
}

func (c *chainStore) backing(t *testing.T, ctx context.Context) *chainBacking {
	t.Helper()
	index, err := c.store.Open(ctx, c.ref)
	if err != nil {
		t.Fatal(err)
	}
	return &chainBacking{store: c.store, index: index}
}

func (b *chainBacking) Size() uint64                 { return chainPages * checkpoint.PageSize4KiB }
func (b *chainBacking) PageSize() uint64             { return checkpoint.PageSize4KiB }
func (b *chainBacking) Verify(context.Context) error { return nil }

func (b *chainBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	return b.store.Read(ctx, b.index, "ram", offset, dst)
}

func (b *chainBacking) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	return b.store.ReadPages(ctx, b.index, "ram", offset, dst, wanted)
}

func (b *chainBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	return b.index.Locate(ctx, "ram", offset, length)
}

// chainRuntime is a world whose reads of the store take chainGet whatever
// they carry, and which prices planning and decoding.
func chainRuntime() *sim.Runtime {
	return sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{GetLatency: chainGet, BytesPerSecond: 1 << 50},
		Compute: map[string]int64{vmmemory.WorkPlan: planRate, checkpoint.WorkPageTable: tableRate}})
}

// requireChainPage requires a page to be mapped and to hold what the
// checkpoint published for it.
func requireChainPage(t *testing.T, m *mapping, page uint64) {
	t.Helper()
	p, ok := m.mappedPage(page)
	if !ok {
		t.Fatalf("page %d is not mapped", page)
	}
	if got := binary.LittleEndian.Uint64(m.arena.pageUnder(p.place)); got != page+1 {
		t.Fatalf("page %d holds page %d's bytes", page, got-1)
	}
}

// A guest that follows pointers through 4 KiB pages faults once a hop, each
// fault in a window of its own and none in the window after a recent one, so
// none prefetches but the first. Each hop costs exactly one read of the store
// and the planning of its own page: the fault plans the 512 pages of its
// window while its read is under way, and the segment that locates them is
// decoded once, by the first lookup, for every hop after. Planning the window
// before the read (pager-plan-the-window-first) adds the window's planning to
// every hop; decoding the segment for every lookup
// (checkpoint-decode-every-lookup) adds a read of the segment and its decode.
// A store into a page the memory region holds nothing for asks the volume
// about the page twice, whether it is a hole and then for its read, and costs
// the same read.
func TestADependentChainOf4KiBFaultsPaysOnePageReadAHop(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := chainRuntime()
				ctx := sim.WithRuntime(t.Context(), runtime)
				chain := newChainStore(t, ctx, runtime)
				f, err := newFixtureOn(t, ctx, runtime.NewDisk("pager", sim.DiskConfig{}), vmmemory.Config{
					PageSize: checkpoint.PageSize4KiB, ResidentPages: 4 * chainWindow, LogicalPages: chainPages,
					DirtyPages: chainWindow, ReadAheadPages: chainWindow, WriteAheadPages: 1, PrefetchRuns: 16})
				if err != nil {
					t.Fatal(err)
				}
				r, m := f.attach(chain.backing(t, ctx))
				// A hop that fails leaves the first hop's prefetch to settle
				// before the memory region is taken down.
				t.Cleanup(func() { _ = r.SettlePrefetches(context.Background()) })
				// Windows 30, 1, 20, 9, 25, 5, 15 and 11 of 32.
				hops := []uint64{30*chainWindow + 77, chainWindow + 3, 20*chainWindow + 500, 9*chainWindow + 1,
					25*chainWindow + 256, 5*chainWindow + 9, 15*chainWindow + 511, 11*chainWindow + 128}
				plans := uint64(1)
				if write {
					plans = 2
				}
				hop := chainGet + time.Duration(plans)*planPage
				for at, page := range hops {
					started := time.Now()
					if err := r.Fault(ctx, page, write); err != nil {
						t.Fatal(err)
					}
					took := time.Since(started)
					if at > 0 && took != hop {
						t.Fatalf("hop %d, to page %d, took %v, want %v: one read and its page's planning",
							at, page, took, hop)
					}
					if at == 0 && took < 2*chainGet {
						t.Fatalf("the first hop took %v, want at least the reads of the segment and the page", took)
					}
					requireChainPage(t, m, page)
				}
				if decodes := runtime.Work(checkpoint.WorkPageTable).Pieces; decodes != 1 {
					t.Fatalf("the segment was decoded %d times, want once", decodes)
				}
				if err := r.SettlePrefetches(ctx); err != nil {
					t.Fatal(err)
				}
				s := hostStats(t, f)
				if s.Prefetches != 1 || s.PrefetchRandom != uint64(len(hops)-1) {
					t.Fatalf("prefetches %d, runs left unread %d; want the first hop's alone, and %d",
						s.Prefetches, s.PrefetchRandom, len(hops)-1)
				}
				if tables := chain.cache.Stats().Tables; tables.Loads != 1 || tables.Entries != 1 {
					t.Fatalf("the cache's page tables %+v, want the segment's loaded once and held", tables)
				}
			})
		})
	}
}

// errWindowUnlocated is what windowFailing answers a lookup of more than one
// page with.
var errWindowUnlocated = errors.New("the window could not be located")

// windowFailing is a backing that locates a page alone and fails to locate
// anything longer, while fail is set, and fails every read while unreadable
// is.
type windowFailing struct {
	*slowBacking
	fail, unreadable bool
}

// errUnreadable is what windowFailing answers a read with.
var errUnreadable = errors.New("the page could not be read")

func (b *windowFailing) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	if b.unreadable {
		return errUnreadable
	}
	return b.slowBacking.LoadPages(ctx, offset, dst, wanted)
}

func (b *windowFailing) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if b.fail && length > uint64(b.pageSize) {
		return nil, errWindowUnlocated
	}
	return b.slowBacking.Locate(ctx, offset, length)
}

// A fault that took its page and the run of slots for its window, and started
// its page's read, and then could not locate the rest of the window, fails
// whole: its read is abandoned, and every slot it took goes back, the
// window's run with its page's. The next fault of the window, once the window
// can be located, reads the page and prefetches the run as if nothing had
// happened.
func TestAFaultThatCannotLocateItsWindowGivesBackEverySlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := &windowFailing{slowBacking: f.slowBacking(16), fail: true}
		r, m := f.attach(b)
		if err := r.Fault(f.ctx, 3, false); !errors.Is(err, errWindowUnlocated) {
			t.Fatalf("the fault returned %v, want %v", err, errWindowUnlocated)
		}
		if s := hostStats(t, f); s.ResidentPages != 0 || s.Loads != 0 || s.Prefetches != 0 {
			t.Fatalf("the failed fault left %d pages held, %d loads and %d prefetches, want none",
				s.ResidentPages, s.Loads, s.Prefetches)
		}
		// A fault whose own page's read fails fails with it, and holds nothing
		// after either.
		b.fail, b.unreadable = false, true
		if err := r.Fault(f.ctx, 3, false); !errors.Is(err, errUnreadable) {
			t.Fatalf("the fault returned %v, want %v", err, errUnreadable)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); s.ResidentPages != 0 || s.Loads != 0 {
			t.Fatalf("the fault whose read failed left %d pages held and %d loads, want none", s.ResidentPages, s.Loads)
		}
		if _, mapped := m.mappedPage(3); mapped {
			t.Fatal("the fault whose read failed mapped its page")
		}
		b.unreadable = false
		if err := r.Fault(f.ctx, 3, false); err != nil {
			t.Fatal(err)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(8) {
			requirePage(t, m, page)
		}
		if s := hostStats(t, f); s.ResidentPages != 8 || s.Loads != 2 || s.Prefetches != 2 {
			t.Fatalf("the fault after left %d pages held, %d loads and %d prefetches, want 8, 2 and 2, "+
				"the failed fault's among them", s.ResidentPages, s.Loads, s.Prefetches)
		}
	})
}
