package vmmemory_test

import (
	"cmp"
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

// campaignGuest is one guest of the campaign: its machine, and the byte it
// must read at each page.
type campaignGuest struct {
	machine
	want []byte
	// vcpus is how many vCPUs the guest runs, one where it is zero. Each
	// reads and stores only its own pages, every vcpus-th from its number,
	// so what each reads is still the model's, while its faults and stores
	// meet the others' in the same read-ahead windows.
	vcpus int
	// storeOdds is one in how many accesses is a store, four where it is
	// zero.
	storeOdds int
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
				run := prefetchCampaign(t, seed)
				maps.Copy(probes, addCounts(probes, run.probes))
				maps.Copy(fired, addCounts(fired, run.fired))
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

// campaignVersions is how many published versions a campaign's pager keeps on
// seed: none on half the seeds, its own bound on the rest, independently of
// which seeds take the client's batched path. A pager that keeps versions
// loads a page it evicted from its spill file, so the seeds that keep none are
// the ones whose refaults reach the backing and the client's commands again.
func campaignVersions(seed uint64) int {
	if (seed/2)%2 == 0 {
		return -1
	}
	return 0
}

// prefetchCampaign runs one seed of the campaign.
func prefetchCampaign(t *testing.T, seed uint64) campaignRun {
	return runCampaign(t, seed, func(ctx context.Context, disk *sim.Disk) { prefetchWorld(t, ctx, seed, disk, 2, 1) })
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
		ReadAheadPages: 4, PrefetchRuns: 2, MaxSpillVersions: campaignVersions(seed)})
	if err != nil {
		// A host whose spill file could not be made never started.
		if !injected(err) {
			t.Error(err)
		}
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

// runCampaignWorld runs a campaign's guests at once, each vCPU on a task of
// its own, and beside each disk a flusher on a task of its own, as a guest's
// flush arrives on another vCPU than its stores, each guest's host closing it
// once it is gone. Then it checks every page of each guest still running and
// detaches them.
func runCampaignWorld(t *testing.T, ctx context.Context, seed uint64, guests []*campaignGuest) {
	machines := make([]*machine, len(guests))
	for at, guest := range guests {
		guest.boot(ctx)
		machines[at] = &guest.machine
	}
	stopHosts := hostMachines(ctx, t, machines)
	var vcpus, flushers sync.WaitGroup
	stop := make(chan struct{})
	for at, guest := range guests {
		count := max(1, guest.vcpus)
		for vcpu := range count {
			task := guest.name
			if count > 1 {
				task = fmt.Sprintf("%s/vcpu-%d", guest.name, vcpu)
			}
			random := rand.New(rand.NewPCG(seed, uint64(at)|uint64(vcpu)<<32))
			guest.run(&vcpus, task, func(ctx context.Context) { runCampaignGuest(ctx, t, guest, random, vcpu) })
		}
		if guest.region.Kind() == vmmemory.Pmem {
			random := rand.New(rand.NewPCG(seed, uint64(len(guests)+at)))
			guest.run(&flushers, guest.name+"-flusher", func(ctx context.Context) {
				runCampaignFlusher(ctx, t, guest, random, stop)
			})
		}
	}
	vcpus.Wait()
	// The last vCPU to stop goes on beside this, and so may a flusher whose
	// time came at the same instant: the flushers are stopped when the run
	// chooses, not by whichever of the two the Go scheduler runs first.
	if err := sim.Admit(sim.WithTask(ctx, "world"), "campaign/vcpus-stopped"); err != nil {
		t.Error(err)
	}
	close(stop)
	flushers.Wait()
	for _, guest := range guests {
		if guest.gone() {
			continue
		}
		if err := unjournaledWritable(guest.region, guest.m); err != nil {
			t.Errorf("%s once its vCPUs stopped: %v", guest.name, err)
		}
	}
	for _, guest := range guests {
		var look sync.WaitGroup
		guest.run(&look, guest.name+"-check", func(ctx context.Context) { checkCampaignGuest(ctx, t, guest) })
		look.Wait()
	}
	// The guests stop and detach while the scheduler still runs: a detach
	// waits for the prefetches it cancels.
	stopHosts()
	for _, guest := range guests {
		guest.close(ctx, t)
	}
}

// checkCampaignGuest requires every page of a guest still running to read
// what the model holds, once its prefetches have settled.
func checkCampaignGuest(ctx context.Context, t *testing.T, g *campaignGuest) {
	if g.gone() {
		return
	}
	if err := g.region.SettlePrefetches(ctx); err != nil {
		if !g.lose(err) {
			t.Errorf("%s settling its prefetches: %v", g.name, err)
		}
		return
	}
	for page := range uint64(len(g.want)) {
		got, err := memoryByte(ctx, g.region, g.m, page, nil)
		if g.lose(err) {
			return
		}
		if err != nil || got != g.want[page] {
			t.Errorf("%s page %d reads %d at the end, want %d: %v", g.name, page, got, g.want[page], err)
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
// stopped. The disk fails at random, as a device does (EIO, ENOSPC): an
// eviction's spill, a fault's read of a spilled page and the pager's making
// and giving back of the file.
func scheduledSpillDisk(seed uint64, scheduler *sim.Scheduler, done <-chan struct{}) *sim.Disk {
	wait := func(ctx context.Context, id string, minimum, maximum time.Duration) error {
		select {
		case <-done:
			return nil
		default:
		}
		return scheduler.Wait(ctx, id, minimum, maximum)
	}
	return sim.New(sim.Config{Seed: seed, Wait: wait, Buggify: true}).NewDisk("pager", sim.DiskConfig{})
}

// A seed of the campaign replays (testReplays). Before 2026-10-04 one did not:
// a fault's read and a prefetch were tasks named by numbers counted across
// the host, in the order the Go scheduler ran the faults of other tasks; an
// allocation woken by slots coming back went on beside whatever gave them
// back; and the guests whose pauses ended at one instant took free slots in
// whatever order they ran. A seed reached the duplicate probe in some runs
// and not in others.
func TestPrefetchCampaignReplaysItsSeeds(t *testing.T) {
	testReplays(t, []uint64{5, 7, 10}, prefetchCampaign)
}

// campaignGuests attaches the campaign's guests: forks forks of one
// checkpoint, whose loads race for the same identities, one whose migration
// source serves two pages the volume names as its own, and a disk.
func campaignGuests(f *fixture, forks int) []*campaignGuest {
	var guests []*campaignGuest
	attach := func(name string, kind vmmemory.MemoryRegionKind, b vmmemory.Backing, want []byte) {
		if guest := f.attachGuest(name, kind, b, want); guest != nil {
			guests = append(guests, guest)
		}
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

// attachGuest attaches a campaign's guest, which must read want. A guest
// that fails of an injected fault as it attaches is a machine that never
// started: it is nil.
func (f *fixture) attachGuest(name string, kind vmmemory.MemoryRegionKind, b vmmemory.Backing,
	want []byte) *campaignGuest {
	r, m, err := f.tryAttachBacking(vmmemory.MemoryRegionBacking{Kind: kind, Backing: b})
	if injected(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return &campaignGuest{machine: machine{name: name, region: r, m: m}, want: want}
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
			if !g.lose(err) {
				t.Errorf("%s flusher: %v", g.name, err)
			}
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
			if !g.lose(err) {
				t.Errorf("%s: %v", g.name, err)
			}
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
