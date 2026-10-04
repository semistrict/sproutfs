package simtest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// shardProbes is every probe the shard campaign must reach across its seeds.
// A shard fenced while a process holds it is not among them: the cloud here
// takes a detached disk from every process of its machine, as Compute Engine
// does, so no member that lost a shard still holds its device, and the fence
// is reached where a test keeps two handles of one device
// (TestAStaleMemberThatStillHoldsTheDeviceIsFenced in checkpoint).
var shardProbes = []string{
	host.ProbeShardOpened,
	host.ProbeShardClosed,
	host.ProbeShardNotAttached,
	host.ProbeShardLost,
	checkpoint.ProbeShardLeaseRefused,
}

// shardSites is every site the shard campaign must fire across its seeds: the
// cloud's and the hosts'.
var shardSites = append(sim.NetworkDiskSites(), "host/shard-open-fails", "host/shard-close-slow")

// shardCampaign is one seed of the campaign: a world of six hosts and six
// shards under 4+2, driven through a seeded schedule of the things that
// happen to shards, with the sites on.
type shardCampaign struct {
	t       *testing.T
	ctx     context.Context
	runtime *sim.Runtime
	world   *simtest.World
	choose  sim.Random
	step    int
	// down is the hosts the schedule has stopped, which a join starts again.
	down []int
}

func (c *shardCampaign) up() []int {
	var up []int
	for index := range c.world.Hosts() {
		if !slices.Contains(c.down, index) {
			up = append(up, index)
		}
	}
	return up
}

func (c *shardCampaign) pick(name string, from []int) int {
	return from[c.choose.Intn(fmt.Sprintf("%d/%s", c.step, name), len(from))]
}

// memberships is the membership the world's store holds.
func (c *shardCampaign) membership() membership.Membership {
	c.t.Helper()
	store, err := membership.NewStore(membership.Config{ObjectStore: c.runtime.ObjectStore(),
		ObjectPrefix: c.world.Prefix()})
	if err != nil {
		c.t.Fatal(err)
	}
	for {
		m, err := store.Read(c.ctx)
		if err == nil {
			return m
		}
		// The read-fails site of the store is on: read again.
	}
}

