package simtest_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver is the
// combination the plan named as unreachable: three faults on at once, a
// migration underneath them, and a second host that ends up holding the VM.
//
// The campaign draws its own combinations from the seed, so it reaches this one
// on the seeds that draw it. This names it instead, because a combination a
// campaign can only reach by luck is one nothing can say it reached: here the
// source is partitioned from the destination's page server, the destination
// cannot reach object storage at all, and the migration happens anyway.
//
// What must hold is what always holds. The VM ends up on a host that can run
// it, at a checkpoint one of its own writers published, reading the bytes that
// checkpoint made durable and nothing else; and what the store holds at the end
// is still a deployment.
func TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 5,
			Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
				ConnectLatency: time.Microsecond},
			ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
				PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
				BytesPerSecond: 1 << 40}})
		prefix, err := platform.NewObjectPrefix("sproutfs/")
		if err != nil {
			t.Fatal(err)
		}
		// Two hosts and one VM: the smallest deployment in which a source can be
		// partitioned while a second host takes its VM over.
		topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
			VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: []volume.VolumeSpec{
				{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage}, {Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
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
		// The guest writes and publishes, so there is a checkpoint to come back
		// to, and then writes again, so what it holds past that checkpoint is
		// only in the pages the source is about to be cut off from.
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}

		// All three at once: the destination cannot reach the source's page
		// server, the source cannot reach the destination's, and the
		// destination cannot reach object storage at all.
		faults := []simtest.Fault{
			simtest.PartitionedPages(0, 1),
			simtest.HostLosesStore(1),
			simtest.DegradedLinks(),
		}
		for _, fault := range faults {
			if err := fault.Begin(ctx, world); err != nil {
				t.Fatalf("%s: %v", fault.Name(), err)
			}
		}
		if err := world.Migrate(ctx, "vm-1", 1); err != nil {
			t.Fatal(err)
		}
		for _, fault := range faults {
			if err := fault.End(ctx, world); err != nil {
				t.Fatalf("%s: %v", fault.Name(), err)
			}
		}
		// The world is given the chance to finish what the faults interrupted,
		// which is where a VM nobody is running is taken over.
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		for _, fault := range faults {
			if err := fault.Holds(ctx, world); err != nil {
				t.Errorf("%s did not leave the world as it found it: %v", fault.Name(), err)
			}
		}
		// A migration that succeeded under all three would say the faults
		// reached nothing: with the destination cut off from both the source's
		// pages and the store, the VM has to have been taken over instead.
		if world.Takeovers() == 0 {
			t.Fatal("the migration completed as if nothing were wrong with the world")
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}

		// The deeper half of the same combination: with the store reachable the
		// destination does take the VM over, and only then finds that it cannot
		// fetch the pages the source is still holding. What it owes the VM
		// then is the checkpoint the record selects, page for page.
		took := world.Takeovers()
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}
		partition := simtest.PartitionedPages(0, 1)
		if err := partition.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.Migrate(ctx, "vm-1", 0); err != nil {
			t.Fatal(err)
		}
		if err := partition.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if world.Takeovers() == took {
			t.Fatal("the post-copy completed across a partition")
		}
		// The VM is running again, and one more checkpoint of it lands now that
		// the world is whole: a takeover that left the VM unable to publish
		// would be a takeover that did not finish.
		if err := world.Store(ctx, "vm-1", 3, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := volume.CheckDeployment(t.Context(), runtime.ObjectStore(), prefix,
			volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex); err != nil {
			t.Error(err)
		}
	})
}
