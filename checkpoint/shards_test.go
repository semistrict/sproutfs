package checkpoint

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

const (
	shardBytes       = 64 << 20
	shardRegionBytes = 1 << 20
)

var (
	shardOne = rank.Identity{0x5a, 0x01}
	memberA  = rank.Identity{0xa0}
	memberB  = rank.Identity{0xb0}
)

// shardServedBy is the membership at generation in which shard is assigned
// to member at assigned, in state, under 1+1: one disk, which holds both
// stripes of every window.
func shardServedBy(generation uint64, member rank.Identity, assigned uint64,
	state membership.DiskState) membership.Membership {
	m, err := membership.New(generation, rank.Code{K: 1, M: 1},
		[]membership.Member{{ID: memberA, Address: "host-a", State: membership.Active},
			{ID: memberB, Address: "host-b", State: membership.Active}},
		[]membership.Disk{{ID: shardOne, Volume: "shard-one", Weight: 1, Member: member, State: state,
			Assigned: assigned}})
	if err != nil {
		panic(err)
	}
	return m
}

// shardCache is a cache that serves shards and keeps no disk of its own, as
// the host of member, following m.
func shardCache(t *testing.T, ctx context.Context, member rank.Identity, m membership.Membership) *Cache {
	t.Helper()
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(ctx, budget, CacheConfig{Shards: true, DiskRegionBytes: shardRegionBytes,
		ClusterPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	cache.FollowMembership(membership.NewFixed(m), member)
	return cache
}

// shardOf is the shard on device, leased to member at assigned.
func shardOf(device platform.File, member rank.Identity, assigned uint64) ShardConfig {
	return ShardConfig{Device: device, Identity: shardOne, Budget: fixedShare(shardBytes - shardRegionBytes),
		Lease: Lease{Assigned: assigned, Member: member}}
}

// A shard keeps its stripes when it moves: one member keeps a window's
// stripes on it, lets it go, and the cloud detaches it; another member it is
// assigned to under a newer generation opens it on its own machine, reads its
// regions back from their tables, and serves the same stripes. The member
// that let it go, holding an older assignment, is refused it.
func TestAShardMovesWithItsStripes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{AttachLatency: time.Second})
		if err := cloud.Create(ctx, "shard-one", shardBytes); err != nil {
			t.Fatal(err)
		}
		served := shardServedBy(2, memberA, 1, membership.Serving)
		a := shardCache(t, ctx, memberA, served)
		if err := cloud.Attach(ctx, "shard-one", "machine-a"); err != nil {
			t.Fatal(err)
		}
		deviceA, err := cloud.Devices("machine-a").Open(ctx, "shard-one")
		if err != nil {
			t.Fatal(err)
		}
		if err := a.AddShard(ctx, shardOf(deviceA, memberA, 1)); err != nil {
			t.Fatal(err)
		}
		if !a.Keeps(shardOne) || !slices.Equal(a.Disks(), []rank.Identity{shardOne}) {
			t.Fatalf("the cache keeps %v, want the shard", a.Disks())
		}
		code := served.Code()
		key := keyOf("vm", 0)
		if err := a.Keep(ctx, served, shardOne, keepOf(t, key, code, []int{0, 1})); err != nil {
			t.Fatal(err)
		}
		// Released, let go and detached, the shard is assigned to b.
		if err := a.RemoveShard(ctx, shardOne); err != nil {
			t.Fatal(err)
		}
		if a.Keeps(shardOne) {
			t.Fatal("a cache still keeps a shard it removed")
		}
		if err := a.Keep(ctx, served, shardOne, keepOf(t, key, code, []int{0})); !errors.Is(err, ErrNotKept) {
			t.Fatalf("a keep of a shard the cache removed: %v, want ErrNotKept", err)
		}
		if err := deviceA.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Detach(ctx, "shard-one", "machine-a"); err != nil {
			t.Fatal(err)
		}
		moved := shardServedBy(5, memberB, 4, membership.Serving)
		b := shardCache(t, ctx, memberB, moved)
		if err := cloud.Attach(ctx, "shard-one", "machine-b"); err != nil {
			t.Fatal(err)
		}
		deviceB, err := cloud.Devices("machine-b").Open(ctx, "shard-one")
		if err != nil {
			t.Fatal(err)
		}
		defer deviceB.Close()
		if err := b.AddShard(ctx, shardOf(deviceB, memberB, 4)); err != nil {
			t.Fatal(err)
		}
		stats := b.Stats()
		if len(stats.Shards) != 1 || stats.Shards[0].FromTables != 1 || stats.Shards[0].Identity != shardOne {
			t.Fatalf("the shard opened with %+v, want one region read back from its table", stats.Shards)
		}
		stripes, err := b.ReadStripes(ctx, moved, shardOne, peer.StripeRead{Window: key.rankWindow(),
			Code: code, MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(stripes.Items) != 2 {
			t.Fatalf("the moved shard serves %d stripes of the window, want the 2 kept before the move",
				len(stripes.Items))
		}
		// The member that let it go, should it open the device again under its
		// old assignment, is refused, and the shard is left as b leased it.
		stale, err := cloud.Disk("shard-one").Open(ctx, "device", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer stale.Close()
		again := shardCache(t, ctx, memberA, served)
		if err := again.AddShard(ctx, shardOf(stale, memberA, 1)); !errors.Is(err, ErrFenced) {
			t.Fatalf("a member opening a shard under an older assignment: %v, want ErrFenced", err)
		}
		if runtime.Probes()[ProbeShardLeaseRefused] == 0 {
			t.Fatal("the refusal reached no probe")
		}
		lease, err := readDiskLease(ctx, deviceB, shardRegionBytes)
		if err != nil || lease.lease != (Lease{Assigned: 4, Member: memberB}) {
			t.Fatalf("the shard's lease is %+v (%v), want b's", lease, err)
		}
	})
}

// A member that still holds a shard's device after another member took it,
// as two processes over one device would, writes it no more: the next region
// it opens finds the lease taken, fences it, and it writes no table. The
// member that took it reads back what it wrote, and nothing the fenced one
// wrote after.
func TestAStaleMemberThatStillHoldsTheDeviceIsFenced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{})
		if err := cloud.Create(ctx, "shard-one", shardBytes); err != nil {
			t.Fatal(err)
		}
		open := func() platform.File {
			handle, err := cloud.Disk("shard-one").Open(ctx, "device", platform.OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handle.Close() })
			return handle
		}
		old := shardServedBy(2, memberA, 1, membership.Serving)
		a := shardCache(t, ctx, memberA, old)
		if err := a.AddShard(ctx, shardOf(open(), memberA, 1)); err != nil {
			t.Fatal(err)
		}
		code := old.Code()
		if err := a.Keep(ctx, old, shardOne, keepOf(t, keyOf("vm", 0), code, []int{0, 1})); err != nil {
			t.Fatal(err)
		}
		taken := shardServedBy(5, memberB, 4, membership.Serving)
		b := shardCache(t, ctx, memberB, taken)
		if err := b.AddShard(ctx, shardOf(open(), memberB, 4)); err != nil {
			t.Fatal(err)
		}
		// a fills its region and opens the next: the lease is b's.
		for page := uint64(1); page < 1024 && !a.Fenced(shardOne); page++ {
			_ = a.Keep(ctx, old, shardOne, keepOf(t, keyOf("vm", page*512), code, []int{0, 1}))
		}
		if !a.Fenced(shardOne) {
			t.Fatal("a member whose lease was taken was never fenced")
		}
		if runtime.Probes()[ProbeShardFenced] == 0 {
			t.Fatal("the fence reached no probe")
		}
		if err := a.Keep(ctx, old, shardOne, keepOf(t, keyOf("vm", 2048*512), code, []int{0})); err == nil {
			t.Fatal("a fenced shard took a keep")
		}
		if err := a.RemoveShard(ctx, shardOne); err != nil {
			t.Fatal(err)
		}
		lease, err := readDiskLease(ctx, open(), shardRegionBytes)
		if err != nil || lease.lease != (Lease{Assigned: 4, Member: memberB}) {
			t.Fatalf("the shard's lease is %+v (%v), want b's", lease, err)
		}
	})
}

