package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// hotCampaignSeeds are the seeds the hot tier campaign runs, which between
// them fire every site and reach every probe.
var hotCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// hotCampaign is one seed's hosts, each a store with a hot tier of its own
// over the one regional bucket and the one hot bucket, what they published,
// and the VMs whose parts were deleted behind the hot tier's back.
type hotCampaign struct {
	runtime *sim.Runtime
	draw    sim.Random
	hot     *sim.ObjectStore
	tiers   []*checkpoint.HotTier
	stores  []*checkpoint.Store
	// published is every checkpoint, and objects every object a publication
	// wrote, with its bytes.
	published map[string]*checkpoint.Index
	models    map[string]*model
	objects   map[string][]byte
	deleted   map[string]bool
	mu        sync.Mutex
}

// wholeGets is a host's view of the regional bucket, which fails a GET of a
// whole object, which only a fill makes, as often as the seed says. A read
// of a range is never failed: a read the regional bucket fails is a read
// that fails, and that is not the hot tier's to cause.
type wholeGets struct {
	platform.ObjectStore
	draw  sim.Random
	host  string
	count atomic.Int64
}

func (s *wholeGets) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if request.Range == nil &&
		s.draw.Chance(fmt.Sprintf("%s/whole-get/%s/%d", s.host, request.Key, s.count.Add(1)), 0.2) {
		return platform.GetResult{}, platform.ErrInjectedFault
	}
	return s.ObjectStore.Get(ctx, request)
}

// runHotCampaign is one seed: three to five hosts, every completion released
// by the seed's scheduler and the sites on. Each round publishes from any
// host, some of which write their publications to the hot tier and some of
// which do not, and has many hosts read the same pages at once, while the
// seed takes the hot bucket down for the round or deletes a checkpoint's
// parts from the regional bucket. Every read is what was published, or fails
// only for a part that is gone. At rest, every object the hot bucket holds is
// what was published under its name, or the first part of it.
func runHotCampaign(t *testing.T, seed uint64) *sim.Runtime {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true,
		ObjectStore: sim.ObjectStoreConfig{GetLatency: 5 * time.Millisecond, HeadLatency: 2 * time.Millisecond,
			PutLatency: 8 * time.Millisecond, BytesPerSecond: 1 << 40}})
	c := &hotCampaign{runtime: runtime, draw: runtime.Random("hot-campaign"),
		hot: runtime.NewObjectStore("hot", sim.ObjectStoreConfig{GetLatency: 500 * time.Microsecond,
			HeadLatency: 500 * time.Microsecond, PutLatency: time.Millisecond}),
		published: map[string]*checkpoint.Index{}, models: map[string]*model{}, objects: map[string][]byte{},
		deleted: map[string]bool{}}
	hosts := 3 + c.draw.Intn("hosts", 3)
	t.Logf("seed=%d: %d hosts", seed, hosts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		for at := range hosts {
			name := fmt.Sprintf("host-%d", at)
			// The first host's queue holds an index object and no part beside
			// it, and the second's rate never has room for a whole part of
			// four incompressible 2 MiB pages. The rest have room for both.
			config := checkpoint.HotTierConfig{Store: c.hot, Bound: 20 * time.Millisecond, QueueBytes: 64 << 20,
				BytesPerSecond: 64 << 20, SkipPublications: at%2 == 1, HeadCheckEvery: 4}
			switch at {
			case 0:
				config.QueueBytes = 1 << 20
			case 1:
				config.BytesPerSecond = 4 << 20
			}
			tier, err := checkpoint.NewHotTier(ctx, config)
			if err != nil {
				t.Error(err)
				return
			}
			defer tier.Close()
			c.tiers = append(c.tiers, tier)
			c.stores = append(c.stores, mustStore(t, checkpoint.Config{
				ObjectStore: &wholeGets{ObjectStore: runtime.ObjectStore(), draw: c.draw, host: name}, HotTier: tier}))
		}
		c.run(t, ctx)
		c.settle(t)
		c.check(t, ctx)
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	return runtime
}

// run is the campaign's rounds.
func (c *hotCampaign) run(t *testing.T, ctx context.Context) {
	for round := range 6 {
		id := fmt.Sprintf("round-%d", round)
		if round < 2 || c.draw.Chance(id+"/publish", 0.5) {
			vm := fmt.Sprintf("vm-%d", round)
			publisher := c.draw.Intn(id+"/publisher", len(c.stores))
			index, m, err := publishFromOf(t, c.stores[publisher], vm, fillCampaignPages, noisySector)
			if err != nil {
				t.Errorf("publishing %s from host-%d: %v", vm, publisher, err)
				return
			}
			c.published[vm], c.models[vm] = index, m
			c.record(t, ctx, vm)
			c.settle(t)
		}
		down := c.draw.Chance(id+"/down", 0.25)
		if down {
			c.hot.Fail()
		}
		if len(c.published) > 1 && c.draw.Chance(id+"/delete", 0.3) {
			c.deleteParts(t, ctx, id)
		}
		c.burst(t, ctx, id)
		if down {
			c.hot.Recover()
		}
		c.settle(t)
		// Long enough for a hot tier marked down to be tried again.
		time.Sleep(11 * time.Second)
	}
}

