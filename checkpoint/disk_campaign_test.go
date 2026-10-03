package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// diskWorkload is a writer filling the disk with the pages of two VMs in turn,
// two readers reading what has been written so far, and a limiter that lowers
// the share, fits the disk to it and raises it again at even points of the
// writer's run, all at once. The writer waits for each fit, because the two
// would otherwise race for the log's one writer by the Go scheduler's choice
// rather than the seed's; the readers race both. Every read is checked: a hit
// returns what was written, and an item the disk refuses is one the disk lied
// about to that read.
type diskWorkload struct {
	writes, reads, fits int
	// lengths gives each page's envelope length.
	length func(page uint64) int
}

// readsAhead is how many reads the readers may make ahead of the writes.
const readsAhead = 40

// diskWorkloadResult is what one run of a workload saw.
type diskWorkloadResult struct {
	outcomes map[diskReadOutcome]int
	refused  int
}

func (w diskWorkload) keys(writer string) []diskKey { return pages(writer, 0, uint64(w.writes)) }

// run drives the workload against f from the goroutine it is called on, and
// returns once every actor has finished.
func (w diskWorkload) run(t *testing.T, ctx context.Context, f *diskFixture) diskWorkloadResult {
	writers := []string{"wa", "wb"}
	model := make(map[diskKey][]byte)
	for _, writer := range writers {
		for _, key := range w.keys(writer) {
			model[key] = payloadOf(key, w.length(key.Page))
		}
	}
	var progress [2]atomic.Int64
	writes := make([]diskKey, 0, len(writers)*w.writes)
	for page := range uint64(w.writes) {
		for _, writer := range writers {
			writes = append(writes, keyOf(writer, page))
		}
	}
	// Each write lets one reader read once more, so the readers keep pace
	// with the writers rather than finish before them. They start a few reads
	// ahead, so some are still reading while a write evicts.
	ticks := make(chan struct{}, len(writers)*w.writes+readsAhead)
	for range readsAhead {
		ticks <- struct{}{}
	}
	fitTicks, fitted := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	result := diskWorkloadResult{outcomes: make(map[diskReadOutcome]int)}
	var group sync.WaitGroup
	group.Go(func() {
		ctx := sim.WithTask(ctx, "writer")
		fitEvery := len(writes) / (w.fits + 1)
		for at, key := range writes {
			err := f.disk.write(ctx, key, model[key], WriteFillPublication)
			if errors.Is(err, ErrDiskRefused) {
				result.refused++
			} else if err != nil {
				t.Errorf("writing page %d of %s: %v", key.Page, key.Ref.VM, err)
			}
			progress[at%len(writers)].Add(1)
			ticks <- struct{}{}
			if (at+1)%fitEvery == 0 && (at+1)/fitEvery <= w.fits {
				fitTicks <- struct{}{}
				<-fitted
			}
		}
	})
	random := f.runtime.Random("disk-workload")
	for reader := range 2 {
		group.Go(func() {
			ctx := sim.WithTask(ctx, fmt.Sprintf("reader-%d", reader))
			for read := range w.reads {
				<-ticks
				id := fmt.Sprintf("%d/%d", reader, read)
				which := random.Intn("writer/"+id, len(writers))
				written := progress[which].Load()
				if written == 0 {
					continue
				}
				// Most reads are of the pages written last, which the disk
				// is most likely to still hold; the rest are of any page
				// written, which is how a read lands on a region being
				// evicted.
				back := min(written, int64(1+random.Intn("back/"+id, 48)))
				if random.Chance("old/"+id, 0.4) {
					back = int64(1 + random.Intn("oldest/"+id, int(written)))
				}
				key := keyOf(writers[which], uint64(written-back))
				witnessCtx, witness := witnessed(ctx)
				data, outcome := f.disk.read(witnessCtx, key)
				mu.Lock()
				result.outcomes[outcome]++
				mu.Unlock()
				switch outcome {
				case diskHit:
					if !bytes.Equal(data, model[key]) {
						t.Errorf("page %d of %s read back %d other bytes", key.Page, key.Ref.VM, len(data))
					}
					f.disk.served(key)
				case diskKeyMismatch, diskDamaged:
					if !witness.lied.Load() {
						t.Errorf("the disk returned page %d of %s as written, and the cache refused it as %d",
							key.Page, key.Ref.VM, outcome)
					}
				}
			}
		})
	}
	group.Go(func() {
		ctx := sim.WithTask(ctx, "limiter")
		share := f.budget.Share()
		for range w.fits {
			<-fitTicks
			f.budget.share.Store(share - 2*testRegionBytes)
			if err := f.disk.fit(ctx); err != nil {
				t.Errorf("fitting the disk: %v", err)
			}
			f.budget.share.Store(share)
			fitted <- struct{}{}
		}
	})
	group.Wait()
	return result
}