// countingFile counts the reads of a file.
type countingFile struct {
	platform.File
	reads atomic.Int64
}

func (f *countingFile) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	f.reads.Add(1)
	return f.File.ReadAt(ctx, destination, offset)
}

// A shard's lease says how many regions it has ever opened, and a member that
// opens it reads back only those: a device of thousands of slots that holds
// two regions opens in a handful of reads.
func TestAShardReadsBackOnlyTheRegionsItsLeaseNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		const regionBytes = 64 << 10
		const deviceBytes = 512 << 20
		cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{})
		if err := cloud.Create(ctx, "shard-one", deviceBytes); err != nil {
			t.Fatal(err)
		}
		handle, err := cloud.Disk("shard-one").Open(ctx, "device", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		settings := diskSettings{regionBytes: regionBytes, indexLimit: DefaultDiskIndexBytes, threshold: 1,
			clusterPercent: 100, identity: shardOne, lease: &Lease{Assigned: 1, Member: memberA}}
		disk, err := openCacheDisk(ctx, handle, fixedShare(deviceBytes-regionBytes), settings)
		if err != nil {
			t.Fatal(err)
		}
		disk.follow(membership.NewFixed(shardServedBy(2, memberA, 1, membership.Serving)), memberA)
		for page := uint64(0); page < 40; page++ {
			if err := disk.write(ctx, keyOf("vm", page*512), payloadOf(keyOf("vm", page*512), 3000),
				WriteFillPublication); err != nil {
				t.Fatal(err)
			}
		}
		disk.shutdown(ctx)
		regions := disk.stats().Regions
		if regions < 2 {
			t.Fatalf("the writes took %d regions, want at least 2", regions)
		}
		counting := &countingFile{File: handle}
		settings.lease = &Lease{Assigned: 3, Member: memberB}
		again, err := openCacheDisk(ctx, counting, fixedShare(deviceBytes-regionBytes), settings)
		if err != nil {
			t.Fatal(err)
		}
		if got := again.stats().Regions; got != regions {
			t.Fatalf("the shard read back %d regions, want %d", got, regions)
		}
		slots := int64(deviceBytes/regionBytes - 1)
		if reads := counting.reads.Load(); reads > 8*int64(regions)+8 || reads >= slots {
			t.Fatalf("opening a shard of %d slots that held %d regions took %d reads", slots, regions, reads)
		}
	})
}

