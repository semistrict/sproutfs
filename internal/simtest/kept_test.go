package simtest_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// keptTopology is one VM with a memory and a disk, and two VMs created from
// what it keeps, one on each host.
func keptTopology() simtest.Topology {
	volumes := []volume.VolumeSpec{
		{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: simtest.DiskVolume, Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage},
	}
	return simtest.Topology{Hosts: []string{"host-0", "host-1"}, VMs: []simtest.VMSpec{
		{ID: "vm-0", Host: 0, Volumes: volumes},
		{ID: "vm-1", Host: 1, Parent: "vm-0", Kept: true, Volumes: volumes},
		{ID: "vm-2", Host: 0, Parent: "vm-0", Kept: true, Volumes: volumes},
	}}
}

// TestACreateFromAKeptCheckpointReadsThatCheckpoint: a VM keeps three of its
// checkpoints — two with its memory and VMM state, one of its disks alone —
// and goes on writing and checkpointing past all of them, and then stops. Every
// kept checkpoint still reads as the pause it was kept at. A VM created from
// the first resumes its guest at that pause, reading exactly its bytes; one
// created from the disks alone boots cold over them. The kept checkpoint
// nothing was created from is released, the two that were are not, and what
// the store holds at the end is a deployment with nothing left over.
func TestACreateFromAKeptCheckpointReadsThatCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		topology := keptTopology()
		prefix := newPrefix(t, "kept/")
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})

		for value, disks := range []bool{false, true, false} {
			if err := world.StoreAll("vm-0", byte(10+value)); err != nil {
				t.Fatal(err)
			}
			if err := world.Keep(ctx, "vm-0", disks); err != nil {
				t.Fatal(err)
			}
		}
		for _, value := range []byte{20, 21} {
			if err := world.StoreAll("vm-0", value); err != nil {
				t.Fatal(err)
			}
			if err := world.Checkpoint(ctx, "vm-0"); err != nil {
				t.Fatal(err)
			}
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyKept(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a kept checkpoint lost what it read to the checkpoints after it: %v", err)
		}
		kept, _ := world.KeptOf(ctx, "vm-0")
		if len(kept) != 3 {
			t.Fatalf("vm-0 keeps %v, want the three checkpoints it asked to keep", kept)
		}

		if err := world.CreateFromKept(ctx, topology.VMs[1], kept[0], false); err != nil {
			t.Fatalf("resuming a VM from a kept checkpoint of a stopped VM: %v", err)
		}
		if err := world.CreateFromKept(ctx, topology.VMs[2], kept[1], false); err != nil {
			t.Fatalf("booting a VM from a kept checkpoint of the disks: %v", err)
		}
		for _, id := range []string{"vm-1", "vm-2"} {
			if world.HostOf(id) < 0 {
				t.Fatalf("%s was not created", id)
			}
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}

		for _, sequence := range kept {
			if err := world.Release(ctx, "vm-0", sequence); err != nil {
				t.Fatal(err)
			}
		}
		left, forked := world.KeptOf(ctx, "vm-0")
		if len(left) != 2 || !forked[kept[0]] || !forked[kept[1]] {
			t.Fatalf("vm-0 keeps %v (forked %v) after the releases, want the two VMs were created from",
				left, forked)
		}
		if err := world.VerifyKept(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := volume.CheckDeployment(ctx, runtime.ObjectStore(), prefix); err != nil {
			t.Fatal(err)
		}
	})
}
