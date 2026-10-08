package vmmemory_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
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
	// vcpus is how many vCPUs the guest runs, one where it is zero. Each
	// reads and stores only its own pages, every vcpus-th from its number,
	// so what each reads is still the model's, while its faults and stores
	// meet the others' in the same read-ahead windows.
	vcpus int
	// storeOdds is one in how many accesses is a store, four where it is
	// zero.
	storeOdds int
	// lost is set once the guest's client lost a command and its region
	// failed of it (commandLost): the machine is gone, and nothing of it is
	// checked again.
	lost atomic.Bool
}

// lose reports whether err ended the guest because its client lost a command:
// its region failed of an injected fault. The guest stops, and its region is
// detached with the others at the end, as a host closes a machine whose VMM
// has ended: until then the pages it held are no one's, and the guests that go
// on must still read every byte they stored. A detach beside the guests that
// go on does not replay, since nothing of it is admitted to the run's
// scheduler, so it waits for the end.
func (g *campaignGuest) lose(err error) bool {
	if g.lost.Load() {
		return true
	}
	if err == nil || !lostToItsClient(g.region, g.m) {
		return false
	}
	g.lost.Store(true)
	return true
}

// lostToItsClient reports whether region is a machine its client ended: the
// region failed after the client injected a fault it cannot survive (a lost
// command or a refused revocation), which a campaign takes as a machine gone
// rather than a failure of the pager.
func lostToItsClient(region *vmmemory.MemoryRegion, m *mapping) bool {
	return m.injected.Load() && vmmemory.Failed(region) != nil
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
		prefetchWorld(t, sim.WithRuntime(t.Context(), runtime), seed, scheduledSpillDisk(seed, scheduler, done), 2, 1)
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

// prefetchWorld is the campaign's world on one seed: forks forks of one
// checkpoint, a migrated guest and a disk, each on vcpus vCPUs, reading and
// storing at once over an arena too small for them, then each checked page by
// page and detached. ctx carries the runtime, and with it whether a scheduler
// orders what they do.
func prefetchWorld(t *testing.T, ctx context.Context, seed uint64, disk *sim.Disk, forks, vcpus int) {
	// A host never checkpoints RAM to relieve its dirty budget, so the budget
	// holds every page its guests may store into.
	guestCount := forks + 2
	f, err := newFixtureOn(t, ctx, disk, vmmemory.Config{PageSize: uint64(pageSize), Arena: suiteArena,
		ResidentPages: 24, LogicalPages: campaignPages * (guestCount + 1), DirtyPages: campaignPages * guestCount,
		ReadAheadPages: 4, PrefetchRuns: 2})
	if err != nil {
		t.Error(err)
		return
	}
	// Production's client takes runs in batches; odd seeds take that path.
	f.batched = seed%2 == 1
	guests := campaignGuests(f, forks)
	for _, guest := range guests {
		guest.vcpus = vcpus
	}
	runCampaignWorld(t, ctx, seed, guests)
}

// runCampaignWorld runs a campaign's guests at once, each on a task of its
// own, and beside each disk a flusher on a task of its own, as a guest's flush
// arrives on another vCPU than its stores. Then it checks every page of each
// guest and detaches them.
func runCampaignWorld(t *testing.T, ctx context.Context, seed uint64, guests []*campaignGuest) {
	var wg, flushers sync.WaitGroup
	stop := make(chan struct{})
	for at, guest := range guests {
		vcpus := max(1, guest.vcpus)
		for vcpu := range vcpus {
			task := guest.name
			if vcpus > 1 {
				task = fmt.Sprintf("%s/vcpu-%d", guest.name, vcpu)
			}
			wg.Go(func() {
				runCampaignGuest(sim.WithTask(ctx, task), t, guest,
					rand.New(rand.NewPCG(seed, uint64(at)|uint64(vcpu)<<32)), vcpu)
			})
		}
		if guest.region.Kind() == vmmemory.Pmem {
			flushers.Go(func() {
				runCampaignFlusher(sim.WithTask(ctx, guest.name+"-flusher"), t, guest,
					rand.New(rand.NewPCG(seed, uint64(len(guests)+at))), stop)
			})
		}
	}
	wg.Wait()
	close(stop)
	flushers.Wait()
	// A guest's region may have failed of a command another guest's eviction
	// lost, with nothing of its own left to meet the failure: it is lost too.
	for _, guest := range guests {
		if failed := vmmemory.Failed(guest.region); failed != nil {
			guest.lose(failed)
		}
	}
	for _, guest := range guests {
		if guest.lost.Load() {
			continue
		}
		if err := unjournaledWritable(guest.region, guest.m); err != nil {
			t.Errorf("%s once its vCPUs stopped: %v", guest.name, err)
		}
	}
	for _, guest := range guests {
		if guest.lost.Load() {
			continue
		}
		if err := guest.region.SettlePrefetches(ctx); err != nil {
			t.Error(err)
			return
		}
		for page := range uint64(len(guest.want)) {
			got, err := memoryByte(sim.WithTask(ctx, guest.name+"-check"), guest.region, guest.m, page, nil)
			if err != nil && guest.lose(err) {
				break
			}
			if err != nil || got != guest.want[page] {
				t.Errorf("%s page %d reads %d at the end, want %d: %v", guest.name, page, got, guest.want[page], err)
			}
		}
	}
	// The guests stop and detach while the scheduler still runs: a detach
	// waits for the prefetches it cancels.
	for _, guest := range guests {
		guest.m.arena.mu.Lock()
		clear(guest.m.pages)
		guest.m.arena.mu.Unlock()
		if err := guest.region.Detach(ctx); err != nil {
			t.Error(err)
		}
	}
}

// scheduledSpillDisk is the disk of a campaign's spill file, a runtime's of
// its own whose waits the campaign's scheduler releases until done closes, as
// it releases everything else the guests do. A disk is one queue, and whoever
// gives it up wakes the next in it: before 2026-10-07 the disk was a runtime's
// with no scheduler, and the two went on side by side at one instant, outside
// any turn of the run, both faults that go on to take slots. Once done has
// closed nothing waits: the fixture closes the file after the scheduler has
// stopped.
func scheduledSpillDisk(seed uint64, scheduler *sim.Scheduler, done <-chan struct{}) *sim.Disk {
	wait := func(ctx context.Context, id string, minimum, maximum time.Duration) error {
		select {
		case <-done:
			return nil
		default:
		}
		return scheduler.Wait(ctx, id, minimum, maximum)
	}
	return sim.New(sim.Config{Seed: seed, Wait: wait}).NewDisk("pager", sim.DiskConfig{})
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

// campaignGuests attaches the campaign's guests: forks forks of one
// checkpoint, whose loads race for the same identities, one whose migration
// source serves two pages the volume names as its own, and a disk. A guest
// whose client loses a command as it attaches is a machine that never started,
// and is left out.
func campaignGuests(f *fixture, forks int) []*campaignGuest {
	var guests []*campaignGuest
	attach := func(name string, kind vmmemory.MemoryRegionKind, b vmmemory.Backing, want []byte) {
		r, m, err := f.tryAttachBacking(vmmemory.MemoryRegionBacking{Kind: kind, Backing: b})
		if errors.Is(err, errInjected) {
			return
		}
		if err != nil {
			f.t.Fatal(err)
		}
		guests = append(guests, &campaignGuest{name: name, region: r, m: m, want: want})
	}
	for fork := range forks {
		b := f.slowBacking(campaignPages)
		b.admit = true
		attach("fork-"+string(rune('a'+fork)), vmmemory.Ram, b, withZeros(b.backing, initialBytes(campaignPages)))
	}
	peer := &slowPeerBacking{&peerBacking{backing: f.newBacking(campaignPages),
		hidden: map[uint64]byte{5: 0xa5, 11: 0xab}}}
	peer.source = control.Ref{VM: peer.owner + "-migrated", Sequence: 1}
	want := withZeros(peer.backing, initialBytes(campaignPages))
	want[5], want[11] = 0xa5, 0xab
	attach("migrated", vmmemory.Ram, peer, want)
	// A disk whose guest flushes between its stores, so a journal capture
	// write-protects pages the other steps then store into, read and spill.
	disk := f.slowBacking(campaignPages)
	attach("disk", vmmemory.Pmem, disk, withZeros(disk.backing, initialBytes(campaignPages)))
	return guests
}

// campaignZeros are the pages a campaign's volumes hold zeros at, which a
// fault maps to zero without reading anything: without them no campaign maps
// a zero, and no fault of a zero mapping is ever reached.
var campaignZeros = []uint64{3, 12}

// withZeros makes campaignZeros zeros in b, and in want, which it returns.
func withZeros(b *backing, want []byte) []byte {
	for _, page := range campaignZeros {
		b.zero[page] = true
		clear(b.data[page*uint64(b.pageSize) : (page+1)*uint64(b.pageSize)])
		want[page] = 0
	}
	return want
}

// runCampaignFlusher flushes a disk guest's writes from a task of its own
// until stop closes: a capture of what the guest stored, whose journal write
// fails one time in four, beside the guest's own stores, reads and spills.
func runCampaignFlusher(ctx context.Context, t *testing.T, g *campaignGuest, random *rand.Rand, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(1+random.IntN(4)) * time.Millisecond):
		}
		if err := sim.Admit(ctx, "campaign/flush"); err != nil {
			t.Errorf("%s flusher: %v", g.name, err)
			return
		}
		captured, err := g.region.Capture(ctx, g.region.Unjournaled())
		if err != nil {
			if !g.lose(err) {
				t.Errorf("%s flusher capturing: %v", g.name, err)
			}
			return
		}
		if random.IntN(4) == 0 {
			captured.Fail(ctx)
		}
	}
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
//
// vcpu is the guest's vCPU this runs, whose pages are every g.vcpus-th from it.
func runCampaignGuest(ctx context.Context, t *testing.T, g *campaignGuest, random *rand.Rand, vcpu int) {
	stride := uint64(max(1, g.vcpus))
	page := uint64(vcpu)
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
			page = (page + stride) % uint64(len(g.want))
		} else {
			page = uint64(vcpu) + stride*random.Uint64N(uint64(len(g.want))/stride)
		}
		if random.IntN(cmp.Or(g.storeOdds, 4)) == 0 {
			value := byte(0x40 + op)
			if _, err := memoryByte(ctx, g.region, g.m, page, &value); err != nil {
				if !g.lose(err) {
					t.Errorf("%s storing into page %d: %v", g.name, page, err)
				}
				return
			}
			g.want[page] = value
		} else {
			got, err := memoryByte(ctx, g.region, g.m, page, nil)
			if err != nil {
				if !g.lose(err) {
					t.Errorf("%s reading page %d: %v", g.name, page, err)
				}
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
				if !g.lose(err) {
					t.Errorf("%s capturing: %v", g.name, err)
				}
				return
			}
			if random.IntN(4) == 0 {
				captured.Fail(ctx)
			}
		}
		// The journal's rule holds after every step, in RAM as in a disk: a
		// page the guest may store into without a fault is unjournaled. With
		// one vCPU nothing else stores between the two looks this takes; with
		// several, another vCPU's store, its write-ahead or a rule it set off
		// may, so the rule is checked once they have all stopped.
		if stride == 1 && !g.lost.Load() {
			if err := unjournaledWritable(g.region, g.m); err != nil {
				t.Errorf("%s after step %d: %v", g.name, op, err)
				return
			}
		}
		if random.IntN(3) == 0 {
			time.Sleep(time.Duration(random.IntN(4)) * time.Millisecond)
		}
	}
}
