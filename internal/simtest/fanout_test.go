package simtest_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestAFanOutWhoseReceiveFailsPartWayLeavesTheParentDurable: a fan-out is one
// pause of the parent and one hold on the point per child, taken before any
// child exists anywhere. A destination that takes the first child and refuses
// the next leaves the rest of those holds on a parent nothing can checkpoint,
// fence, migrate or stop, and nothing will ever fetch what they keep — the
// children they were taken for were never offered to anybody.
//
// The deployment gives them up rather than asking to release them. A release is
// a request the source can only refuse, because the pages it holds for a child
// that never started are the only copy of the parent's writes since its last
// checkpoint: a control plane that kept asking kept asking for ever, and the
// parent stayed sealed until the host's own deadline retired the holds minutes
// later. The assertion here is the parent's own durability, and the clock: it
// is checkpointable again long before that deadline.
func TestAFanOutWhoseReceiveFailsPartWayLeavesTheParentDurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 23,
			Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
				ConnectLatency: time.Microsecond},
			ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
				PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
				BytesPerSecond: 1 << 40}})
		prefix, err := platform.NewObjectPrefix("sproutfs/")
		if err != nil {
			t.Fatal(err)
		}
		topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
			VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: []volume.VolumeSpec{
				{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
				{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 32, 32, 128
		k.ReadAheadPages, k.WriteAheadPages = 1, 1
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})
		choose := func(int) int { return 0 }
		// A checkpoint to inherit, and then stores that live only in the
		// parent's pages: those are exactly the pages the point holds and
		// exactly what a release of a hold nobody fetched from cannot give up.
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 3, choose); err != nil {
			t.Fatal(err)
		}
		before := world.VM("vm-1").Status().Checkpoint.Sequence

		// The destination takes the first child of the fan-out and can take no
		// more, which is a host that filled up, was fenced or is being deleted
		// under the request.
		refusal := simtest.RefusedStartAfter(1, 1)
		if err := refusal.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		children := []simtest.VMSpec{
			{ID: "vm-1-a", Parent: "vm-1", Host: 1, Volumes: topology.VMs[0].Volumes},
			{ID: "vm-1-b", Parent: "vm-1", Host: 1, Volumes: topology.VMs[0].Volumes},
			{ID: "vm-1-c", Parent: "vm-1", Host: 1, Volumes: topology.VMs[0].Volumes},
		}
		if err := world.FanOut(ctx, "vm-1", children); err != nil {
			t.Fatal(err)
		}
		if err := refusal.End(ctx, world); err != nil {
			t.Fatal(err)
		}

		// One request forked one parent into a set of children and the set did
		// not happen, so none of them is left running anywhere.
		for _, child := range children {
			if host := world.HostOf(child.ID); host >= 0 {
				t.Fatalf("%s is running on host %d after the fan-out it belonged to failed",
					child.ID, host)
			}
		}
		// The parent's host holds nothing for any of them: the holds were given
		// up rather than left for the deadline, which is what makes the parent
		// durable again in the moment rather than in four checkpoint intervals.
		if serving := world.Host(0).Status().Serving; len(serving) != 0 {
			t.Fatalf("the parent's host still holds %v for children that will never be received",
				serving)
		}
		// This is what the whole of it is for: a parent that can be checkpointed
		// again, at the writes it has made since the fork point.
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatalf("checkpointing the parent of a fan-out that failed: %v", err)
		}
		if after := world.VM("vm-1").Status().Checkpoint.Sequence; after == before {
			t.Fatalf("the parent is still at checkpoint %d after the fan-out failed", after)
		}
		// And it happened because the deployment gave the holds up, not because
		// anything waited: the host's own deadline is four checkpoint intervals
		// away and nothing came near it.
		deadline := 4 * host.DefaultCheckpointInterval
		if waited := time.Since(began); waited >= deadline {
			t.Fatalf("the parent was durable again after %s, which is the hold deadline of %s "+
				"rather than the deployment giving the holds up", waited, deadline)
		}

		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
			t.Error(err)
		}
		if err := world.VerifyLossWindow(ctx, "vm-1"); err != nil {
			t.Error(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}
