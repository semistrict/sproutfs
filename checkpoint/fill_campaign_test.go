package checkpoint_test

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// fillProbes are the probes the fill campaign must reach.
var fillProbes = []string{
	checkpoint.ProbeFillRightGranted, checkpoint.ProbeFillWithoutRight, checkpoint.ProbeFillRightLost,
	checkpoint.ProbeFillQueueFull, checkpoint.ProbeFillRateSpent, checkpoint.ProbeFillNoRoom,
	checkpoint.ProbeFillPeerDropped, checkpoint.ProbeFillWriteRefused, checkpoint.ProbeFillRanksChanged,
	checkpoint.ProbeKeepKept, checkpoint.ProbeKeepDuplicate, checkpoint.ProbeKeepRefused, checkpoint.ProbeKeepDropped,
}

// fillCampaignSeeds are the seeds the fill campaign runs, which between them
// fire every site and reach every probe.
var fillCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// fillCampaignPages is what each checkpoint the campaign publishes holds.
var fillCampaignPages = []uint64{0, 1, 2, 3}

// noisySector is sector content no encoder shrinks, so a page's envelope
// costs a fill what the page holds, and the campaign's queue, rate and budget
// are spent by what its bursts send.
func noisySector(tag string, page uint64, sector uint32) []byte {
	data := make([]byte, checkpoint.SectorSize)
	state := uint64(len(tag))<<48 ^ page<<32 ^ uint64(sector) ^ 0x9e3779b97f4a7c15
	for _, b := range []byte(tag) {
		state = state*31 + uint64(b)
	}
	for at := range data {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		data[at] = byte(state)
	}
	return data
}

// fillCampaign is one seed's cluster and what it published, read and held.
type fillCampaign struct {
	c *fillCluster
	// published is every checkpoint published, by VM, and models what each
	// holds.
	published map[string]*checkpoint.Index
	models    map[string]*model
	// lists is every list of caches the cluster held, which every stripe a
	// host holds must be ranked under.
	lists []rank.List
	// rights bounds the fills of reads: one per window read in each interval.
	rights int
	mu     sync.Mutex
}

