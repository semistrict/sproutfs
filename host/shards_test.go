package host_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// shardCluster is hosts that serve shards, each on a machine of its own, a
// cloud of network disks, and a controller over both, in one simulated world.
type shardCluster struct {
	t       *testing.T
	ctx     context.Context
	runtime *sim.Runtime
	cloud   *sim.NetworkDisks
	control *membership.ShardControl
	hosts   map[string]*shardHost
	order   []string
	// started counts the hosts started, each with entropy of its own, as
	// every process draws its own.
	started int
}

// shardHost is one host: its machine, its clock, and whether it is leaving.
type shardHost struct {
	machine string
	clock   *sim.Clock
	host    *host.Host
	leaving bool
}

const testShardBytes = 32 << 30

func newShardCluster(t *testing.T, shards int) *shardCluster {
	t.Helper()
	runtime := sim.New(sim.Config{})
	c := &shardCluster{t: t, ctx: sim.WithRuntime(t.Context(), runtime), runtime: runtime,
		cloud: runtime.NewNetworkDisks(sim.NetworkDisksConfig{AttachLatency: 2 * time.Second,
			DetachLatency: time.Second}), hosts: map[string]*shardHost{}}
	store, err := membership.NewStore(membership.Config{ObjectStore: runtime.ObjectStore(),
		Entropy: runtime.NewEntropy("controller")})
	if err != nil {
		t.Fatal(err)
	}
	c.control = &membership.ShardControl{Store: store, Disks: c.cloud}
	for n := range shards {
		volume := fmt.Sprintf("shard-%d", n)
		if err := c.cloud.Create(c.ctx, volume, testShardBytes); err != nil {
			t.Fatal(err)
		}
		c.control.Volumes = append(c.control.Volumes, volume)
	}
	return c
}

// start starts a host on machine, which the cluster steps.
func (c *shardCluster) start(machine string) *shardHost {
	c.t.Helper()
	h := c.startOn(machine, machine)
	c.hosts[machine] = h
	c.order = append(c.order, machine)
	return h
}

// startOn starts a host called name on machine, its peer server at
// name-pages, and leaves it to the test to step.
func (c *shardCluster) startOn(name, machine string) *shardHost {
	c.t.Helper()
	c.started++
	h := &shardHost{machine: machine, clock: c.runtime.NewClock(fmt.Sprintf("%s/%d", name, c.started))}
	config := host.Config{Network: c.runtime.Network(), Resources: testresource.New(),
		ObjectStore: c.runtime.ObjectStore(), Clock: h.clock,
		Entropy:            c.runtime.NewEntropy(fmt.Sprintf("%s/%d", name, c.started)),
		Cache:              checkpoint.CacheConfig{DiskRegionBytes: 8 << 20, ClusterPercent: 100},
		Shards:             host.ShardsConfig{Devices: c.cloud.Devices(machine), Machine: machine},
		Migration:          host.MigrationConfig{Address: platform.Address(name + "-pages")},
		MembershipInterval: -1, CheckpointInterval: -1, EpochInterval: -1}
	started, err := host.StartHost(c.ctx, config)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = started.Close(context.WithoutCancel(c.ctx)) })
	h.host = started
	return h
}

// stop closes a host, as its pod ending does.
func (c *shardCluster) stop(machine string) {
	c.t.Helper()
	if err := c.hosts[machine].host.Close(context.WithoutCancel(c.ctx)); err != nil {
		c.t.Fatal(err)
	}
	delete(c.hosts, machine)
	c.order = slices.DeleteFunc(c.order, func(name string) bool { return name == machine })
}

// want is every host as it reports itself now.
func (c *shardCluster) want() membership.Want {
	want := membership.Want{Code: rank.Code{K: 2, M: 1}}
	for _, machine := range c.order {
		h := c.hosts[machine]
		self, _ := h.host.Member()
		self.Leaving = h.leaving
		want.Hosts = append(want.Hosts, self)
	}
	return want
}

