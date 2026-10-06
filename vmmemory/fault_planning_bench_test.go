package vmmemory_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// What a fault costs a processor to plan, at 4 KiB with the host's read-ahead
// run of 2,048 pages, over a real checkpoint index whose page tables are
// decoded and held. The backing locates through the index and makes up the
// bytes of a read, so what a fault costs here is its planning and the pager's
// own work, not a read.

const (
	// benchWindow is the host's read-ahead run at 4 KiB, 8 MiB, and
	// benchWindows how many the volume has: three segments' worth, enough
	// that a fault in each window in turn, downwards, follows none of the
	// eight before it.
	benchWindow  = 8 << 20 / checkpoint.PageSize4KiB
	benchWindows = 24
	benchPages   = benchWindows * benchWindow
)

// locatedBacking locates a checkpoint's volume through its index and makes up
// what a read of it holds. A prefetch's read waits until the benchmark is
// over, so the first one holds the pager's one prefetch and every fault after
// it plans its window as a fault reading forwards does, and then reads its
// page alone.
type locatedBacking struct {
	index *checkpoint.Index
	pages uint64
}

func (b *locatedBacking) Size() uint64                 { return b.pages * checkpoint.PageSize4KiB }
func (b *locatedBacking) PageSize() uint64             { return checkpoint.PageSize4KiB }
func (b *locatedBacking) Verify(context.Context) error { return nil }

func (b *locatedBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	return b.LoadPages(ctx, offset, dst, nil)
}

func (b *locatedBacking) LoadPages(ctx context.Context, offset uint64, dst []byte, _ []bool) error {
	if checkpoint.Prefetching(ctx) {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	for at := 0; at < len(dst); at += checkpoint.PageSize4KiB {
		binary.LittleEndian.PutUint64(dst[at:], (offset+uint64(at))/checkpoint.PageSize4KiB+1)
	}
	return nil
}

func (b *locatedBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	return b.index.Locate(ctx, "ram", offset, length)
}

// benchPager is one host of a benchmark and the memory region it attaches.
type benchPager struct {
	host   *vmmemory.Host
	region *vmmemory.MemoryRegion
	// mapping is what the region's guest maps, where a store writes.
	mapping *mapping
	spill   platform.File
}

func newBenchPager(b *testing.B, ctx context.Context, runtime *sim.Runtime, backing vmmemory.Backing,
	number int) *benchPager {
	b.Helper()
	return newConfiguredBenchPager(b, ctx, runtime, backing, number, vmmemory.Config{
		ResidentPages: benchPages, LogicalPages: benchPages, DirtyPages: benchWindow, ReadAheadPages: benchWindow,
		PrefetchRuns: 1})
}

// newConfiguredBenchPager is a benchmark's 4 KiB pager as cfg shapes it.
func newConfiguredBenchPager(b *testing.B, ctx context.Context, runtime *sim.Runtime, backing vmmemory.Backing,
	number int, cfg vmmemory.Config) *benchPager {
	b.Helper()
	spill, err := runtime.NewDisk(fmt.Sprintf("pager-%d", number), sim.DiskConfig{}).Open(ctx, "spill",
		platform.OpenOptions{Create: true})
	if err != nil {
		b.Fatal(err)
	}
	cfg.PageSize = checkpoint.PageSize4KiB
	a := newArena(checkpoint.PageSize4KiB)
	host, err := vmmemory.New(ctx, testresource.New(), cfg, a, spill)
	if err != nil {
		b.Fatal(err)
	}
	m := newMapping(a)
	region, err := host.Attach(ctx, ram(backing), m)
	if err != nil {
		b.Fatal(err)
	}
	return &benchPager{host: host, region: region, mapping: m, spill: spill}
}

// close detaches the memory region, which cancels the prefetch held reading,
// and closes the pager.
func (p *benchPager) close(b *testing.B, ctx context.Context) {
	b.Helper()
	if err := p.region.Detach(ctx); err != nil {
		b.Fatal(err)
	}
	if err := p.host.Close(ctx); err != nil {
		b.Fatal(err)
	}
	if err := p.spill.Close(); err != nil {
		b.Fatal(err)
	}
}

// benchFaults runs one fault an iteration on pagers of their own, each of
// which takes faults faults, the hop'th at page(hop), each page once. Each
// pager first takes a fault at first, which prefetches as a memory region's
// first fault does, holds the pager's one prefetch, and is left out of the
// timing. random(hops) is how many of a pager's first hops read their page
// alone, as a fault at random does.
func benchFaults(b *testing.B, first uint64, faults int, page func(hop int) uint64,
	random func(faults int) uint64) {
	if pageSize != checkpoint.PageSize4KiB {
		b.Skip("the benchmark builds a 4 KiB pager of its own, once")
	}
	runtime := sim.New(sim.Config{})
	ctx := sim.WithRuntime(b.Context(), runtime)
	chain := newChainStore(b, ctx, runtime, benchPages)
	index, err := chain.store.Open(ctx, chain.ref)
	if err != nil {
		b.Fatal(err)
	}
	backing := &locatedBacking{index: index, pages: benchPages}
	var pager *benchPager
	hop, pagers := faults, 0
	b.ReportAllocs()
	for b.Loop() {
		if hop == faults {
			b.StopTimer()
			if pager != nil {
				pager.close(b, ctx)
			}
			pagers++
			pager = newBenchPager(b, ctx, runtime, backing, pagers)
			if err := pager.region.Fault(ctx, first, false); err != nil {
				b.Fatal(err)
			}
			hop = 0
			b.StartTimer()
		}
		if err := pager.region.Fault(ctx, page(hop), false); err != nil {
			b.Fatal(err)
		}
		hop++
	}
	b.StopTimer()
	stats, err := pager.host.Stats(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if stats.Loads != uint64(hop)+1 || stats.Prefetches != 1 || stats.PrefetchRandom != random(hop) {
		b.Fatalf("the last pager read %d times, prefetched %d times and read %d pages alone at random, "+
			"want %d, 1 and %d", stats.Loads, stats.Prefetches, stats.PrefetchRandom, hop+1, random(hop))
	}
	pager.close(b, ctx)
}

// permuted is the page of a window the hop's turn there faults: 1,031 is odd,
// so its multiples modulo the window's power of two visit every page once.
func permuted(turn int) uint64 { return uint64(turn) * 1031 % benchWindow }

// BenchmarkARandom4KiBFault is a fault at random: a guest that follows
// pointers through 4 KiB pages, each fault in a window none of the eight
// faults before it touched, which reads its page alone. It faults the windows
// below the last, where the held prefetch is, in turn, downwards, a page of
// each at a time.
func BenchmarkARandom4KiBFault(b *testing.B) {
	const windows = benchWindows - 1
	benchFaults(b, benchPages-1, windows*benchWindow, func(hop int) uint64 {
		return uint64(windows-1-hop%windows)*benchWindow + permuted(hop/windows)
	}, func(faults int) uint64 { return uint64(faults) })
}

// BenchmarkAForward4KiBFault is a fault reading forwards: each fault in the
// window of the one before it or the window after, which plans the rest of its
// window to prefetch it. The pager's one prefetch is held, so each reads its
// page alone once it has planned its window. It faults every page of each
// window above the first, where the held prefetch is, in turn, upwards.
func BenchmarkAForward4KiBFault(b *testing.B) {
	const windows = benchWindows - 1
	benchFaults(b, 0, windows*benchWindow, func(hop int) uint64 {
		return uint64(1+hop/benchWindow)*benchWindow + permuted(hop%benchWindow)
	}, func(int) uint64 { return 0 })
}
