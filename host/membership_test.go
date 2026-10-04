package host_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// cachingHost is one host with a page cache disk, in a synctest bubble, on a
// simulated clock: what the membership needs of a host and nothing more.
type cachingHost struct {
	ctx     context.Context
	runtime *sim.Runtime
	clock   *sim.Clock
	disk    *sim.Disk
	config  host.Config
}

func newCachingHost(t *testing.T, space sim.SpaceConfig) *cachingHost {
	t.Helper()
	runtime := sim.New(sim.Config{})
	h := &cachingHost{runtime: runtime, clock: runtime.NewClock("host"),
		disk: runtime.NewDisk("node", sim.DiskConfig{Space: space})}
	h.ctx = sim.WithRuntime(t.Context(), runtime)
	h.config = host.Config{Network: runtime.Network(), Resources: testresource.New(),
		ObjectStore: runtime.ObjectStore(), Clock: h.clock, Entropy: runtime.NewEntropy("host"),
		Cache:              checkpoint.CacheConfig{Disk: h.open(t, "cache-0"), DiskRegionBytes: 8 << 20},
		CacheVolume:        "cache-0",
		Migration:          host.MigrationConfig{Address: "host-0-pages"},
		CheckpointInterval: -1, EpochInterval: -1}
	return h
}

// open opens a cache file of the node's, closed with the test.
func (h *cachingHost) open(t *testing.T, name string) platform.File {
	t.Helper()
	file, err := h.disk.Open(h.ctx, name, platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func (h *cachingHost) start(t *testing.T) *host.Host {
	t.Helper()
	started, err := host.StartHost(h.ctx, h.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := started.Close(context.WithoutCancel(h.ctx)); err != nil {
			t.Error(err)
		}
	})
	return started
}

// A host with a cache disk reports itself to the membership: its identity,
// which is its disk's, its peer-server address, and its disk with the weight
// of the disk it is given. Until it reads the membership it holds its own
// disk alone, under the code of one host, so it ranks first for every window
// and each window's one stripe is its envelope whole.
func TestAHostReportsItselfAndHoldsItsDiskAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		h.config.Cache.DiskBytes = 40 << 30
		h.config.MembershipInterval = -1
		started := h.start(t)
		status := started.Status()
		self, member := started.Member()
		identity := status.Cache.Disk.Identity
		if !member || identity.IsZero() || self.ID != identity || self.Address != "host-0-pages" ||
			len(self.Disks) != 1 || self.Disks[0] != (membership.Disk{ID: identity, Volume: "cache-0", Weight: 3}) ||
			status.Member.ID != self.ID {
			t.Fatalf("the host reports itself as %+v (%v), want its disk %s of weight 3", self, member, identity)
		}
		held := started.Membership()
		if held.Generation() != 0 || held.Code() != (rank.Code{K: 1, M: 0}) || !held.Serves(self.ID, identity) {
			t.Fatalf("a host that read no membership holds generation %d under %s", held.Generation(), held.Code())
		}
		ref := control.Ref{VM: "vm-alone", Sequence: 1}
		for page := range uint64(2048) {
			window := rank.PageWindow(control.Identity{Ref: ref, Volume: "ram0", Page: page}, 4096)
			if holders := held.List().Holders(window); len(holders) != 1 || holders[0].Identity != identity {
				t.Fatalf("page %d's window goes on %v, want this host's disk whole", page, holders)
			}
		}
	})
}

// A host whose cache is given no disk is no member: it has no weight, and so
// no disk to report.
func TestAHostWithNoCacheDiskIsNoMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		started := h.start(t)
		if self, member := started.Member(); member || !self.ID.IsZero() {
			t.Fatalf("a host with no disk reports itself as %+v", self)
		}
		if got := started.Membership(); got.List().Len() != 0 {
			t.Fatalf("a host with no disk holds %v", got.List().Caches())
		}
	})
}

