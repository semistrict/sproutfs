package simtest_test

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// ephemeralPages is the size of the ephemeral disk the scenarios here give
// their VM, in PMEM pages.
const ephemeralPages = 2

// ephemeralWorld is two hosts and one VM with memory, a disk every checkpoint
// holds and an ephemeral disk, which is the least that shows the two disks
// going different ways.
func ephemeralWorld(t *testing.T, runtime *sim.Runtime, prefix string) *simtest.World {
	t.Helper()
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0, Volumes: []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: 2 * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: simtest.DiskVolume, Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage},
			{Name: simtest.EphemeralVolume, Size: ephemeralPages * simtest.PMEMPage,
				PageSize: simtest.PMEMPage, Ephemeral: true},
		}}}}
	return simtest.MustStart(t, sim.WithRuntime(t.Context(), runtime), simtest.Config{
		Runtime: runtime, Topology: topology, Knobs: campaignKnobs(t, runtime, topology),
		Prefix: newPrefix(t, prefix), Log: t.Logf})
}

// requireEphemeral requires one VM's ephemeral disk to read, through its own
// guest, as value on every page, at the size it was created with.
func requireEphemeral(t *testing.T, ctx context.Context, world *simtest.World, id string, value byte, what string) {
	t.Helper()
	vm := world.VM(id)
	if vm == nil {
		t.Fatalf("%s: %s is running nowhere", what, id)
	}
	disk := vm.Volume(simtest.EphemeralVolume)
	if disk == nil || !disk.Ephemeral() || disk.Size() != ephemeralPages*simtest.PMEMPage {
		t.Fatalf("%s: %s's ephemeral disk is %+v, want an ephemeral disk of %d pages", what, id, disk, ephemeralPages)
	}
	want := bytes.Repeat([]byte{value}, simtest.PMEMPage)
	for page := range uint64(ephemeralPages) {
		got, err := world.ReadPage(ctx, id, simtest.EphemeralVolume, page)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: page %d of %s's ephemeral disk reads %d, want %d", what, page, id, got[0], value)
		}
	}
}

// storeEphemeral has one VM's guest write value into every page of its
// ephemeral disk.
func storeEphemeral(t *testing.T, world *simtest.World, id string, value byte) {
	t.Helper()
	if err := world.StorePages(id, simtest.EphemeralVolume, []uint64{0, 1}, value); err != nil {
		t.Fatal(err)
	}
}

// checkNothingEphemeralPublished runs the deployment check, which reads every
// part every selected root reaches and refuses a member of an ephemeral disk,
// and every root, which may not address a segment of one.
func checkNothingEphemeralPublished(t *testing.T, ctx context.Context, runtime *sim.Runtime, prefix string) {
	t.Helper()
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), newPrefix(t, prefix),
		volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
		volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
		t.Fatal(err)
	}
}

// TestAnEphemeralDiskIsNeverPublishedAndIsLostWithItsHost: a guest's stores into
// its ephemeral disk are held by its host and by nothing else. Neither a
// capture, nor a checkpoint of the disks, nor a stop publishes them, so a host
// loss takes them, and the VM comes back on another host with the disk zeroed
// at its size, while its other disk comes back at the checkpoint.
func TestAnEphemeralDiskIsNeverPublishedAndIsLostWithItsHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := ephemeralWorld(t, runtime, "ephemeral-lost/")

		if err := world.StoreAll("vm-0", 5); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.CheckpointDisks(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		// The guest holds what it wrote, though nothing published it.
		requireEphemeral(t, ctx, world, "vm-0", 5, "the running VM")
		checkNothingEphemeralPublished(t, ctx, runtime, "ephemeral-lost/")
		index, err := world.Host(0).Checkpoints().Open(ctx, world.VM("vm-0").Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !index.Ephemeral(simtest.EphemeralVolume) ||
			index.Size(simtest.EphemeralVolume) != ephemeralPages*simtest.PMEMPage {
			t.Fatalf("the selected root records the ephemeral disk as ephemeral %v at %d bytes",
				index.Ephemeral(simtest.EphemeralVolume), index.Size(simtest.EphemeralVolume))
		}

		if err := world.LoseHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the VM of the lost host is on host-%d, want the one that took it over", at)
		}
		requireEphemeral(t, ctx, world, "vm-0", 0, "the VM taken over from a lost host")
		// The disk every checkpoint holds came back at generation 5.
		if err := world.VerifyDurable(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}

		// A stop publishes the disks, and the ephemeral one is still not among
		// them: a start gets it back zeroed.
		storeEphemeral(t, world, "vm-0", 6)
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		requireEphemeral(t, ctx, world, "vm-0", 0, "the VM started after a stop")
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkNothingEphemeralPublished(t, ctx, runtime, "ephemeral-lost/")
	})
}

// TestAnEphemeralDiskReachesNoFork: a fork point holds the parent's pages and
// not its ephemeral disk's, so a child on the parent's own host and a child on
// another both start with the disk zeroed, and the parent goes on with what it
// wrote there.
func TestAnEphemeralDiskReachesNoFork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(2, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := ephemeralWorld(t, runtime, "ephemeral-fork/")

		if err := world.StoreAll("vm-0", 7); err != nil {
			t.Fatal(err)
		}
		for _, child := range []simtest.VMSpec{
			{ID: "vm-local", Parent: "vm-0", Host: 0},
			{ID: "vm-remote", Parent: "vm-0", Host: 1},
		} {
			if err := world.Fork(ctx, child); err != nil {
				t.Fatal(err)
			}
			if at := world.HostOf(child.ID); at != child.Host {
				t.Fatalf("%s is on host-%d, want host-%d", child.ID, at, child.Host)
			}
			requireEphemeral(t, ctx, world, child.ID, 0, "the child "+child.ID)
		}
		requireEphemeral(t, ctx, world, "vm-0", 7, "the parent after its forks")
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkNothingEphemeralPublished(t, ctx, runtime, "ephemeral-fork/")
	})
}

// TestAMigrationCarriesAnEphemeralDisk: a migrated VM keeps running, and its
// filesystem on the ephemeral disk with it, so the destination fetches every
// page of the disk the source holds, as it fetches every unpublished page, and
// the guest reads there what it wrote here.
func TestAMigrationCarriesAnEphemeralDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(3, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := ephemeralWorld(t, runtime, "ephemeral-migrate/")

		if err := world.StoreAll("vm-0", 9); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Migrate(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the migrated VM is on host-%d, want host-1", at)
		}
		// The source is gone: what the destination reads is what it fetched.
		if err := world.LoseHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		requireEphemeral(t, ctx, world, "vm-0", 9, "the migrated VM")
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkNothingEphemeralPublished(t, ctx, runtime, "ephemeral-migrate/")
	})
}