// pass is a second on every host's clock, one pass of the controller, and
// every host reading the membership; it reports the generation it left.
func (c *shardCluster) pass() uint64 {
	c.t.Helper()
	for _, h := range c.hosts {
		h.clock.Advance(time.Second)
	}
	synctest.Wait()
	m, _, err := c.control.Pass(c.ctx, c.want())
	if err != nil {
		c.t.Logf("a pass of the controller: %v", err)
	}
	for _, h := range c.hosts {
		if err := h.host.RefreshMembership(c.ctx); err != nil {
			c.t.Logf("%s reading the membership: %v", h.machine, err)
		}
	}
	synctest.Wait()
	return m.Generation()
}

// settle passes until the membership has not moved for five passes and
// every shard serves on a host that holds it; it reports the passes taken.
func (c *shardCluster) settle() int {
	c.t.Helper()
	still, last := 0, uint64(0)
	for passes := 1; passes <= 300; passes++ {
		generation := c.pass()
		if generation == last {
			still++
		} else {
			still, last = 0, generation
		}
		if still >= 5 && c.serving() {
			return passes
		}
	}
	m, _ := c.control.Store.Read(c.ctx)
	c.t.Fatalf("the shards never settled at generation %d: %+v", m.Generation(), m.Disks())
	return 0
}

// serving reports every shard serving on a host that holds it open, and no
// shard held by two hosts.
func (c *shardCluster) serving() bool {
	m, err := c.control.Store.Read(c.ctx)
	if err != nil {
		return false
	}
	holders := map[rank.Identity]int{}
	for _, h := range c.hosts {
		self, _ := h.host.Member()
		for _, disk := range self.Disks {
			holders[disk.ID]++
		}
	}
	for _, volume := range c.control.Volumes {
		id := membership.ShardIdentity(volume)
		disk, listed := m.Disk(id)
		if holders[id] > 1 {
			c.t.Fatalf("shard %s is held by %d hosts", volume, holders[id])
		}
		if !listed || disk.State != membership.Serving || holders[id] != 1 {
			return false
		}
	}
	return true
}

// held is the volumes of the shards each host holds open.
func (c *shardCluster) held() map[string][]string {
	held := map[string][]string{}
	for _, machine := range c.order {
		self, _ := c.hosts[machine].host.Member()
		for _, disk := range self.Disks {
			held[machine] = append(held[machine], disk.Volume)
		}
	}
	return held
}

// Hosts serve the shards the membership assigns them, each opened on the
// machine the cloud attached it to; when a host leaves, as a pod the
// autoscaler removes does, its shards move to the others, released, closed,
// detached and opened again there, and a host that joins takes its share.
func TestHostsServeTheShardsAssignedThemAndMoveThemWhenOneLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newShardCluster(t, 6)
		for _, machine := range []string{"machine-a", "machine-b", "machine-c"} {
			c.start(machine)
		}
		c.settle()
		for machine, volumes := range c.held() {
			if len(volumes) != 2 {
				t.Fatalf("%s holds %v, want two shards", machine, volumes)
			}
			for _, volume := range volumes {
				if got := c.cloud.Attached(volume); got != machine {
					t.Fatalf("%s holds %s attached to %q", machine, volume, got)
				}
			}
		}
		status := c.hosts["machine-a"].host.Status()
		if len(status.Cache.Shards) != 2 || !status.Cache.Disk.Identity.IsZero() {
			t.Fatalf("machine-a's cache reports shards %+v and a disk of its own %s", status.Cache.Shards,
				status.Cache.Disk.Identity)
		}
		moving := c.held()["machine-b"]
		c.hosts["machine-b"].leaving = true
		c.settle()
		if held := c.held(); len(held["machine-b"]) != 0 || len(held["machine-a"])+len(held["machine-c"]) != 6 {
			t.Fatalf("after machine-b left the shards are held %v", held)
		}
		for _, volume := range moving {
			if got := c.cloud.Attached(volume); got == "machine-b" || got == "" {
				t.Fatalf("shard %s moved off machine-b is attached to %q", volume, got)
			}
		}
		c.stop("machine-b")
		c.settle()
		c.start("machine-d")
		c.settle()
		for machine, volumes := range c.held() {
			if len(volumes) != 2 {
				t.Fatalf("after machine-d joined %s holds %v, want two shards", machine, volumes)
			}
		}
		if c.runtime.Probes()[host.ProbeShardClosed] == 0 || c.runtime.Probes()[host.ProbeShardOpened] < 8 {
			t.Fatalf("the shards opened and closed %v", c.runtime.Probes())
		}
	})
}