// scheduledDisk runs a workload over a disk whose every operation a seeded
// scheduler releases, so the order reads, writes, evictions and fits complete
// in is the seed's.
func scheduledDisk(t *testing.T, seed uint64, buggify bool, config diskFixtureConfig,
	workload diskWorkload) (*diskFixture, diskWorkloadResult) {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: buggify})
	var f *diskFixture
	var result diskWorkloadResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		var err error
		if f, err = openDiskFixture(ctx, runtime, config); err != nil {
			t.Error(err)
			return
		}
		result = workload.run(t, ctx, f)
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.FailNow()
	}
	return f, result
}

// The races that matter on the disk — a read in flight while its region is
// evicted, a second chance under reads, a region closing under a read, a fit
// under reads — complete at the disk in the order each seed's scheduler
// chooses. What the readers do between two disk operations is the Go
// scheduler's, so a seed explores, it does not replay. Nothing fails: every
// hit returns what was written, no item is refused, and at rest the
// bookkeeping adds up and the disk holds no more than its share.
func TestDiskRacesUnderTheScheduler(t *testing.T) {
	workload := diskWorkload{writes: 150, reads: 150, fits: 4,
		length: func(page uint64) int { return 500 + int(page*97%4000) }}
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Reads are slow against writes, so a read is often still in
				// flight when its region is chosen as the victim.
				f, result := scheduledDisk(t, seed, false,
					diskFixtureConfig{regions: 6, disk: sim.DiskConfig{ReadLatency: 2 * time.Millisecond}}, workload)
				stats := f.disk.stats()
				t.Logf("the disk ended at %+v, reached %v and served %v", stats, f.runtime.Probes(), result.outcomes)
				if stats.Lost != 0 || f.file.lied() != 0 || result.outcomes[diskHit] == 0 {
					t.Fatalf("the disk lost %d copies, lied %d times and served %v", stats.Lost, f.file.lied(),
						result.outcomes)
				}
				if stats.Regions > 5 || stats.Evicted == 0 {
					t.Fatalf("the disk ended at %+v, want no more than five regions and some given back", stats)
				}
				f.disk.checkInvariants(t)
			})
		})
	}
}

// diskSites are the fault-injection sites the disk campaign must fire: the
// cache's own and the simulated disk's read chaos.
var diskSites = []string{
	buggifyDiskFailedWrite, buggifyDiskShortWrite, buggifyDiskFailedSync, buggifyDiskTornTable,
	buggifyDiskFailedPunch, buggifyDiskFailedAllocate,
	sim.BuggifyDiskSlowRead, sim.BuggifyDiskReadBitFlip, sim.BuggifyDiskMisdirectsRead,
}

// diskProbes are the probes the disk campaign must reach.
var diskProbes = []string{
	ProbeDiskSecondChance, ProbeDiskSecondChanceBounded, ProbeDiskFreeRegion,
	ProbeDiskEvictionWaitsForReader, ProbeDiskKeyMismatch, ProbeDiskChecksumMismatch,
}