// A host's identity is written in its disk. A pod replaced on the same node
// opens the same cache file and keeps the identity, and with it its place in
// the membership; a pod on another node opens that node's file and takes its
// identity.
func TestAHostsIdentityIsItsDisks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		h.config.Cache.DiskBytes = 16 << 30
		first, err := host.StartHost(h.ctx, h.config)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := first.Member()
		if err := first.Close(h.ctx); err != nil {
			t.Fatal(err)
		}
		replaced := h.start(t)
		if after, _ := replaced.Member(); after.ID != before.ID {
			t.Fatalf("a host replaced over its file is %s, want %s", after.ID, before.ID)
		}
		elsewhere := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		elsewhere.config.Cache.DiskBytes = 16 << 30
		elsewhere.config.Entropy = h.runtime.NewEntropy("another node")
		other, _ := elsewhere.start(t).Member()
		if other.ID == before.ID || other.ID.IsZero() {
			t.Fatalf("a host on another node is %s, the first node's host %s", other.ID, before.ID)
		}
	})
}

// A host reads the membership as it starts and then on its own timer, and
// keeps the newest generation it read while the store does not answer.
func TestAHostHoldsTheMembershipItLastRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 64 << 30})
		h.config.Cache.DiskBytes = 16 << 30
		writer, err := membership.NewStore(membership.Config{ObjectStore: h.runtime.ObjectStore(),
			Entropy: h.runtime.NewEntropy("controller")})
		if err != nil {
			t.Fatal(err)
		}
		other := membership.Host{ID: rank.Identity{9}, Address: "host-1-pages",
			Disks: []membership.Disk{{ID: rank.Identity{9}, Volume: "cache-0", Weight: 1}}}
		settle(t, h.ctx, writer, membership.Want{Code: rank.Code{K: 1, M: 1}, Hosts: []membership.Host{other}})
		started := h.start(t)
		// A read of the store takes its latency on the bubble's clock.
		time.Sleep(time.Second)
		synctest.Wait()
		self, _ := started.Member()
		if got := started.Membership(); got.Generation() != 3 || got.Serves(self.ID, self.ID) {
			t.Fatalf("a started host holds generation %d; want 3, which has not heard of it", got.Generation())
		}
		settle(t, h.ctx, writer, membership.Want{Code: rank.Code{K: 1, M: 1}, Hosts: []membership.Host{other, self}})
		h.clock.Advance(membership.DefaultInterval)
		time.Sleep(time.Second)
		synctest.Wait()
		both := started.Membership()
		if both.Generation() != 5 || !both.Serves(self.ID, self.ID) {
			t.Fatalf("after an interval the host holds generation %d, want 5 in which it serves its disk",
				both.Generation())
		}
		h.runtime.ObjectStore().Fail()
		h.clock.Advance(membership.DefaultInterval)
		time.Sleep(time.Second)
		synctest.Wait()
		status := started.Status().Membership
		if !status.Membership.Equal(both) || status.Reads != 2 || status.Failures != 1 || status.Error == "" {
			t.Fatalf("with the store down the host holds generation %d after %d reads and %d failures (%q)",
				status.Membership.Generation(), status.Reads, status.Failures, status.Error)
		}
		h.runtime.ObjectStore().Recover()
	})
}

// settle has a controller move the membership in store to want.
func settle(t *testing.T, ctx context.Context, store *membership.Store, want membership.Want) {
	t.Helper()
	for {
		_, changed, err := store.Reconcile(ctx, want)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			return
		}
	}
}

// A disk's weight is the disk it is given, as the limiter's goals leave it on
// the filesystem, and never the share the limiter moves as other writers fill
// the disk: every change of a weight moves windows. Here other writers hold
// most of a 200 GiB disk, so the share is a few GiB, while the goal of a tenth
// kept free gives the cache 180 GiB: a weight of eleven.
func TestACachesWeightIsItsDiskNotItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCachingHost(t, sim.SpaceConfig{TotalBytes: 200 << 30, OutsideBytes: 170 << 30})
		limiter, err := resource.NewDiskLimiter(h.ctx, resource.DiskLimiterConfig{Space: h.disk,
			Goal: resource.DiskGoal{FreePercent: 10}, Clock: h.clock})
		if err != nil {
			t.Fatal(err)
		}
		defer limiter.Close()
		h.config.DiskLimiter = limiter
		started := h.start(t)
		// The room is the 30 GiB free, less the floor of 20, less a band of a
		// fifth of the 10 left.
		if share := limiter.CacheShare(); share != 8<<30 {
			t.Fatalf("the share is %d bytes, want 8 GiB", share)
		}
		if self, _ := started.Member(); len(self.Disks) != 1 || self.Disks[0].Weight != 11 {
			t.Fatalf("the disk weighs %+v, want 11 for 180 GiB", self.Disks)
		}
	})
}