// read opens the VM on a host that is up, reads every page back, and
// suspends it there again, which publishes and fills.
func (c *shardCampaign) read() {
	c.t.Helper()
	index := c.pick("reader", c.up())
	if err := c.world.Start(c.ctx, "vm-0", index); err != nil {
		c.t.Fatalf("step %d: starting the VM on host-%d: %v", c.step, index, err)
	}
	if err := c.world.Verify(c.ctx, simtest.ReadsMustSucceed); err != nil {
		c.t.Fatalf("step %d: the VM opened on host-%d reads back something its guest did not write: %v", c.step,
			index, err)
	}
	if err := c.world.StoreAll("vm-0", byte(c.step)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.world.Suspend(c.ctx, "vm-0"); err != nil {
		c.t.Fatalf("step %d: suspending the VM: %v", c.step, err)
	}
}

// leave takes a host out, as the autoscaler removes its machine: its shards
// move off it while it runs, and then it stops.
func (c *shardCampaign) leave() {
	index := c.pick("leaving", c.up())
	c.world.Unlist(c.ctx, index)
	if err := c.world.Shutdown(c.ctx, index); err != nil {
		c.t.Fatal(err)
	}
	c.down = append(c.down, index)
}

// kill loses a host with its shards open on its machine.
func (c *shardCampaign) kill() {
	index := c.pick("killed", c.up())
	if err := c.world.Kill(c.ctx, index, sim.CrashProcess); err != nil {
		c.t.Fatal(err)
	}
	c.down = append(c.down, index)
}

// dieMidMove has a host leave, and kills the member one of its shards is
// assigned to before that member serves it.
func (c *shardCampaign) dieMidMove() {
	c.t.Helper()
	leaving := c.pick("leaving", c.up())
	c.world.Leaving(leaving)
	for range 16 {
		c.world.ShardPass(c.ctx)
		m := c.membership()
		for _, disk := range m.Disks() {
			if disk.State != membership.Attaching {
				continue
			}
			for _, index := range c.up() {
				if self, _ := c.world.Host(index).Member(); self.ID == disk.Member && index != leaving {
					if err := c.world.Kill(c.ctx, index, sim.CrashProcess); err != nil {
						c.t.Fatal(err)
					}
					c.down = append(c.down, index)
					return
				}
			}
		}
	}
}

// join starts a host that the schedule stopped.
func (c *shardCampaign) join() {
	index := c.pick("joining", c.down)
	if err := c.world.Restart(c.ctx, index); err != nil {
		c.t.Fatal(err)
	}
	c.down = slices.DeleteFunc(c.down, func(down int) bool { return down == index })
}

// yank detaches a shard from its machine under its host, as an operator or
// the cloud does by hand: its host finds the device gone and closes it, and
// the controller attaches it there again.
func (c *shardCampaign) yank() {
	volumes := c.world.ShardVolumes()
	volume := volumes[c.choose.Intn(fmt.Sprintf("%d/yanked", c.step), len(volumes))]
	if machine := c.world.Cloud().Attached(volume); machine != "" {
		if err := c.world.Cloud().Detach(c.ctx, volume, machine); err != nil {
			c.t.Logf("step %d: detaching %s by hand: %v", c.step, volume, err)
		}
	}
}

// stale has a process open a shard's device beside its member, under its
// member's assignment, as a member the cluster lost track of would hold it;
// the shard then moves. That process can no longer use the shard, and a
// process of the old member that opens the device again, as one that still
// reaches it would, is refused by the lease.
func (c *shardCampaign) stale() {
	c.t.Helper()
	m := c.membership()
	var disk membership.Disk
	owner := -1
	for _, listed := range m.Disks() {
		for _, index := range c.up() {
			if self, _ := c.world.Host(index).Member(); listed.State == membership.Serving && self.ID == listed.Member {
				disk, owner = listed, index
			}
		}
		if owner >= 0 {
			break
		}
	}
	if owner < 0 {
		return
	}
	open := func() platform.File {
		device, err := c.world.Cloud().Disk(disk.Volume).Open(c.ctx, "device", platform.OpenOptions{})
		if err != nil {
			c.t.Fatal(err)
		}
		c.t.Cleanup(func() { _ = device.Close() })
		return device
	}
	stale := c.staleCache(m, disk.Member)
	lease := checkpoint.Lease{Assigned: disk.Assigned, Member: disk.Member}
	if err := stale.AddShard(c.ctx, c.shardOf(open(), disk.ID, lease)); err != nil {
		c.t.Logf("step %d: the stale process could not open %s: %v", c.step, disk.Volume, err)
		return
	}
	c.world.Unlist(c.ctx, owner)
	if err := c.world.Shutdown(c.ctx, owner); err != nil {
		c.t.Fatal(err)
	}
	c.down = append(c.down, owner)
	if moved, _ := c.membership().Disk(disk.ID); moved.State != membership.Serving || moved.Member == disk.Member {
		return
	}
	// The cloud took the device from every process of the machine it
	// detached it from, so the process's handle is gone; the lease would
	// fence it otherwise (checkpoint's TestAStaleMemberThatStillHoldsTheDeviceIsFenced).
	if err := stale.CheckShard(c.ctx, disk.ID); err == nil {
		c.t.Fatalf("step %d: a process that held shard %s before it moved still checks its lease as its own",
			c.step, disk.Volume)
	}
	again := c.staleCache(m, disk.Member)
	if err := again.AddShard(c.ctx, c.shardOf(open(), disk.ID, lease)); !errors.Is(err, checkpoint.ErrFenced) {
		c.t.Fatalf("step %d: opening shard %s under the assignment it moved from: %v, want ErrFenced", c.step,
			disk.Volume, err)
	}
}

func (c *shardCampaign) staleCache(m membership.Membership, member rank.Identity) *checkpoint.Cache {
	c.t.Helper()
	budget, err := resource.New(1 << 20)
	if err != nil {
		c.t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(c.ctx, budget, checkpoint.CacheConfig{Shards: true,
		DiskRegionBytes: 8 << 20, ClusterPercent: 100})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(cache.Close)
	cache.FollowMembership(membership.NewFixed(m), member)
	return cache
}

// shardBudget is a stale process's share of a shard: the world's shard less
// its header region.
type shardBudget int64

func (b shardBudget) Share() int64                         { return int64(b) }
func (shardBudget) Admit(int64, checkpoint.WriteKind) bool { return true }

func (c *shardCampaign) shardOf(device platform.File, id rank.Identity, lease checkpoint.Lease) checkpoint.ShardConfig {
	return checkpoint.ShardConfig{Device: device, Identity: id, Budget: shardBudget(256<<20 - 8<<20), Lease: lease}
}

// run is the schedule: a dozen steps, each a seeded choice among what can
// happen to shards, then a settle and a read.
func (c *shardCampaign) run() {
	c.t.Helper()
	if err := c.world.StoreAll("vm-0", 1); err != nil {
		c.t.Fatal(err)
	}
	if err := c.world.Suspend(c.ctx, "vm-0"); err != nil {
		c.t.Fatal(err)
	}
	for c.step = 1; c.step <= 12; c.step++ {
		var events []string
		if len(c.up()) > 3 {
			events = append(events, "leave", "kill", "die-mid-move", "stale")
		}
		if len(c.down) > 0 {
			events = append(events, "join")
		}
		events = append(events, "yank", "read")
		event := events[c.choose.Intn(fmt.Sprintf("%d/event", c.step), len(events))]
		c.t.Logf("step %d: %s, %d hosts up", c.step, event, len(c.up()))
		switch event {
		case "leave":
			c.leave()
		case "kill":
			c.kill()
		case "die-mid-move":
			c.dieMidMove()
		case "join":
			c.join()
		case "yank":
			c.yank()
		case "stale":
			c.stale()
		}
		if err := c.world.Settle(c.ctx); err != nil {
			c.t.Fatalf("step %d: settling: %v", c.step, err)
		}
		c.read()
	}
}

// The shard campaign: shards move as hosts leave, die, die while a shard
// moves to them, join, have a shard yanked from under them, and keep a stale
// process holding a shard's device, with every site of the cloud and the
// hosts' shards on. The guest reads back every page it wrote after every
// step, and across the seeds every site fires and every shard probe is
// reached.
func TestShardsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	reached, fired := map[string]uint64{}, map[string]uint64{}
	for seed := uint64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := newCampaignRuntime(seed, true)
				ctx := sim.WithRuntime(t.Context(), runtime)
				c := &shardCampaign{t: t, ctx: ctx, runtime: runtime, world: shardWorld(t, ctx, runtime),
					choose: runtime.Random("simtest/shards")}
				c.run()
				if err := c.world.Close(ctx); err != nil {
					t.Fatal(err)
				}
				for name, count := range runtime.Probes() {
					reached[name] += count
				}
				for site, count := range runtime.FiredSites() {
					fired[site] += count
				}
			})
		})
	}
	for _, probe := range shardProbes {
		if reached[probe] == 0 {
			t.Errorf("no seed reached %s", probe)
		}
	}
	for _, site := range shardSites {
		if fired[site] == 0 {
			t.Errorf("no seed fired %s", site)
		}
	}
}