// record keeps the bytes of every object vm's publication wrote.
func (c *hotCampaign) record(t *testing.T, ctx context.Context, vm string) {
	err := platform.ListAll(ctx, c.runtime.ObjectStore(), platform.ObjectPrefix{},
		func(object platform.ObjectMetadata) error {
			if !strings.Contains(object.Key.String(), "/vm/"+vm+"/") {
				return nil
			}
			data, _, err := platform.ReadObject(ctx, c.runtime.ObjectStore(), object.Key, 0, 1<<30, checkpoint.ErrCorrupt)
			c.objects[object.Key.String()] = data
			return err
		})
	if err != nil {
		t.Errorf("recording what %s published: %v", vm, err)
	}
}

// settle waits for every host's fills.
func (c *hotCampaign) settle(t *testing.T) {
	for _, tier := range c.tiers {
		if err := tier.Settle(t.Context()); err != nil {
			t.Error(err)
		}
	}
}

// deleteParts deletes the parts of one published checkpoint from the
// regional bucket, as a reclamation that took what a root still reads would.
func (c *hotCampaign) deleteParts(t *testing.T, ctx context.Context, id string) {
	vms := slices.Sorted(maps.Keys(c.published))
	vm := vms[c.draw.Intn(id+"/deleted", len(vms))]
	ref := c.published[vm].Ref()
	parts := fmt.Sprintf("vm/%s/ckpt/%d/part/", ref.VM, ref.Sequence)
	err := platform.ListAll(ctx, c.runtime.ObjectStore(), platform.ObjectPrefix{},
		func(object platform.ObjectMetadata) error {
			if !strings.Contains(object.Key.String(), parts) {
				return nil
			}
			return c.runtime.ObjectStore().Delete(ctx, platform.DeleteRequest{Key: object.Key})
		})
	if err != nil {
		t.Errorf("deleting %s's parts: %v", vm, err)
	}
	c.mu.Lock()
	c.deleted[vm] = true
	c.mu.Unlock()
}

// burst has many hosts read the same pages at once, and checks what each
// read.
func (c *hotCampaign) burst(t *testing.T, ctx context.Context, id string) {
	vms := slices.Sorted(maps.Keys(c.published))
	type read struct {
		host int
		vm   string
		page uint64
	}
	var reads []read
	for at := range 2 + c.draw.Intn(id+"/pages", 3) {
		vm := vms[c.draw.Intn(fmt.Sprintf("%s/vm-%d", id, at), len(vms))]
		page := fillCampaignPages[c.draw.Intn(fmt.Sprintf("%s/page-%d", id, at), len(fillCampaignPages))]
		for host := range c.stores {
			if c.draw.Chance(fmt.Sprintf("%s/%s/%d/%d", id, vm, page, host), 0.7) {
				reads = append(reads, read{host: host, vm: vm, page: page})
			}
		}
	}
	var readers sync.WaitGroup
	for at, one := range reads {
		readers.Go(func() {
			ctx := sim.WithTask(ctx, fmt.Sprintf("%s/%d/host-%d/%s/%d", id, at, one.host, one.vm, one.page))
			c.mu.Lock()
			gone := c.deleted[one.vm]
			c.mu.Unlock()
			store := c.stores[one.host]
			index, err := store.Open(ctx, c.published[one.vm].Ref())
			if err != nil {
				t.Errorf("host-%d opening %s: %v", one.host, one.vm, err)
				return
			}
			got := make([]byte, checkpoint.PageSize2MiB)
			if err := store.Read(ctx, index, "root", one.page*checkpoint.PageSize2MiB, got); err != nil {
				if !gone || !errors.Is(err, checkpoint.ErrCorrupt) {
					t.Errorf("host-%d reading page %d of %s: %v", one.host, one.page, one.vm, err)
				}
				return
			}
			want := c.models[one.vm].contents["root"][one.page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB]
			if !bytes.Equal(got, want) {
				t.Errorf("host-%d read page %d of %s as other bytes than were published", one.host, one.page, one.vm)
			}
		})
	}
	readers.Wait()
}

// check holds the hot bucket at rest to what fills may leave: every object in
// it is what a publication wrote under its name, or the first part of that,
// which is what the site that has the bucket keep half a fill leaves.
func (c *hotCampaign) check(t *testing.T, ctx context.Context) {
	err := platform.ListAll(ctx, c.hot, platform.ObjectPrefix{}, func(object platform.ObjectMetadata) error {
		want, published := c.objects[object.Key.String()]
		data, _, err := platform.ReadObject(ctx, c.hot, object.Key, 0, 1<<30, checkpoint.ErrCorrupt)
		if err != nil {
			return err
		}
		if !published || !bytes.HasPrefix(want, data) || len(data) == 0 {
			t.Errorf("the hot bucket holds %d bytes under %s that no publication wrote there", len(data), object.Key)
		}
		return nil
	})
	if err != nil {
		t.Errorf("listing the hot bucket: %v", err)
	}
}

// Reads through a hot tier survive every fault its sites inject — a bucket
// that is down, slow past the bound, refusing a fill, losing a reply, or
// cutting a reply or a fill short — and the campaign's own: the hot bucket
// down for a round, a regional GET of a fill that fails, and parts deleted
// from the regional bucket behind the hot tier. Every read is what was
// published, or fails only for a part that is gone, and the hot bucket holds
// nothing a publication did not write. Across the seeds every site fires and
// every probe is reached, because a fault nothing drives proves nothing.
func TestHotTierSurvivesItsFaultsAndReachesItsProbes(t *testing.T) {
	probes := map[string]uint64{}
	fired := map[string]uint64{}
	for _, seed := range hotCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runHotCampaign(t, seed)
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
	for _, name := range checkpoint.HotTierSites() {
		if fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	for _, name := range checkpoint.HotTierProbes() {
		if probes[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}
