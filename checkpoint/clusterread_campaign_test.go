package checkpoint_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// readProbes are the probes the read campaign must reach.
var readProbes = []string{
	checkpoint.ProbeClusterHit, checkpoint.ProbeClusterOwnHit, checkpoint.ProbeClusterMiss,
	checkpoint.ProbeClusterParity, checkpoint.ProbeClusterReplaced, checkpoint.ProbeClusterSecondRequest,
	checkpoint.ProbeClusterRefused, checkpoint.ProbeClusterStoreHedge, checkpoint.ProbeClusterStoreHedgeWon,
	checkpoint.ProbeClusterStoreHedgeRefused, checkpoint.ProbeClusterWrongStripe, checkpoint.ProbeClusterDrop,
	checkpoint.ProbeClusterRepair, checkpoint.ProbeClusterTimeout, checkpoint.ProbeClusterMarkedDown,
	checkpoint.ProbeClusterCapped, checkpoint.ProbeClusterCleared, checkpoint.ProbeClusterHeadCheck,
	checkpoint.ProbeClusterHeadMissing, checkpoint.ProbeClusterEarlierCode,
	// The membership changes under the reads, and some hosts read it only
	// when a peer names a newer generation.
	membership.ProbeHolderCaughtUp, membership.ProbeStaleAnswered, membership.ProbeSenderCaughtUp,
}

// readCampaignSeeds are the seeds the read campaign runs, which between them
// fire every site and reach every probe.
var readCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// readCampaign is one seed's cluster, what it published, and the parts it
// deleted behind the caches' backs.
type readCampaign struct {
	c         *fillCluster
	published map[string]*checkpoint.Index
	models    map[string]*model
	// deleted is the VMs whose parts are gone from the store, which a read
	// may fail to find but must never read wrong.
	deleted map[string]bool
	// lists is every list of caches the cluster held.
	lists []rank.List
	mu    sync.Mutex
}

