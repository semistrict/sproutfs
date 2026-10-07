package vmmemory_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// slowPeerBacking is a peerBacking whose reads take time as slowBacking's do
// and are released by a controlled run.
type slowPeerBacking struct{ *peerBacking }

func (b *slowPeerBacking) LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	timer := time.NewTimer(readCost + time.Duration(uint64(len(dst))/uint64(b.pageSize))*pageCost)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	if err := sim.Admit(ctx, "slow-peer-backing/read"); err != nil {
		return nil, err
	}
	return b.peerBacking.LoadUnpublished(ctx, offset, dst)
}

func (b *slowPeerBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	_, err := b.LoadUnpublished(ctx, offset, dst)
	return err
}

// prefetchCampaignSeeds are the seeds the campaign runs, which between them
// fire every prefetch site and reach every prefetch probe.
var prefetchCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// campaignPages is each guest's memory, in pages.
const campaignPages = 16

// campaignGuest is one guest of the campaign: its memory region, what it maps,
// and the byte it must read at each page.
type campaignGuest struct {
	name   string
	region *vmmemory.MemoryRegion
	m      *mapping
	want   []byte
}

// Prefetches survive every fault their sites inject — a read held back, a run
// left unread as if at the bound, a read that fails — beside guests that read
// and store at once, an arena too small for them so allocations cancel
// prefetches, two guests of one checkpoint whose loads race for the same
// identities, and a migration's source holding pages the volume names. Every
// read a guest makes returns what it last stored or what its volume holds, and
// across the seeds every site fires and every probe is reached. Every read of
// a backing and every point a prefetch begins, lands, maps or wakes a waiting
// fault completes when the seed's scheduler chooses.
func TestPrefetchSurvivesItsFaultsAndReachesItsProbes(t *testing.T) {
	probes := make(map[string]uint64)
	fired := make(map[string]uint64)
	for _, seed := range prefetchCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reached, sites, _ := prefetchCampaign(t, seed)
				maps.Copy(probes, addCounts(probes, reached))
				maps.Copy(fired, addCounts(fired, sites))
			})
		})
	}
	var missed []string
	for _, name := range slices.Concat(vmmemory.PrefetchSites(), vmmemory.PrefetchProbes()) {
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

// prefetchCampaign runs one seed and reports the probes it reached, the sites
// it fired, and the order its scheduler released every operation in.
func prefetchCampaign(t *testing.T, seed uint64) (map[string]uint64, map[string]uint64, []byte) {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		// The spill file's disk is a runtime's of its own: what is under test
		// is the pager's order, and the fixture closes the file after the
		// scheduler has stopped.
		disk := sim.New(sim.Config{Seed: seed}).NewDisk("pager", sim.DiskConfig{})
		f, err := newFixtureOn(t, ctx, disk, vmmemory.Config{PageSize: uint64(pageSize), Arena: suiteArena,
			ResidentPages: 24, LogicalPages: 80, DirtyPages: 48, ReadAheadPages: 4, PrefetchRuns: 2})
		if err != nil {
			t.Error(err)
			return
		}
		guests := campaignGuests(f)
		var wg sync.WaitGroup
		for at, guest := range guests {
			wg.Go(func() {
				runCampaignGuest(sim.WithTask(ctx, guest.name), t, guest, rand.New(rand.NewPCG(seed, uint64(at))))
			})
		}
		wg.Wait()
		for _, guest := range guests {
			if err := guest.region.SettlePrefetches(ctx); err != nil {
				t.Error(err)
				return
			}
			for page := range uint64(campaignPages) {
				got, err := memoryByte(sim.WithTask(ctx, guest.name+"-check"), guest.region, guest.m, page, nil)
				if err != nil || got != guest.want[page] {
					t.Errorf("%s page %d reads %d at the end, want %d: %v", guest.name, page, got, guest.want[page], err)
				}
			}
		}
		// The guests stop and detach while the scheduler still runs: a
		// detach waits for the prefetches it cancels.
		for _, guest := range guests {
			guest.m.arena.mu.Lock()
			clear(guest.m.pages)
			guest.m.arena.mu.Unlock()
			if err := guest.region.Detach(ctx); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	recording, err := scheduler.Recording(nil)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Probes(), runtime.FiredSites(), recording.Execution
}

// A seed of the campaign replays: run twice, it releases every operation in
// the same order and reaches the same probes the same number of times. What a
// seed reaches is then the seed's, not the Go scheduler's. Before 2026-10-04 it
// was not: a fault's read and a prefetch were tasks named by numbers counted
// across the host, in the order the Go scheduler ran the faults of other
// tasks; an allocation woken by slots coming back went on beside whatever gave
// them back; and the guests whose pauses ended at one instant took free slots
// in whatever order they ran. A seed reached the duplicate probe in some runs
// and not in others.
func TestPrefetchCampaignReplaysItsSeeds(t *testing.T) {
	for _, seed := range []uint64{1, 5, 7} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var probes [2]map[string]uint64
			var orders [2][]byte
			for run := range 2 {
				synctest.Test(t, func(t *testing.T) {
					probes[run], _, orders[run] = prefetchCampaign(t, seed)
				})
			}
			if !maps.Equal(probes[0], probes[1]) {
				t.Fatalf("seed %d reached %v, then %v", seed, probes[0], probes[1])
			}
			if !bytes.Equal(orders[0], orders[1]) {
				t.Fatalf("seed %d released its operations in another order on its second run", seed)
			}
		})
	}
}