// A host started again is a new member: it takes none of the assignments the
// process before it held, and the shards it held move through release, let
// go and assignment like any other.
func TestAHostStartedAgainIsANewMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newShardCluster(t, 2)
		first := c.start("machine-a")
		c.settle()
		before, _ := first.host.Member()
		c.stop("machine-a")
		again := c.start("machine-a")
		after, _ := again.host.Member()
		if after.ID == before.ID {
			t.Fatal("a host started again took its predecessor's identity")
		}
		c.settle()
		m, err := c.control.Store.Read(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, disk := range m.Disks() {
			if disk.Member != after.ID || disk.State != membership.Serving {
				t.Fatalf("shard %s is %s for %s, want serving for the new member", disk.Volume, disk.State, disk.Member)
			}
		}
		if _, listed := m.Member(before.ID); listed {
			t.Fatal("the process before is still a member")
		}
	})
}

// update makes one change of the membership, as a controller does.
func (c *shardCluster) update(change func(membership.Membership) (membership.Membership, error)) membership.Membership {
	c.t.Helper()
	m, err := c.control.Store.Update(c.ctx, change)
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

// A host opens a shard only while the object, read again just before, still
// assigns it there under the generation it holds: two hosts share a machine,
// a shard assigned to the first moves to the second before the first opened
// it, and the first, behind, finds the device on its machine and leaves it.
func TestAHostOpensAShardOnlyWhileTheObjectStillAssignsItThere(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newShardCluster(t, 1)
		a, b := c.startOn("host-a", "machine-m"), c.startOn("host-b", "machine-m")
		selfA, _ := a.host.Member()
		selfB, _ := b.host.Member()
		shard := membership.Disk{ID: membership.ShardIdentity("shard-0"), Volume: "shard-0", Weight: 2}
		c.update(func(m membership.Membership) (membership.Membership, error) {
			return m.Join(membership.Member{ID: selfA.ID, Address: selfA.Address})
		})
		c.update(func(m membership.Membership) (membership.Membership, error) {
			return m.Join(membership.Member{ID: selfB.ID, Address: selfB.Address})
		})
		c.update(func(m membership.Membership) (membership.Membership, error) { return m.Add(shard) })
		c.update(func(m membership.Membership) (membership.Membership, error) { return m.Assign(shard.ID, selfA.ID) })
		for _, h := range []*shardHost{a, b} {
			if err := h.host.RefreshMembership(c.ctx); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		// a holds the assignment and the cloud has not attached the shard.
		c.update(func(m membership.Membership) (membership.Membership, error) { return m.Release(shard.ID) })
		c.update(func(m membership.Membership) (membership.Membership, error) { return m.Let(shard.ID, selfA.ID) })
		c.update(func(m membership.Membership) (membership.Membership, error) { return m.Assign(shard.ID, selfB.ID) })
		if err := c.cloud.Attach(c.ctx, "shard-0", "machine-m"); err != nil {
			t.Fatal(err)
		}
		a.clock.Advance(time.Second)
		time.Sleep(time.Second)
		if err := b.host.RefreshMembership(c.ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		b.clock.Advance(time.Second)
		// The bubble's own time passes while b reads the object and the
		// device, which take the simulated store's and disk's latencies.
		time.Sleep(time.Second)
		heldA, _ := a.host.Member()
		heldB, _ := b.host.Member()
		if len(heldA.Disks) != 0 || len(heldB.Disks) != 1 {
			t.Fatalf("a holds %v and b holds %v, want the shard on b alone", heldA.Disks, heldB.Disks)
		}
		if c.runtime.Probes()[host.ProbeShardAssignmentMoved] == 0 {
			t.Fatal("a never found the assignment had moved")
		}
	})
}
