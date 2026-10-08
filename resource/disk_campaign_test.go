package resource_test

import (
	"errors"
	"fmt"
	"math/big"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// diskSites are the faults the simulated disk injects into what a limiter
// reads.
var diskSites = []string{
	sim.BuggifySpaceFails, sim.BuggifySpaceInconsistent, sim.BuggifySpaceLow, sim.BuggifyOutsideFills,
	sim.BuggifyDriftFast, sim.BuggifyDeviceWritesFail, sim.BuggifyDeviceWritesJump, sim.BuggifyDeviceWritesReset,
}

// campaignGoals are the goals the seeds take in turn.
var campaignGoals = []resource.DiskGoal{
	{FreeBytes: 25 * unit},
	{FreePercent: 10},
	{FreeBytes: 10 * unit, UsedBytes: 150 * unit},
}

const (
	campaignSeeds      = 24
	campaignSteps      = 200
	campaignBurst      = 40 * unit
	campaignPerDay     = 400 * unit
	campaignSpill      = 25 * unit
	campaignSpillFiles = 2
)

// The limiter over a disk whose free space drifts on its own and whose readings
// fail, lie low, contradict themselves and jump, under a cache that fills
// whenever it may and spill files that fill as guests spill. At every step the
// cache holds no more than its share, the spill files are counted at their
// whole promise, and the cache has written no more than its budget allowed.
// When the faults stop and the disk stands still, the limiter reaches exactly
// what the disk as it is implies. Across the seeds, every probe fires and every
// fault site is reached.
func TestTheDiskLimiterStaysSafeUnderFaults(t *testing.T) {
	probes := map[string]uint64{}
	fired := map[string]bool{}
	for seed := uint64(1); seed <= campaignSeeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := diskCampaign(t, seed)
				for name, count := range run.Probes() {
					probes[name] += count
				}
				for site := range run.FiredSites() {
					fired[site] = true
				}
			})
		})
	}
	for _, probe := range resource.DiskProbes {
		if probes[probe] == 0 {
			t.Errorf("no seed reached %s: %v", probe, probes)
		}
	}
	for _, site := range diskSites {
		if !fired[site] {
			t.Errorf("no seed fired the fault site %s", site)
		}
	}
}

func diskCampaign(t *testing.T, seed uint64) *sim.Runtime {
	f := newDiskFixture(t, seed, sim.SpaceConfig{TotalBytes: diskTotal, OutsideBytes: diskOutside,
		DriftBytesPerSecond: 1_000_000})
	random := f.runtime.Random("disk-campaign")
	goal := campaignGoals[int(seed)%len(campaignGoals)]
	spills := make([]platform.File, campaignSpillFiles)
	users := make([]resource.DiskUser, campaignSpillFiles)
	for i := range spills {
		name := fmt.Sprintf("spill-%d", i)
		spills[i] = f.sparse(name, campaignSpill)
		users[i] = spill(name, campaignSpill, spills[i])
	}
	started := f.clock.Now()
	l := f.limiter(resource.DiskLimiterConfig{Goal: goal, Users: users,
		Writes: resource.WriteBudget{BytesPerDay: campaignPerDay, BurstBytes: campaignBurst}, Device: f.disk})
	defer l.Close()
	// The faults start once the limiter is built: a host whose first reading
	// fails does not start.
	f.runtime.SetBuggify(true)
	cache := f.cache()
	unregister, err := l.RegisterCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	spilled := make([]int64, campaignSpillFiles)

	for step := range campaignSteps {
		key := fmt.Sprintf("%d", step)
		f.tick(1)
		// The cache fills a few regions while its share and its budget let it.
		for attempt := range 3 {
			// A reading may fail; the cache then acts on the last one.
			_ = l.Refresh(f.ctx)
			if cache.Held()+unit > l.CacheShare() {
				break
			}
			if !l.Admit(unit, random.Intn(fmt.Sprintf("%s/priority/%d", key, attempt), resource.DiskWritePriorities)) {
				break
			}
			cache.mu.Lock()
			at := cache.next
			cache.next += unit
			cache.mu.Unlock()
			if _, err := cache.file.WriteAt(f.ctx, region, at); err != nil {
				// A write the disk refuses or fails leaves the region
				// unwritten, as the cache leaves it: the store serves it.
				if !errors.Is(err, platform.ErrNoSpace) && !errors.Is(err, platform.ErrInjectedFault) {
					t.Fatal(err)
				}
				break
			}
			cache.mu.Lock()
			cache.regions = append(cache.regions, at)
			cache.mu.Unlock()
		}
		// A guest spills a page now and then. The disk may be full of other
		// writers' bytes, which no limiter can help, and its device may fail.
		if file := random.Intn(key+"/spill", 4); file < campaignSpillFiles && spilled[file] < campaignSpill {
			if _, err := spills[file].WriteAt(f.ctx, region, spilled[file]); err == nil {
				spilled[file] += unit
			} else if !errors.Is(err, platform.ErrNoSpace) && !errors.Is(err, platform.ErrInjectedFault) {
				t.Fatal(err)
			}
		}
		// Other writers come and go, and write to the device.
		switch random.Intn(key+"/outside", 40) {
		case 0:
			f.disk.SetOutsideBytes(diskTotal - 30*unit)
		case 1, 2:
			f.disk.SetOutsideBytes(diskOutside)
		}
		if random.Chance(key+"/device", 0.2) {
			f.disk.AddDeviceWrites(random.Uint64(key+"/device-bytes") % (5 * unit))
		}
		if err := cache.failures(); err != nil {
			t.Fatal(err)
		}
		checkDiskLimiter(t, step, l, cache, f.clock.Since(started))
	}

	// The faults stop and the disk stands still: the limiter reaches what the
	// disk implies, and the budget its burst.
	f.runtime.SetBuggify(false)
	f.disk.SetSpaceDrift(0)
	f.converge()
	if err := l.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	usage := f.disk.Usage()
	want := modelShare(goal, usage.TotalBytes-usage.OutsideBytes, usage.TotalBytes,
		campaignSpillFiles*campaignSpill, cache.Held())
	status := l.Status()
	if status.CacheShareBytes != want.share {
		t.Fatalf("with the disk at %+v the cache's share is %d, want %d", usage, status.CacheShareBytes, want.share)
	}
	if ready := l.Ready() == nil; ready != (want.hard >= 0) {
		t.Fatalf("with the disk at %+v the host is ready %v, want %v: %v", usage, ready, want.hard >= 0, l.Ready())
	}
	f.clock.Advance(48 * time.Hour)
	if left := l.Status().Writes.LeftBytes; left != campaignBurst {
		t.Fatalf("two quiet days left the budget at %d, want its burst of %d", left, campaignBurst)
	}
	return f.runtime
}