// campaignGuests attaches the campaign's three guests: two forks of one
// checkpoint, whose loads race for the same identities, and one whose
// migration source serves two pages the volume names as its own.
func campaignGuests(f *fixture) []*campaignGuest {
	var guests []*campaignGuest
	for _, name := range []string{"fork-a", "fork-b"} {
		b := f.slowBacking(campaignPages)
		b.admit = true
		r, m := f.attach(b)
		guests = append(guests, &campaignGuest{name: name, region: r, m: m, want: initialBytes(campaignPages)})
	}
	peer := &slowPeerBacking{&peerBacking{backing: f.newBacking(campaignPages),
		hidden: map[uint64]byte{5: 0xa5, 11: 0xab}}}
	peer.source = control.Ref{VM: peer.owner + "-migrated", Sequence: 1}
	r, m := f.attach(peer)
	want := initialBytes(campaignPages)
	want[5], want[11] = 0xa5, 0xab
	guests = append(guests, &campaignGuest{name: "migrated", region: r, m: m, want: want})
	// A disk whose guest flushes between its stores, so a journal capture
	// write-protects pages the other steps then store into, read and spill.
	disk, diskMapping := f.attachKind(vmmemory.Pmem, f.slowBacking(campaignPages))
	guests = append(guests, &campaignGuest{name: "disk", region: disk, m: diskMapping, want: initialBytes(campaignPages)})
	return guests
}

// initialBytes is what a fixture backing holds: every byte of page i is i+1.
func initialBytes(pages int) []byte {
	want := make([]byte, pages)
	for page := range want {
		want[page] = byte(page + 1)
	}
	return want
}

// runCampaignGuest reads and stores a guest's memory, half the time the page
// after the last and otherwise one at random, sometimes pausing, and requires
// each read to return what the model holds.
func runCampaignGuest(ctx context.Context, t *testing.T, g *campaignGuest, random *rand.Rand) {
	page := uint64(0)
	for op := range 60 {
		// Guests whose pauses end at one instant go on one at a time, in the
		// order the run chooses, as a simulated VMM's accesses are admitted:
		// otherwise which of them takes a free slot first is the Go
		// scheduler's choice, and a seed would not replay.
		if err := sim.Admit(ctx, "campaign/access"); err != nil {
			t.Errorf("%s: %v", g.name, err)
			return
		}
		if random.IntN(2) == 0 {
			page = (page + 1) % campaignPages
		} else {
			page = random.Uint64N(campaignPages)
		}
		if random.IntN(4) == 0 {
			value := byte(0x40 + op)
			if _, err := memoryByte(ctx, g.region, g.m, page, &value); err != nil {
				t.Errorf("%s storing into page %d: %v", g.name, page, err)
				return
			}
			g.want[page] = value
		} else {
			got, err := memoryByte(ctx, g.region, g.m, page, nil)
			if err != nil {
				t.Errorf("%s reading page %d: %v", g.name, page, err)
				return
			}
			if got != g.want[page] {
				t.Errorf("%s page %d reads %d, want %d", g.name, page, got, g.want[page])
				return
			}
		}
		if g.region.Kind() == vmmemory.Pmem && random.IntN(5) == 0 {
			// A flush: a capture of what the guest stored, whose journal
			// write fails one time in four.
			captured, err := g.region.Capture(ctx, g.region.Unjournaled())
			if err != nil {
				t.Errorf("%s capturing: %v", g.name, err)
				return
			}
			if random.IntN(4) == 0 {
				captured.Fail(ctx)
			}
		}
		// The journal's rule holds after every step, in RAM as in a disk: a
		// page the guest may store into without a fault is unjournaled.
		if err := unjournaledWritable(g.region, g.m); err != nil {
			t.Errorf("%s after step %d: %v", g.name, op, err)
			return
		}
		if random.IntN(3) == 0 {
			time.Sleep(time.Duration(random.IntN(4)) * time.Millisecond)
		}
	}
}
