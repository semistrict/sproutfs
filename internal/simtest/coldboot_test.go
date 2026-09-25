package simtest_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// coldTopology is two hosts and one VM with memory and a disk, which is the
// least a cold start needs: something to discard and something to keep.
func coldTopology() simtest.Topology {
	return simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0, Volumes: []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: 2 * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage},
		}}}}
}

func coldWorld(t *testing.T, runtime *sim.Runtime, prefix string) *simtest.World {
	t.Helper()
	topology := coldTopology()
	world := simtest.MustStart(t, sim.WithRuntime(t.Context(), runtime), simtest.Config{
		Runtime: runtime, Topology: topology, Knobs: campaignKnobs(t, runtime, topology),
		Prefix: newPrefix(t, prefix), Log: t.Logf})
	return world
}

// TestAColdStartedVMComesBackWithoutItsMemory: a cold start discards every page
// of the guest's memory and the VMM state with it, so what comes back reads as
// zeroes where its memory was and as the last checkpoint's bytes everywhere
// else. The guest that comes back has written nothing: it is a machine that
// booted, not one that was restored.
func TestAColdStartedVMComesBackWithoutItsMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := coldWorld(t, runtime, "cold/")

		if err := world.StoreAll("vm-0", 7); err != nil {
			t.Fatal(err)
		}
		if err := world.Suspend(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.StartCold(ctx, "vm-0", 1); err != nil {
			t.Fatalf("cold starting a stopped VM: %v", err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the cold-started VM is on host-%d, want the host it was started on", at)
		}
		// Verify reads every page through the guest's own fault path and
		// requires it to be what the world says that VM holds, which for a cold
		// start is zeroes in memory and the stop's bytes on the disk.
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a cold-started VM reads back something it does not hold: %v", err)
		}
		// The guest writes anew over what it came back with, and every one of
		// those writes has to survive a checkpoint and a warm start.
		if err := world.StoreAll("vm-0", 3); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Suspend(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-0", 0); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAHostComingBackLeavesAStoppedVMStopped: a host that is lost and started
// again gives back everything it was running, and a VM the deployment stopped is
// not that — its handle was released and its pages given back before the host
// died, so there is nothing for the host to give back and nothing to repair.
// Only a start brings it back, which is the whole difference between a stop and
// every other way a VM stops running here.
func TestAHostComingBackLeavesAStoppedVMStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(3, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := coldWorld(t, runtime, "restart/")

		if err := world.StoreAll("vm-0", 6); err != nil {
			t.Fatal(err)
		}
		if err := world.Suspend(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.LoseHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if err := world.RestartHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if running := world.Host(0).Machines(); len(running) != 0 {
			t.Fatalf("a host that came back started %v, and the deployment had stopped it", running)
		}
		if stopped := world.Stopped(); len(stopped) != 1 || stopped[0] != "vm-0" {
			t.Fatalf("the stopped VMs are %v, want the one that was stopped", stopped)
		}
		// And it is still a VM a cold start applies to, which is what says it
		// was left stopped rather than quietly started somewhere.
		if err := world.StartCold(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the cold-started VM is on host-%d, want the host it was started on", at)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestALostHostColdBootsFromTheLastCheckpointOfTheDisks: the interval checkpoints
// a VM's disks and not its memory, so after a host loss the checkpoint its
// record selects has no VMM state to restore. The VM comes back cold on the host
// that takes it over: its memory zeroes, even though an earlier checkpoint
// published memory and registers, and its disk exactly what the checkpoint of
// the disks sealed — never those registers over these disks.
func TestALostHostColdBootsFromTheLastCheckpointOfTheDisks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(4, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := coldWorld(t, runtime, "lost-disks/")

		if err := world.StoreAll("vm-0", 5); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.StoreAll("vm-0", 6); err != nil {
			t.Fatal(err)
		}
		if err := world.CheckpointDisks(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		// Past the checkpoint of the disks, and lost with the host.
		if err := world.StoreAll("vm-0", 8); err != nil {
			t.Fatal(err)
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
		if got := world.Takeovers(); got != 1 {
			t.Fatalf("%d takeovers, want the one of the lost host's VM", got)
		}
		if err := world.VerifyDurable(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		// Zeroes where the memory was, and generation 6 on the disk.
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a VM cold booted from its disks reads back something it does not hold: %v", err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAColdStartOfARunningVMDoesNothing: every operation of a schedule is drawn
// rather than chosen, so one that does not apply to the world it lands on is a
// step that does nothing. A cold start is the answer to a stop and to nothing
// else — discarding a running guest's memory under it would be a defect, not an
// operation.
func TestAColdStartOfARunningVMDoesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(2, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := coldWorld(t, runtime, "cold-running/")

		if err := world.StoreAll("vm-0", 4); err != nil {
			t.Fatal(err)
		}
		if err := world.StartCold(ctx, "vm-0", 1); err != nil {
			t.Fatalf("cold starting a VM that is running: %v", err)
		}
		if at := world.HostOf("vm-0"); at != 0 {
			t.Fatalf("a cold start moved a running VM to host-%d", at)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a cold start of a running VM took its memory: %v", err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