// runReadCampaign is one seed: a cluster whose size and code the seed draws,
// on a network with a heavy tail and slow pairs, every completion released by
// the seed's scheduler and the sites on. Each round publishes from any host
// and has many hosts read the same pages at once, while the seed stalls a
// host's links, refuses them, slows them, loses a host and starts it again,
// shifts the ranks with a membership that lacks a disk, changes the
// deployment's code once and names the old one as earlier, or deletes a
// checkpoint's parts behind the caches. Only some hosts read each new
// membership at once; the rest learn of it when a peer names its generation.
// Every read returns what was published, or fails only for a part that is
// gone. At rest, every stripe a host holds is of a window some membership
// ranked it for under that stripe's code.
func runReadCampaign(t *testing.T, seed uint64) *sim.Runtime {
	draw := sim.New(sim.Config{Seed: seed}).Random("read-campaign")
	hosts := 2 + draw.Intn("hosts", 6)
	code := rank.CodeFor(max(1, hosts-draw.Intn("spare", 2)))
	t.Logf("seed=%d: %d hosts under %s", seed, hosts, code)
	scheduler := sim.NewScheduler(seed)
	r := &readCampaign{published: map[string]*checkpoint.Index{}, models: map[string]*model{}, deleted: map[string]bool{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.c = newFillCluster(t, fillConfig{hosts: hosts, code: code, share: 100,
			runtime: sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true,
				Network: sim.NetworkConfig{Latency: 200 * time.Microsecond, ConnectLatency: 200 * time.Microsecond,
					TailEvery: 20, TailLatency: 30 * time.Millisecond, SlowPairPerMille: 200,
					SlowPairLatency: 5 * time.Millisecond},
				ObjectStore: sim.ObjectStoreConfig{GetLatency: 2 * time.Millisecond, HeadLatency: time.Millisecond,
					PutLatency: 4 * time.Millisecond, BytesPerSecond: 1 << 40}},
			cache: func(_ int, config *checkpoint.CacheConfig) {
				config.ClusterBound = 2 * time.Millisecond
				config.ClusterStripeTimeout = 150 * time.Millisecond
				config.HeadCheckEvery = 5
			},
		})
		defer r.c.close()
		r.lists = append(r.lists, *r.c.list.Load())
		r.run(t, draw)
		r.c.settle(t)
		r.check(t)
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	return r.c.runtime
}

// run is the campaign's rounds.
func (r *readCampaign) run(t *testing.T, draw sim.Random) {
	c := r.c
	full := *c.list.Load()
	for round := range 6 {
		id := fmt.Sprintf("round-%d", round)
		if round < 2 || draw.Chance(id+"/publish", 0.5) {
			vm := fmt.Sprintf("vm-%d", round)
			publisher := c.hosts[draw.Intn(id+"/publisher", len(c.hosts))]
			if publisher.up {
				index, m, err := publishFromOf(t, publisher.store, vm, fillCampaignPages, noisySector)
				if err != nil {
					t.Errorf("publishing %s from %s: %v", vm, publisher.name, err)
					return
				}
				r.published[vm], r.models[vm] = index, m
				c.settle(t)
			}
		}
		// A deliberate change of the code, once a checkpoint is out under the
		// first, leaves it to be read under the code it was stored under.
		if round >= 2 && len(full.Earlier()) == 0 && draw.Chance(id+"/code", 0.4) {
			after := rank.CodeFor(1 + draw.Intn(id+"/code-for", 6))
			if after != full.Code() {
				changed, err := rank.NewList(after, full.Caches(), full.Code())
				if err != nil {
					t.Error(err)
					return
				}
				t.Logf("%s: the code changes from %s to %s", id, full.Code(), after)
				full = changed
				r.lists = append(r.lists, full)
			}
		}
		// A membership without a disk shifts the ranks of the windows it held.
		// Some hosts read it at once; the rest hold the generation they held
		// until a peer names a newer one.
		served := full
		if draw.Chance(id+"/leave", 0.3) && full.Len() > 1 {
			served = full.Without(full.Caches()[draw.Intn(id+"/gone", full.Len())].Identity)
			r.lists = append(r.lists, served)
		}
		var told []*fillHost
		for _, h := range c.hosts {
			if h.up && draw.Chance(id+"/told/"+h.name, 0.5) {
				told = append(told, h)
			}
		}
		if len(told) == 0 {
			told = c.hosts[:1]
		}
		c.hold(t, served, told...)
		reader := c.hosts[draw.Intn(id+"/victim-reader", len(c.hosts))]
		victim := c.hosts[draw.Intn(id+"/victim", len(c.hosts))]
		if victim != reader {
			from, to := platform.Address(reader.name), victim.address
			switch draw.Intn(id+"/fault", 5) {
			case 0:
				c.runtime.Network().HoldBoth(from, to, time.Now().Add(draw.Duration(id+"/hold", 2*time.Second)))
			case 1:
				c.runtime.Network().Clog(from, to, time.Now().Add(draw.Duration(id+"/clog", time.Second)))
			case 2:
				c.runtime.Network().SetLink(from, to, simLink(draw.Duration(id+"/slow", 40*time.Millisecond)))
				c.runtime.Network().SetLink(to, from, simLink(draw.Duration(id+"/slow-back", 40*time.Millisecond)))
			case 3:
				victim.shut()
			case 4:
				c.restart(t, slices.Index(c.hosts, victim))
			}
		}
		if len(r.published) > 1 && draw.Chance(id+"/delete", 0.2) {
			r.deleteParts(t, draw, id)
		}
		r.burst(t, draw, id)
		for at, h := range c.hosts {
			if !h.up {
				c.restart(t, at)
			}
			c.runtime.Network().ClearLink(platform.Address(reader.name), h.address)
			c.runtime.Network().ClearLink(h.address, platform.Address(reader.name))
		}
		// Long enough for the probes of a host marked down to answer.
		time.Sleep(20 * time.Second)
	}
}

// deleteParts deletes the parts of one published checkpoint from the store,
// as a reclamation that took what a root still reads would.
func (r *readCampaign) deleteParts(t *testing.T, draw sim.Random, id string) {
	vms := slices.Sorted(maps.Keys(r.published))
	vm := vms[draw.Intn(id+"/deleted", len(vms))]
	ref := r.published[vm].Ref()
	parts := fmt.Sprintf("vm/%s/ckpt/%d/part/", ref.VM, ref.Sequence)
	err := platform.ListAll(r.c.ctx(t), r.c.runtime.ObjectStore(), platform.ObjectPrefix{},
		func(object platform.ObjectMetadata) error {
			if !strings.Contains(object.Key.String(), parts) {
				return nil
			}
			return r.c.runtime.ObjectStore().Delete(r.c.ctx(t), platform.DeleteRequest{Key: object.Key})
		})
	if err != nil {
		t.Errorf("deleting %s's parts: %v", vm, err)
	}
	r.mu.Lock()
	r.deleted[vm] = true
	r.mu.Unlock()
}

// burst has many hosts read the same pages at once, and checks what each
// read.
func (r *readCampaign) burst(t *testing.T, draw sim.Random, id string) {
	vms := slices.Sorted(maps.Keys(r.published))
	type read struct {
		host *fillHost
		vm   string
		page uint64
	}
	var reads []read
	for at := range 2 + draw.Intn(id+"/pages", 3) {
		vm := vms[draw.Intn(fmt.Sprintf("%s/vm-%d", id, at), len(vms))]
		page := fillCampaignPages[draw.Intn(fmt.Sprintf("%s/page-%d", id, at), len(fillCampaignPages))]
		for _, h := range r.c.hosts {
			if h.up && draw.Chance(fmt.Sprintf("%s/%s/%d/%s", id, vm, page, h.name), 0.7) {
				reads = append(reads, read{host: h, vm: vm, page: page})
			}
		}
	}
	var readers sync.WaitGroup
	for _, one := range reads {
		readers.Go(func() {
			ctx := sim.WithTask(r.c.ctx(t), fmt.Sprintf("%s/%s/%s/%d", id, one.host.name, one.vm, one.page))
			r.mu.Lock()
			gone := r.deleted[one.vm]
			r.mu.Unlock()
			index, err := one.host.store.Open(ctx, r.published[one.vm].Ref())
			if err != nil {
				t.Errorf("%s opening %s: %v", one.host.name, one.vm, err)
				return
			}
			got := make([]byte, checkpoint.PageSize2MiB)
			if err := one.host.store.Read(ctx, index, "root", one.page*checkpoint.PageSize2MiB, got); err != nil {
				if !gone || !errors.Is(err, checkpoint.ErrCorrupt) {
					t.Errorf("%s reading page %d of %s: %v", one.host.name, one.page, one.vm, err)
				}
				return
			}
			want := r.models[one.vm].contents["root"][one.page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB]
			if !bytes.Equal(got, want) {
				t.Errorf("%s read page %d of %s as other bytes than were published", one.host.name, one.page, one.vm)
			}
		})
	}
	readers.Wait()
}

// check holds the cluster at rest to what reads may leave: every stripe on a
// host, repairs among them, of a window some list the cluster held ranks it
// for, under a code that list was of.
func (r *readCampaign) check(t *testing.T) {
	var codes []rank.Code
	for _, list := range r.lists {
		if !slices.Contains(codes, list.Code()) {
			codes = append(codes, list.Code())
		}
	}
	for vm, index := range r.published {
		ref := index.Ref()
		windows := []rank.Window{segmentWindow(ref)}
		for _, page := range fillCampaignPages {
			windows = append(windows, pageWindow(ref, page))
		}
		for _, window := range windows {
			for _, h := range r.c.hosts {
				for _, code := range codes {
					if held := h.cache.HeldIndices(window, 0, code); len(held) > 0 && !r.everRanked(window, code, h) {
						t.Errorf("%s holds stripes %v of %s of %+v of %s, which no list of that code ranks it for",
							h.name, held, code, window, vm)
					}
				}
			}
		}
	}
}

// everRanked reports whether some list the cluster held under code ranks h
// for window.
func (r *readCampaign) everRanked(window rank.Window, code rank.Code, h *fillHost) bool {
	for _, list := range r.lists {
		if list.Code() == code && slices.ContainsFunc(list.Ranks(window), func(cache rank.Cache) bool {
			return cache.Identity == h.cache.Identity()
		}) {
			return true
		}
	}
	return false
}

// Reads of the cluster survive every fault their sites inject — a wrong
// stripe whose checksum holds, an item damaged on the way, an answer lost, a
// read that reaches its bound at once, an answer taken for a timeout — and the
// network's, the peer server's and the disk's own: stalls, refusals, slow
// links and slow pairs, a heavy tail, lost and restarted hosts, ranks that
// shift, and parts deleted behind the caches. Every read is what was
// published, and every stripe on a host is of a window a list ranked it for.
// Across the seeds every site fires and every probe is reached, because a
// fault nothing drives proves nothing.
func TestClusterReadsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	probes := map[string]uint64{}
	fired := map[string]uint64{}
	for _, seed := range readCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runReadCampaign(t, seed)
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
	for _, name := range checkpoint.ReadSites {
		if fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	for _, name := range readProbes {
		if probes[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}
