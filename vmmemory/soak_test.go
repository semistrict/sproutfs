package vmmemory_test

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

const (
	// soakForks is how many forks of one checkpoint each prefetch world of
	// the soak starts at once, and soakChildren how many children of one fork
	// point each fork world does: an embedder's boot starts several VMs from
	// one cold template on one host together.
	soakForks    = 8
	soakChildren = 6
	// soakVCPUs is how many vCPUs each guest of a prefetch or rules world
	// runs.
	soakVCPUs = 2
)

// The pager's campaigns soak without a scheduler: the same worlds the seeded
// campaigns run, with more guests, on real goroutines, real timers and as many
// cores as the run has, for as long as SPROUTFS_PAGER_SOAK says. A seeded
// campaign lets one task go on at a time, at its admission points, so a race
// between two of them is found only where an admission point sits in it. Here
// nothing orders them, so every lock gap is open to every interleaving the
// machine makes, and under -race every access is checked too. A seed still
// fixes each world's operations and the fault sites it activates, but not the
// order its guests run in, so a failure names its seed without replaying.
//
// SPROUTFS_PAGER_SOAK_SEED is the first seed, and the clock's when it is unset.
// SPROUTFS_PAGER_SOAK_WORLDS names the worlds to soak, of prefetch, fork and
// rules, separated by commas; all three when it is unset.
// Run it as docs/testing.md says, on a machine with many cores.
func TestThePagersCampaignsSoakWithoutAScheduler(t *testing.T) {
	setting := os.Getenv("SPROUTFS_PAGER_SOAK")
	if setting == "" {
		t.Skip("set SPROUTFS_PAGER_SOAK to a duration to soak the pager's campaigns")
	}
	length, err := time.ParseDuration(setting)
	if err != nil {
		t.Fatalf("SPROUTFS_PAGER_SOAK=%q: %v", setting, err)
	}
	first := uint64(time.Now().UnixNano())
	if setting := os.Getenv("SPROUTFS_PAGER_SOAK_SEED"); setting != "" {
		if first, err = strconv.ParseUint(setting, 10, 64); err != nil {
			t.Fatalf("SPROUTFS_PAGER_SOAK_SEED=%q: %v", setting, err)
		}
	}
	worlds := map[uint64]string{0: "prefetch", 1: "fork", 2: "rules"}
	soaking := func(world uint64) bool { return true }
	if setting := os.Getenv("SPROUTFS_PAGER_SOAK_WORLDS"); setting != "" {
		named := strings.Split(setting, ",")
		soaking = func(world uint64) bool { return slices.Contains(named, worlds[world]) }
	}
	vmmemory.SetCheckpointBatchPages(t, 2)
	workers := max(1, runtime.GOMAXPROCS(0)/2)
	t.Logf("soaking for %s from seed %d with %d worlds at once on %d cores", length, first, workers,
		runtime.GOMAXPROCS(0))
	deadline := time.Now().Add(length)
	var next, prefetches, forks, rules atomic.Uint64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for !t.Failed() && time.Now().Before(deadline) {
				seed := first + next.Add(1) - 1
				ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{Seed: seed, Buggify: true}))
				disk := sim.New(sim.Config{Seed: seed}).NewDisk("pager", sim.DiskConfig{})
				if !soaking(seed % 3) {
					continue
				}
				switch seed % 3 {
				case 0:
					t.Run(fmt.Sprintf("prefetch-seed-%d", seed), func(t *testing.T) {
						prefetchWorld(t, ctx, seed, disk, soakForks, soakVCPUs)
					})
					prefetches.Add(1)
				case 1:
					t.Run(fmt.Sprintf("fork-seed-%d", seed), func(t *testing.T) {
						forkWorld(t, ctx, seed, disk, soakChildren)
					})
					forks.Add(1)
				default:
					t.Run(fmt.Sprintf("rules-seed-%d", seed), func(t *testing.T) {
						rulesWorld(t, ctx, seed, disk, soakForks, soakVCPUs)
					})
					rules.Add(1)
				}
			}
		})
	}
	wg.Wait()
	t.Logf("soaked %d prefetch, %d fork and %d rules worlds", prefetches.Load(), forks.Load(), rules.Load())
}
