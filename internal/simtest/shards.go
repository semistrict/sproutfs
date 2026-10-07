package simtest

import (
	"context"
	"fmt"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// A world with shards keeps the cluster's cache on network disks of a
// simulated cloud, and a controller carries the membership out on them, as
// the orchestrator does in a deployment: each pass takes one step of the
// membership towards the hosts that are up and the shards, and attaches and
// detaches the disks it calls for. The world takes passes, each followed by
// every host that is up reading the membership and opening and closing what
// it calls for, until a pass changes nothing.

// shardBytes is each shard's network disk: room for every window of a
// topology's VMs many times over, as the hosts' own disks have.
const shardBytes = clusterCacheBytes

// maximumShardPasses bounds how many passes one settle takes. A move is a
// handful of passes, and a world that has not settled in this many has a
// shard that cannot be served, which the next settle tries again.
const maximumShardPasses = 64

// makeShards makes the world's cloud and its shards, and the controller over
// them.
func (w *World) makeShards(ctx context.Context) error {
	w.cloud = w.runtime.NewNetworkDisks(sim.NetworkDisksConfig{AttachLatency: 2 * time.Second,
		DetachLatency: time.Second, DescribeLatency: 50 * time.Millisecond,
		Device: sim.DiskConfig{PowerLossFaults: true}})
	w.control = &membership.ShardControl{Store: w.membership, Disks: w.cloud}
	w.leaving = map[int]bool{}
	for n := range w.config.Shards {
		volume := fmt.Sprintf("%sshard-%d", w.config.Namespace, n)
		if err := w.cloud.Provision(ctx, volume, shardBytes); err != nil {
			return err
		}
		w.control.Volumes = append(w.control.Volumes, volume)
	}
	return nil
}

// shardWant is every host that is up, as it reports itself now, and leaving
// where the world took it out of the membership.
func (w *World) shardWant() membership.Want {
	want := membership.Want{Code: w.code}
	for index := range w.hosts {
		running := w.up(index)
		if running == nil {
			continue
		}
		self, member := running.Member()
		if !member {
			continue
		}
		w.mu.Lock()
		self.Leaving = w.leaving[index]
		w.mu.Unlock()
		want.Hosts = append(want.Hosts, self)
	}
	return want
}

// ShardPass is one pass of the controller, then every host that is up
// reading the membership and opening and closing the shards it calls for. It
// reports whether the pass changed the membership or asked anything of the
// cloud.
func (w *World) ShardPass(ctx context.Context) bool {
	_, changed, err := w.control.Pass(ctx, w.shardWant())
	if err != nil {
		w.logf("controller: a pass over the shards: %v", err)
		// A call of the cloud that failed is made again by a later pass.
		changed = true
	}
	for index := range w.hosts {
		running := w.up(index)
		if running == nil {
			continue
		}
		if err := running.RefreshMembership(ctx); err != nil {
			w.logf("%s: reading the membership: %v", w.hosts[index].name, err)
		}
		running.SettleShards(ctx)
	}
	return changed
}

// Leaving marks the host at index leaving, as a pod being deleted is, and
// takes no pass: a test that steps the controller itself asks for each.
func (w *World) Leaving(index int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.leaving[index] = true
}

// settleShards takes passes until one changes nothing, or the bound.
func (w *World) settleShards(ctx context.Context) {
	for range maximumShardPasses {
		if !w.ShardPass(ctx) || ctx.Err() != nil {
			return
		}
	}
	w.logf("controller: the shards did not settle in %d passes", maximumShardPasses)
}

// Prefix is the deployment's prefix in the object store, where its
// membership is.
func (w *World) Prefix() platform.ObjectPrefix { return w.config.Prefix }

// Cloud is the network disks a world with shards keeps them on.
func (w *World) Cloud() *sim.NetworkDisks { return w.cloud }

// ShardVolumes is the volumes of a world's shards, in order.
func (w *World) ShardVolumes() []string { return w.control.Volumes }

// ShardsServed is how many shards the membership has serving on the host at
// index, held open by it: zero for a host that is not up.
func (w *World) ShardsServed(ctx context.Context, index int) int {
	running := w.up(index)
	if running == nil {
		return 0
	}
	m, err := w.membership.Read(ctx)
	if err != nil {
		return 0
	}
	self, _ := running.Member()
	served := 0
	for _, disk := range self.Disks {
		if m.Serves(self.ID, disk.ID) {
			served++
		}
	}
	return served
}

// ShardServing reports whether every shard serves on a host that holds it.
func (w *World) ShardServing(ctx context.Context) bool {
	m, err := w.membership.Read(ctx)
	if err != nil {
		return false
	}
	held := map[rank.Identity]bool{}
	for index := range w.hosts {
		if running := w.up(index); running != nil {
			self, _ := running.Member()
			for _, disk := range self.Disks {
				held[disk.ID] = true
			}
		}
	}
	for _, volume := range w.control.Volumes {
		id := membership.ShardIdentity(volume)
		disk, listed := m.Disk(id)
		if !listed || disk.State != membership.Serving || !held[id] {
			return false
		}
	}
	return true
}