// checkDiskLimiter is what must hold at every step, whatever the disk reported.
func checkDiskLimiter(t *testing.T, step int, l *resource.DiskLimiter, cache *diskCache, elapsed time.Duration) {
	t.Helper()
	if held, share := cache.Held(), l.CacheShare(); held > share {
		t.Fatalf("step %d: the cache holds %d of a share of %d", step, held, share)
	}
	status := l.Status()
	for _, promise := range status.Promises {
		if promise.PromisedBytes != campaignSpill {
			t.Fatalf("step %d: %s is counted at %d, want its whole promise of %d", step, promise.Name,
				promise.PromisedBytes, campaignSpill)
		}
	}
	if (status.Unready != "") != (l.Ready() != nil) {
		t.Fatalf("step %d: the status says unready %q and Ready says %v", step, status.Unready, l.Ready())
	}
	// Admitted bytes never exceed a burst and the daily average since the
	// limiter started.
	allowed := new(big.Int).Mul(big.NewInt(campaignPerDay), big.NewInt(int64(elapsed)))
	allowed.Quo(allowed, big.NewInt(int64(24*time.Hour)))
	allowed.Add(allowed, big.NewInt(campaignBurst))
	if admitted := new(big.Int).SetUint64(status.Writes.AdmittedBytes); admitted.Cmp(allowed) > 0 {
		t.Fatalf("step %d: the cache was admitted %v bytes in %s, past the budget's %v", step, admitted, elapsed, allowed)
	}
}

type modelled struct{ hard, share int64 }

// modelShare is the cache's share as the limiter's goals define it, computed
// from the disk's true state rather than from any reading.
func modelShare(goal resource.DiskGoal, room, total, promised, held int64) modelled {
	percent := func(value, of int64) int64 {
		product := new(big.Int).Mul(big.NewInt(value), big.NewInt(of))
		return product.Quo(product, big.NewInt(100)).Int64()
	}
	floor := max(goal.FreeBytes, percent(total, goal.FreePercent))
	hard := room - floor - promised
	share := hard - min(percent(max(hard-held, 0), resource.DefaultDiskBandPercent), resource.DefaultDiskMaxBand)
	if goal.UsedBytes > 0 {
		hard = min(hard, goal.UsedBytes-promised)
		share = min(share, goal.UsedBytes-promised)
	}
	return modelled{hard: hard, share: share}
}
