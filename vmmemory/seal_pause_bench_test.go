package vmmemory_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// What a capture's pause costs a processor at 4 KiB, which is the seal's
// write-protect commands, and what the walk behind it costs, which is the pages.
// On GCE a capture of 2,204,672 sealed pages in 2,264 runs paused 2.14 s: 0.18 s
// of commands and 1.97 s of walk (docs/vm-memory.md). This is a smaller
// capture of the same shape, a run of about a thousand pages per command.

const (
	// sealRuns is how many runs of dirty pages the capture seals, sealRun how
	// many pages each holds, and sealStride how far each starts from the last:
	// the gap between them is wider than any rule copies across, so each run
	// is a write-protect command of its own.
	sealRuns   = 32
	sealRun    = 1024
	sealStride = benchPages / sealRuns
)

// BenchmarkA4KiBCapturePause seals one memory region holding sealRuns runs of
// dirty pages, an iteration a seal. The timed part is the pause, and
// walk-ns/op is the walk behind it. Each iteration abandons its checkpoint,
// which gives every page back dirty, so the next seals the same set.
func BenchmarkA4KiBCapturePause(b *testing.B) {
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
	pager := newConfiguredBenchPager(b, ctx, runtime, &locatedBacking{index: index, pages: benchPages}, 1,
		// Each page the guest stored into is its copy and the page it was
		// copied from, which stays resident for the settle to compare.
		vmmemory.Config{ResidentPages: 2 * benchPages, LogicalPages: 2 * benchPages, DirtyPages: benchPages,
			ReadAheadPages: 1, WriteAheadPages: 1})
	for run := range uint64(sealRuns) {
		for page := run * sealStride; page < run*sealStride+sealRun; page++ {
			if err := pager.region.Fault(ctx, page, true); err != nil {
				b.Fatal(err)
			}
			// The store changes the page: a copy the guest left as it was is a
			// cold copy, which no checkpoint takes.
			mapped, _ := pager.mapping.mappedPage(page)
			pager.mapping.arena.pageUnder(mapped.place)[0] ^= 0xff
		}
	}
	var walk time.Duration
	b.ReportAllocs()
	for b.Loop() {
		if err := pager.region.Seal(ctx); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		began := time.Now()
		ckpt := pager.region.Checkpoint()
		if got := len(ckpt.DirtyPages()); got != sealRuns*sealRun {
			b.Fatalf("the seal took %d pages, want %d", got, sealRuns*sealRun)
		}
		walk += time.Since(began)
		if err := ckpt.Retire(ctx, false); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(walk.Nanoseconds())/float64(b.N), "walk-ns/op")
	pager.close(b, ctx)
}