// runFillCampaign is one seed: a cluster whose size and code the seed draws,
// with every completion released by the seed's scheduler and the sites on,
// through rounds of publications from any host, bursts of reads of the same
// pages by many hosts at once, and changes to the list of caches. Every read
// returns what was published. At rest, every stripe a host holds is of a
// window some list the cluster held ranks it for, and the reads filled no
// window more than once an interval.
func runFillCampaign(t *testing.T, seed uint64) *sim.Runtime {
	draw := sim.New(sim.Config{Seed: seed}).Random("fill-campaign")
	hosts := 2 + draw.Intn("hosts", 6)
	// Some seeds run more hosts than the code is wide, so a list that
	// changes pushes a cache out of a window's ranks.
	code := rank.CodeFor(max(1, hosts-draw.Intn("spare", 2)))
	t.Logf("seed=%d: %d hosts under %s", seed, hosts, code)
	scheduler := sim.NewScheduler(seed)
	f := &fillCampaign{published: map[string]*checkpoint.Index{}, models: map[string]*model{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.c = newFillCluster(t, fillConfig{hosts: hosts, code: code, share: 100,
			runtime: sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true,
				Network: sim.NetworkConfig{Latency: 200 * time.Microsecond, ConnectLatency: 200 * time.Microsecond},
				ObjectStore: sim.ObjectStoreConfig{GetLatency: 2 * time.Millisecond, HeadLatency: time.Millisecond,
					PutLatency: 4 * time.Millisecond, BytesPerSecond: 1 << 40}},
			// A small queue and rate, so a burst finds each of them spent, and
			// a background budget with no room for a keep of a whole 2 MiB
			// window, which a host sends its peer under 1+1: a host sends one
			// keep at a time, so its own keeps never spend the budget against
			// each other.
			cache: func(_ int, config *checkpoint.CacheConfig) {
				config.FillQueueBytes, config.FillBytesPerSecond = 6<<20, 4<<20
			},
			table: func(config *peer.TableConfig) { config.BackgroundBytes = 3 << 19 },
		})
		defer f.c.close()
		f.lists = append(f.lists, *f.c.list.Load())
		f.run(t, draw)
		f.c.settle(t)
		f.check(t)
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	return f.c.runtime
}

// run is the campaign's rounds.
func (f *fillCampaign) run(t *testing.T, draw sim.Random) {
	full := *f.c.list.Load()
	for round := range 6 {
		id := fmt.Sprintf("round-%d", round)
		// Each round is an interval of the fill rights, and refills the rate.
		for _, h := range f.c.hosts {
			h.clock.Advance(checkpoint.DefaultFillRightInterval)
		}
		if round < 3 || draw.Chance(id+"/publish", 0.5) {
			vm := fmt.Sprintf("vm-%d", round)
			publisher := f.c.hosts[draw.Intn(id+"/publisher", len(f.c.hosts))]
			index, m, err := publishFromOf(t, publisher.store, vm, fillCampaignPages, noisySector)
			if err != nil {
				t.Errorf("publishing %s from %s: %v", vm, publisher.name, err)
				return
			}
			f.published[vm], f.models[vm] = index, m
		}
		// A disk leaves the membership, as a drained host's does, and comes
		// back in a later round. Some hosts read the membership at once, and
		// the rest only when a peer names its generation or in the next
		// round.
		served := full
		if draw.Chance(id+"/leave", 0.4) && full.Len() > 1 {
			served = full.Without(full.Caches()[draw.Intn(id+"/gone", full.Len())].Identity)
			f.lists = append(f.lists, served)
		}
		var readers []*fillHost
		for _, h := range f.c.hosts {
			if draw.Chance(id+"/reads/"+h.name, 0.5) {
				readers = append(readers, h)
			}
		}
		if len(readers) == 0 {
			// Every host catches up with the list it was last served.
			readers = f.c.hosts
		}
		f.c.hold(t, served, readers...)
		f.burst(t, draw, id)
	}
}

// burst has many hosts read the same pages at once, each through its own
// cache, and checks what each read.
func (f *fillCampaign) burst(t *testing.T, draw sim.Random, id string) {
	vms := slices.Sorted(maps.Keys(f.published))
	type read struct {
		host *fillHost
		vm   string
		page uint64
	}
	var reads []read
	windows := map[rank.Window]bool{}
	for at := range 2 + draw.Intn(id+"/pages", 3) {
		vm := vms[draw.Intn(fmt.Sprintf("%s/vm-%d", id, at), len(vms))]
		page := fillCampaignPages[draw.Intn(fmt.Sprintf("%s/page-%d", id, at), len(fillCampaignPages))]
		ref := f.published[vm].Ref()
		windows[pageWindow(ref, page)], windows[segmentWindow(ref)] = true, true
		for _, h := range f.c.hosts {
			if draw.Chance(fmt.Sprintf("%s/%s/%d/%s", id, vm, page, h.name), 0.7) {
				reads = append(reads, read{host: h, vm: vm, page: page})
			}
		}
	}
	f.rights += len(windows)
	var readers sync.WaitGroup
	for _, r := range reads {
		readers.Go(func() {
			ctx := sim.WithTask(f.c.ctx(t), fmt.Sprintf("%s/%s/%s/%d", id, r.host.name, r.vm, r.page))
			index, err := r.host.store.Open(ctx, f.published[r.vm].Ref())
			if err != nil {
				t.Errorf("%s opening %s: %v", r.host.name, r.vm, err)
				return
			}
			got := make([]byte, checkpoint.PageSize2MiB)
			if err := r.host.store.Read(ctx, index, "root", r.page*checkpoint.PageSize2MiB, got); err != nil {
				t.Errorf("%s reading page %d of %s: %v", r.host.name, r.page, r.vm, err)
				return
			}
			want := f.models[r.vm].contents["root"][r.page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB]
			if !bytes.Equal(got, want) {
				t.Errorf("%s read page %d of %s as other bytes than were published", r.host.name, r.page, r.vm)
			}
		})
	}
	readers.Wait()
}

// check holds the cluster at rest to what fills may leave: every stripe on
// a host whose window some list the cluster held ranks it for, and no more
// fills of reads than one a window read in each interval.
func (f *fillCampaign) check(t *testing.T) {
	code := f.lists[0].Code()
	for vm, index := range f.published {
		ref := index.Ref()
		windows := []rank.Window{segmentWindow(ref)}
		for _, page := range fillCampaignPages {
			windows = append(windows, pageWindow(ref, page))
		}
		for _, window := range windows {
			for _, h := range f.c.hosts {
				if held := h.cache.HeldIndices(window, 0, code); len(held) > 0 && !f.everRanked(window, h) {
					t.Errorf("%s holds stripes %v of %+v of %s, which no list ranks it for", h.name, held, window, vm)
				}
			}
		}
	}
	if fills := f.c.fills(); fills.FromReads > uint64(f.rights) {
		t.Errorf("the reads filled %d windows, and only %d windows were read an interval", fills.FromReads, f.rights)
	}
}

// everRanked reports whether some list the cluster held ranks h for window.
func (f *fillCampaign) everRanked(window rank.Window, h *fillHost) bool {
	for _, list := range f.lists {
		if slices.ContainsFunc(list.Ranks(window), func(cache rank.Cache) bool {
			return cache.Identity == h.cache.Identity()
		}) {
			return true
		}
	}
	return false
}

// Fills survive every fault their sites inject — a queue that reports itself
// full, a fill right whose answer is lost, a list that changes between the
// read and the fill, a keep sent twice, a write the budget refuses and a keep
// its holder drops — and the peer server's and the disk's own: every read is
// what was published, every stripe on a host is of a window a list ranked it
// for, and reads fill a window at most once an interval. Across the seeds
// every site fires and every probe is reached, because a fault nothing drives
// proves nothing.
func TestFillsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	probes := map[string]uint64{}
	fired := map[string]uint64{}
	for _, seed := range fillCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runFillCampaign(t, seed)
				for name, count := range runtime.Probes() {
					probes[name] += count
				}
				for name, count := range runtime.FiredSites() {
					fired[name] += count
				}
			})
		})
	}
	var missed []string
	for _, name := range checkpoint.FillSites {
		if fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	for _, name := range fillProbes {
		if probes[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}