// diskCampaignSeeds are the seeds the campaign runs, which between them
// activate every site.
var diskCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8}

// The disk survives every fault its sites inject — a failed or short write, a
// failed sync, a torn table, a failed punch or allocation, and a read that is
// slow, flips a bit or is misdirected — with misses alone: every hit returns
// what was written, and every item the cache refused is one the disk lied
// about, which a file that keeps a copy of everything written tells apart
// from one the cache misread. Across the seeds every site fires and every
// probe is reached, because a fault nothing drives proves nothing.
func TestDiskSurvivesItsFaultsAndReachesItsProbes(t *testing.T) {
	workload := diskWorkload{writes: 200, reads: 200, fits: 4,
		length: func(page uint64) int { return 1500 + int(page*97%1500) }}
	probes := make(map[string]uint64)
	fired := make(map[string]uint64)
	for _, seed := range diskCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, result := scheduledDisk(t, seed, true,
					diskFixtureConfig{regions: 6, disk: sim.DiskConfig{ReadChaos: true, ReadLatency: 2 * time.Millisecond}},
					workload)
				if result.outcomes[diskHit] == 0 {
					t.Fatalf("the campaign served %v, want hits", result.outcomes)
				}
				f.disk.checkInvariants(t)
				maps.Copy(probes, addCounts(probes, f.runtime.Probes()))
				maps.Copy(fired, addCounts(fired, f.runtime.FiredSites()))
			})
		})
	}
	var missed []string
	for _, name := range slices.Concat(diskSites, diskProbes) {
		if probes[name]+fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}

func addCounts(into, from map[string]uint64) map[string]uint64 {
	sum := maps.Clone(into)
	for name, count := range from {
		sum[name] += count
	}
	return sum
}

// Losing power around a region's close leaves only what a later read can
// tell apart: a read is the bytes written or a miss, and a table that reads
// back intact names only items that read back intact, whatever the device
// kept of what was not synced. The power is lost after the items' sync and before
// the table is written, or after the table is written and before its sync,
// on a disk that resolves every unsynced write as a real device might.
func TestDiskPowerLossAroundClosingARegion(t *testing.T) {
	for _, moment := range []string{"before-table", "after-table"} {
		for _, seed := range []uint64{1, 2, 3, 4, 5, 6} {
			t.Run(fmt.Sprintf("%s/seed-%d", moment, seed), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newDiskFixture(t, diskFixtureConfig{seed: seed, regions: 8,
						disk: sim.DiskConfig{PowerLossFaults: true}})
					f.file.powerLoss = moment
					keys := pages("va", 0, testItemsPerRegion+1)
					f.fill(t, keys)
					if ops := f.file.operations(); !slices.Contains(ops, "power-loss "+moment) {
						t.Fatalf("the power was never lost: %q", ops)
					}
					for _, key := range keys {
						witnessCtx, _ := witnessed(f.ctx(t))
						if data, outcome := f.disk.read(witnessCtx, key); outcome == diskHit &&
							!bytes.Equal(data, f.model[key]) {
							t.Fatalf("page %d read back other bytes after the power loss", key.Page)
						}
					}
					_, items, err := readRegionTable(t.Context(), f.file, 0, testRegionBytes)
					if errors.Is(err, errNoTable) {
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					for _, item := range items {
						buffer := make([]byte, itemHeaderBytes(item.key)+int64(item.length))
						if err := readFull(t.Context(), f.file, buffer, int64(item.offset)); err != nil {
							t.Fatal(err)
						}
						parsed, err := parseItem(buffer, false)
						if err != nil || parsed.key != item.key || !bytes.Equal(parsed.data, f.model[item.key]) {
							t.Fatalf("the table names page %d at %d, which reads back as %v, %v", item.key.Page,
								item.offset, parsed.key, err)
						}
					}
				})
			})
		}
	}
}
