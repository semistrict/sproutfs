package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A read through a pager faults each page in where the pager does not map
// it. With the pager as built, a fault reads its page and its run is
// prefetched behind it; with every fault reading its run first, as before
// 2026-10-04, there are no prefetches and each fault reads its whole run.
// Either way every page reads back the guest's bytes.
func TestAReadThroughAPagerFaultsItsPagesIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		nodes := simNodes(t, ctx, runtime, nil)
		specs, err := parseCases("4KiB/chain/fault/1,4KiB/chain/runfirst/1,2MiB/chain/fault/1,2MiB/chain/runfirst/1," +
			"4KiB/sequential/fault/1,4KiB/sequential/runfirst/1")
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := drive(ctx, nodes, driveConfig{
			pages: map[uint64]uint64{checkpoint.PageSize2MiB: 16, checkpoint.PageSize4KiB: 4096},
			code:  "4+2", rounds: 1, cases: specs, sources: []string{sourceCluster, sourceStore}, reads: 8,
			runReads: 4, lost: 3, loseAfter: time.Second, cleared: 20 * time.Second, seed: 1, calibrate: 0})
		if err != nil {
			t.Fatal(err)
		}
		// Whether a hop of a chain lands on a page a prefetch is still
		// reading is how quickly the simulated sources answered, so a chain
		// read page first states what it loaded and not how many faults it
		// took or how many of them waited.
		var got []string
		for _, c := range result.Cases {
			pager := *c.Pager
			if c.Pattern == patternChain && c.Unit == unitFault {
				pager.Faults, pager.PrefetchWaits = 0, 0
			}
			got = append(got, fmt.Sprintf("%s %s: %d reads, wrong %d, failed %d, %+v",
				c.Case, c.Source, c.Reads, c.Wrong, c.Failed, pager))
		}
		slices.Sort(got)
		want := []string{
			"2MiB/chain/fault/1 cluster: 4 reads, wrong 0, failed 0, {Faults:0 Loads:7 LoadedPages:12 Prefetches:3 PrefetchedPages:8 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:1 Evictions:0}",
			"2MiB/chain/fault/1 store: 4 reads, wrong 0, failed 0, {Faults:0 Loads:5 LoadedPages:9 Prefetches:2 PrefetchedPages:6 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:1 Evictions:0}",
			"2MiB/chain/runfirst/1 cluster: 4 reads, wrong 0, failed 0, {Faults:4 Loads:4 LoadedPages:16 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"2MiB/chain/runfirst/1 store: 4 reads, wrong 0, failed 0, {Faults:3 Loads:3 LoadedPages:12 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/chain/fault/1 cluster: 4 reads, wrong 0, failed 0, {Faults:0 Loads:4 LoadedPages:4096 Prefetches:2 PrefetchedPages:4094 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/chain/fault/1 store: 4 reads, wrong 0, failed 0, {Faults:0 Loads:4 LoadedPages:4096 Prefetches:2 PrefetchedPages:4094 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/chain/runfirst/1 cluster: 4 reads, wrong 0, failed 0, {Faults:1 Loads:1 LoadedPages:2048 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/chain/runfirst/1 store: 4 reads, wrong 0, failed 0, {Faults:2 Loads:2 LoadedPages:4096 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/sequential/fault/1 cluster: 4096 reads, wrong 0, failed 0, {Faults:4 Loads:4 LoadedPages:4096 Prefetches:2 PrefetchedPages:4094 PrefetchWaits:2 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/sequential/fault/1 store: 4096 reads, wrong 0, failed 0, {Faults:4 Loads:4 LoadedPages:4096 Prefetches:2 PrefetchedPages:4094 PrefetchWaits:2 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/sequential/runfirst/1 cluster: 4096 reads, wrong 0, failed 0, {Faults:2 Loads:2 LoadedPages:4096 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
			"4KiB/sequential/runfirst/1 store: 4096 reads, wrong 0, failed 0, {Faults:2 Loads:2 LoadedPages:4096 Prefetches:0 PrefetchedPages:0 PrefetchWaits:0 PrefetchRefused:0 PrefetchRandom:0 Evictions:0}",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the reads through a pager were\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