// Removing a shard waits for every request using it: a read in flight holds
// it, and the shard is removed, its open region closed, only once that read
// has finished. A shard the cache keeps already, its own disk among them, is
// refused a second time.
func TestRemovingAShardWaitsForTheRequestsUsingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{})
		if err := cloud.Create(ctx, "shard-one", shardBytes); err != nil {
			t.Fatal(err)
		}
		device, err := cloud.Disk("shard-one").Open(ctx, "device", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer device.Close()
		cache := shardCache(t, ctx, memberA, shardServedBy(2, memberA, 1, membership.Serving))
		if err := cache.AddShard(ctx, shardOf(device, memberA, 1)); err != nil {
			t.Fatal(err)
		}
		if err := cache.AddShard(ctx, shardOf(device, memberA, 1)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("adding a shard the cache keeps already: %v, want ErrInvalidConfig", err)
		}
		_, release, kept := cache.cluster.hold(shardOne)
		if !kept {
			t.Fatal("the cache does not hold the shard it keeps")
		}
		removed := make(chan error, 1)
		go func() { removed <- cache.RemoveShard(ctx, shardOne) }()
		synctest.Wait()
		select {
		case err := <-removed:
			t.Fatalf("the shard was removed under a request using it: %v", err)
		default:
		}
		if cache.Keeps(shardOne) {
			t.Fatal("a shard being removed is still taken by new requests")
		}
		release()
		if err := <-removed; err != nil {
			t.Fatal(err)
		}
		if err := cache.RemoveShard(ctx, shardOne); !errors.Is(err, ErrNotKept) {
			t.Fatalf("removing a shard twice: %v, want ErrNotKept", err)
		}

		own := runtime.NewDisk("own", sim.DiskConfig{})
		file, err := own.Open(ctx, "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		budget, err := resource.New(4 << 10)
		if err != nil {
			t.Fatal(err)
		}
		both, err := NewCache(ctx, budget, CacheConfig{Disk: file, DiskBytes: shardBytes, Shards: true,
			DiskRegionBytes: shardRegionBytes, ClusterPercent: 100})
		if err != nil {
			t.Fatal(err)
		}
		defer both.Close()
		clash := shardOf(device, memberA, 1)
		clash.Identity = both.Identity()
		if err := both.AddShard(ctx, clash); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("adding a shard under the identity of the cache's own disk: %v, want ErrInvalidConfig", err)
		}
	})
}
