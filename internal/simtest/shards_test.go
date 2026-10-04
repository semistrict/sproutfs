package simtest_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
)

// shardWorld is six hosts, a VM on the first, and six shards under 4+2.
func shardWorld(t *testing.T, ctx context.Context, runtime *sim.Runtime) *simtest.World {
	t.Helper()
	topology := clusterTopology(6)
	return simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: campaignKnobs(t, runtime, topology), Prefix: newPrefix(t, "shards/"), Log: t.Logf, Shards: 6})
}

// rankedShards is the disks the membership ranks windows over, by identity
// and weight, wherever they are served.
func rankedShards(t *testing.T, ctx context.Context, world *simtest.World) []string {
	t.Helper()
	store, err := membership.NewStore(membership.Config{ObjectStore: world.Runtime().ObjectStore(),
		ObjectPrefix: world.Prefix()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ranked []string
	for _, cache := range m.List().Caches() {
		ranked = append(ranked, fmt.Sprintf("%s/%d", cache.Identity, cache.Weight))
	}
	if len(ranked) != len(world.ShardVolumes()) {
		t.Fatalf("the membership ranks windows over %v, want the %d shards", ranked, len(world.ShardVolumes()))
	}
	return ranked
}

// openReadsFromTheShards opens vm-0 on host index, has its guest read back
// every page it wrote, and fails unless the one part it read of the store is
// the VMM state the restore loads: every page came from the shards. Then it
// suspends the VM there again, which fills the shards with what it published.
func openReadsFromTheShards(t *testing.T, ctx context.Context, world *simtest.World, index int, when string) {
	t.Helper()
	before := world.PartReads(index)
	if err := world.Start(ctx, "vm-0", index); err != nil {
		t.Fatalf("%s: %v", when, err)
	}
	if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		t.Fatalf("%s: the VM opened on host-%d reads back something its guest did not write: %v", when, index, err)
	}
	if reads := world.PartReads(index) - before; reads != 1 {
		t.Fatalf("%s: host-%d read %d parts from the store to open and fault in the VM, want the VMM state alone",
			when, index, reads)
	}
	if err := world.StoreAll("vm-0", 11); err != nil {
		t.Fatal(err)
	}
	if err := world.Suspend(ctx, "vm-0"); err != nil {
		t.Fatal(err)
	}
	if err := world.Settle(ctx); err != nil {
		t.Fatal(err)
	}
}

// balanced fails unless every shard serves, and the hosts in up each serve
// within one shard of each other and the rest none.
func balanced(t *testing.T, ctx context.Context, world *simtest.World, up []int, when string) {
	t.Helper()
	if !world.ShardServing(ctx) {
		t.Fatalf("%s: not every shard serves on a host that holds it", when)
	}
	var served []int
	for index := range world.Hosts() {
		count := world.ShardsServed(ctx, index)
		if !slices.Contains(up, index) {
			if count != 0 {
				t.Fatalf("%s: host-%d, which is not up, serves %d shards", when, index, count)
			}
			continue
		}
		served = append(served, count)
	}
	if slices.Max(served)-slices.Min(served) > 1 {
		t.Fatalf("%s: the hosts up serve %v shards", when, served)
	}
}

// The cache's windows are ranked over shards, never hosts, so compute
// scaling moves none of them: six hosts scale down to three, as an
// autoscaler removes machines, then back up to six. After each host leaves or
// joins, its shards having moved and spread again, a VM opened on a host that
// was there reads every page from the shards and none from the store. The
// disks the membership ranks windows over are the same six throughout.
func TestHostsScaleUpAndDownWithNoStoreReadForACachedWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := shardWorld(t, ctx, runtime)
		ranked := rankedShards(t, ctx, world)
		balanced(t, ctx, world, []int{0, 1, 2, 3, 4, 5}, "at the start")
		if err := world.StoreAll("vm-0", 7); err != nil {
			t.Fatal(err)
		}
		if err := world.Suspend(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		up := []int{0, 1, 2, 3, 4, 5}
		reader := 0
		for _, leaving := range []int{5, 4, 3} {
			world.Unlist(ctx, leaving)
			if err := world.Shutdown(ctx, leaving); err != nil {
				t.Fatal(err)
			}
			if err := world.Settle(ctx); err != nil {
				t.Fatal(err)
			}
			up = slices.DeleteFunc(up, func(index int) bool { return index == leaving })
			when := fmt.Sprintf("after host-%d left", leaving)
			balanced(t, ctx, world, up, when)
			reader = (reader + 1) % len(up)
			openReadsFromTheShards(t, ctx, world, up[reader], when)
		}
		for _, joining := range []int{3, 4, 5} {
			if err := world.Restart(ctx, joining); err != nil {
				t.Fatal(err)
			}
			if err := world.Settle(ctx); err != nil {
				t.Fatal(err)
			}
			up = append(up, joining)
			when := fmt.Sprintf("after host-%d joined", joining)
			balanced(t, ctx, world, up, when)
			openReadsFromTheShards(t, ctx, world, joining, when)
		}
		if got := rankedShards(t, ctx, world); !slices.Equal(got, ranked) {
			t.Fatalf("the shards ranked changed from %v to %v", ranked, got)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A shard moving is briefly served by nobody, and 4+2 reads around it: with
// one host's shard released and closed and not yet served elsewhere, and with
// a host lost with its shard attached to its machine, a VM opened on another
// host reads every page from the other shards and none from the store.
func TestReadsDuringAShardsMoveHedgeAroundIt(t *testing.T) {
	for _, event := range []string{"leaving", "lost"} {
		t.Run(event, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := newCampaignRuntime(1, false)
				ctx := sim.WithRuntime(t.Context(), runtime)
				world := shardWorld(t, ctx, runtime)
				if err := world.StoreAll("vm-0", 7); err != nil {
					t.Fatal(err)
				}
				if err := world.Suspend(ctx, "vm-0"); err != nil {
					t.Fatal(err)
				}
				if err := world.Settle(ctx); err != nil {
					t.Fatal(err)
				}
				switch event {
				case "leaving":
					// Only the step that drains it, and its host closing the
					// shard: the shard is releasing and served by nobody.
					world.Leaving(2)
					world.ShardPass(ctx)
				case "lost":
					if err := world.Kill(ctx, 2, sim.CrashProcess); err != nil {
						t.Fatal(err)
					}
				}
				if world.ShardServing(ctx) || world.ShardsServed(ctx, 2) != 0 {
					t.Fatalf("with host-2 %s, every shard still serves", event)
				}
				before := world.PartReads(1)
				if err := world.Start(ctx, "vm-0", 1); err != nil {
					t.Fatal(err)
				}
				if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
					t.Fatalf("the VM read back something its guest did not write: %v", err)
				}
				if reads := world.PartReads(1) - before; reads != 1 {
					t.Fatalf("with a shard moving, host-1 read %d parts from the store, want the VMM state alone", reads)
				}
				if err := world.Settle(ctx); err != nil {
					t.Fatal(err)
				}
				if !world.ShardServing(ctx) {
					t.Fatal("the shard never served again")
				}
				if err := world.Close(ctx); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
