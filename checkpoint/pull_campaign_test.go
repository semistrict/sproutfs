package checkpoint_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// pullProbes are the probes the pull campaign must reach.
var pullProbes = []string{
	checkpoint.ProbePresenceHeld, checkpoint.ProbePresenceLacking, checkpoint.ProbePresenceLost,
	checkpoint.ProbePresenceStale, checkpoint.ProbePullHeld, checkpoint.ProbePullFetched,
	checkpoint.ProbePullPressure,
}

// pullCampaignSeeds are the seeds the pull campaign runs, which between them
// fire every site and reach every probe.
var pullCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8}

// pullCampaign is one seed's cluster, what it published, and every list of
// caches it held.
type pullCampaign struct {
	c         *fillCluster
	published map[string]*checkpoint.Index
	models    map[string]*model
	lists     []rank.List
}

// runPullCampaign is one seed: a cluster whose size and code the seed draws,
// on a network with a heavy tail and slow pairs, every completion released by
// the seed's scheduler and the sites on. Each round publishes a checkpoint the
// cluster holds, from a host, or one it lacks, from the publisher; has hosts
// drop some stripes of it; moves the membership with only some hosts told;
// and then has several hosts pull checkpoints at once while others fault on
// their pages, and one host's disk shrink. Every pull ends complete or
// stopped short under pressure, every page reads as it was published, and at
// rest every stripe on a host is of a window some list ranked it for, under
// that list's code: a pull keeps nothing whole inside the share.
func runPullCampaign(t *testing.T, seed uint64) *sim.Runtime {
	draw := sim.New(sim.Config{Seed: seed}).Random("pull-campaign")
	hosts := 2 + draw.Intn("hosts", 6)
	code := rank.CodeFor(max(1, hosts-draw.Intn("spare", 2)))
	t.Logf("seed=%d: %d hosts under %s", seed, hosts, code)
	scheduler := sim.NewScheduler(seed)
	r := &pullCampaign{published: map[string]*checkpoint.Index{}, models: map[string]*model{}}
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
func (r *pullCampaign) run(t *testing.T, draw sim.Random) {
	c := r.c
	full := *c.list.Load()
	for round := range 5 {
		id := fmt.Sprintf("round-%d", round)
		vm := fmt.Sprintf("vm-%d", round)
		store := c.publisher
		if draw.Chance(id+"/filled", 0.6) {
			store = c.hosts[draw.Intn(id+"/publisher", len(c.hosts))].store
		}
		index, m, err := publishFromOf(t, store, vm, fillCampaignPages, noisySector)
		if err != nil {
			t.Errorf("publishing %s: %v", vm, err)
			return
		}
		r.published[vm], r.models[vm] = index, m
		c.settle(t)
		// The cluster loses some stripes of the checkpoint, as eviction and
		// lost hosts leave it.
		ref := index.Ref()
		for _, page := range fillCampaignPages {
			if !draw.Chance(fmt.Sprintf("%s/forget/%d", id, page), 0.4) {
				continue
			}
			var indices []int
			for index := range c.list.Load().Code().Width() {
				if draw.Chance(fmt.Sprintf("%s/forget/%d/%d", id, page, index), 0.5) {
					indices = append(indices, index)
				}
			}
			c.forget(t, pageWindow(ref, page), indices...)
		}
		// A membership without a disk shifts the ranks of the windows it
		// held. Some hosts read it at once; the rest hold the generation they
		// held until a peer names a newer one.
		served := full
		if draw.Chance(id+"/leave", 0.5) && full.Len() > 1 {
			served = full.Without(full.Caches()[draw.Intn(id+"/gone", full.Len())].Identity)
			r.lists = append(r.lists, served)
		}
		var told []*fillHost
		for _, h := range c.hosts {
			if draw.Chance(id+"/told/"+h.name, 0.5) {
				told = append(told, h)
			}
		}
		if len(told) == 0 {
			told = c.hosts[:1]
		}
		c.hold(t, served, told...)
		r.pullAndFault(t, draw, id)
	}
}

// pullAndFault has several hosts pull checkpoints at once while others fault
// on their pages, and one host's disk shrink, and checks each pull's end and
// each page read.
func (r *pullCampaign) pullAndFault(t *testing.T, draw sim.Random, id string) {
	c := r.c
	vms := slices.Sorted(maps.Keys(r.published))
	var work sync.WaitGroup
	for _, h := range c.hosts {
		name := fmt.Sprintf("%s/%s", id, h.name)
		switch does := draw.Intn(name+"/does", 3); does {
		case 0, 2:
			if does == 2 {
				// The host's disk shrinks while it pulls.
				work.Go(func() {
					time.Sleep(draw.Duration(name+"/shrink", 10*time.Millisecond))
					if err := h.cache.FitDisk(sim.WithTask(c.ctx(t), name+"/shrink")); err != nil {
						t.Errorf("%s shrinking its disk: %v", h.name, err)
					}
				})
			}
			vm := vms[draw.Intn(name+"/pulls", len(vms))]
			work.Go(func() {
				ctx := sim.WithTask(c.ctx(t), name+"/pull")
				index, err := h.store.Open(ctx, r.published[vm].Ref())
				if err != nil {
					t.Errorf("%s opening %s: %v", h.name, vm, err)
					return
				}
				pull, err := h.store.Pull(ctx, index)
				if err != nil {
					t.Errorf("%s pulling %s: %v", h.name, vm, err)
					return
				}
				defer pull.Close()
				err = pull.Wait(ctx)
				stats := pull.Stats()
				switch {
				case errors.Is(err, checkpoint.ErrPressure):
				case err != nil:
					t.Errorf("%s's pull of %s ended with %v", h.name, vm, err)
				case stats.Pulled != stats.Bytes || stats.Held+stats.Fetched < stats.Bytes:
					t.Errorf("%s's pull of %s came to %+v; a complete pull dealt with every byte", h.name, vm, stats)
				}
			})
		case 1:
			vm := vms[draw.Intn(name+"/faults", len(vms))]
			page := fillCampaignPages[draw.Intn(name+"/page", len(fillCampaignPages))]
			work.Go(func() {
				ctx := sim.WithTask(c.ctx(t), name+"/fault")
				index, err := h.store.Open(ctx, r.published[vm].Ref())
				if err != nil {
					t.Errorf("%s opening %s: %v", h.name, vm, err)
					return
				}
				got := make([]byte, checkpoint.PageSize2MiB)
				if err := h.store.Read(ctx, index, "root", page*checkpoint.PageSize2MiB, got); err != nil {
					t.Errorf("%s reading page %d of %s: %v", h.name, page, vm, err)
					return
				}
				want := r.models[vm].contents["root"][page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB]
				if !bytes.Equal(got, want) {
					t.Errorf("%s read page %d of %s as other bytes than were published", h.name, page, vm)
				}
			})
		}
	}
	work.Wait()
	c.settle(t)
}

// check holds the cluster at rest to what pulls may leave: every stripe on a
// host of a window some list the cluster held ranks it for, under that list's
// code. A whole copy, under 1+0, is of no list's code.
func (r *pullCampaign) check(t *testing.T) {
	for vm, index := range r.published {
		ref := index.Ref()
		windows := []rank.Window{segmentWindow(ref)}
		for _, page := range fillCampaignPages {
			windows = append(windows, pageWindow(ref, page))
		}
		for _, window := range windows {
			for _, h := range r.c.hosts {
				whole := rank.Code{K: 1}
				if code := r.lists[0].Code(); code != whole && len(h.cache.HeldIndices(window, 0, whole)) > 0 {
					t.Errorf("%s holds %+v of %s whole under %s", h.name, window, vm, code)
				}
				held := h.cache.HeldIndices(window, 0, r.lists[0].Code())
				if len(held) > 0 && !slices.ContainsFunc(r.lists, func(list rank.List) bool {
					return slices.ContainsFunc(list.Ranks(window), func(cache rank.Cache) bool {
						return cache.Identity == h.cache.Identity()
					})
				}) {
					t.Errorf("%s holds stripes %v of %+v of %s, which no list ranks it for", h.name, held, window, vm)
				}
			}
		}
	}
	// Every page of every checkpoint reads as published on every host.
	for vm, index := range r.published {
		for _, h := range r.c.hosts {
			opened, err := h.store.Open(r.c.ctx(t), index.Ref())
			if err != nil {
				t.Errorf("%s opening %s: %v", h.name, vm, err)
				continue
			}
			for _, page := range fillCampaignPages {
				got := make([]byte, checkpoint.PageSize2MiB)
				if err := h.store.Read(r.c.ctx(t), opened, "root", page*checkpoint.PageSize2MiB, got); err != nil {
					t.Errorf("%s reading page %d of %s at rest: %v", h.name, page, vm, err)
					continue
				}
				want := r.models[vm].contents["root"][page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB]
				if !bytes.Equal(got, want) {
					t.Errorf("%s read page %d of %s at rest as other bytes than were published", h.name, page, vm)
				}
			}
		}
	}
}

// Pulls survive every fault their sites inject — a presence answer lost, and
// pressure that stops a pull before one of its fetches — and the network's,
// the peer server's, the fills' and the reads' own: a heavy tail, slow pairs,
// stripes lost, ranks that shift under hosts that have not read the new
// membership, faults on the pulled pages and disks that shrink. Every pull
// ends complete or stopped short under pressure, every page reads as it was
// published, and no host holds a stripe no list ranked it for. Across the
// seeds every site fires and every probe is reached.
func TestPullsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	probes := map[string]uint64{}
	fired := map[string]uint64{}
	for _, seed := range pullCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runPullCampaign(t, seed)
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
	for _, name := range checkpoint.PullSites {
		if fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	for _, name := range pullProbes {
		if probes[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}
