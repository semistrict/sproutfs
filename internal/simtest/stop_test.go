package simtest_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// stopTopology is two hosts and one VM, which is the least a stop and a start
// need: somewhere the VM was and somewhere else to bring it back.
func stopTopology() simtest.Topology {
	return simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0,
			Volumes: []volume.VolumeSpec{{Name: "ram0", Size: 4 * simtest.PageSize}}}}}
}

func startStopWorld(t *testing.T, runtime *sim.Runtime, prefix string) (*simtest.World, simtest.Topology) {
	t.Helper()
	topology := stopTopology()
	world := simtest.MustStart(t, sim.WithRuntime(t.Context(), runtime), simtest.Config{
		Runtime: runtime, Topology: topology, Knobs: campaignKnobs(t, runtime, topology),
		Prefix: newPrefix(t, prefix), Log: t.Logf})
	return world, topology
}

// TestAStoppedVMKeepsWhatItsGuestWroteAndComesBackAtIt: a stop publishes
// everything the guest holds and closes it, so the difference between a stop
// and a host loss is exactly the writes since the last checkpoint. The start
// reads every page back through the restarted guest's own fault path, which is
// the only place a page the pager rebuilt wrongly shows.
func TestAStoppedVMKeepsWhatItsGuestWroteAndComesBackAtIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(1, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "stopped/")

		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		// Written after that checkpoint, so only the host's frames hold it: a
		// stop that published nothing would lose it the way a kill does.
		if err := world.StoreAll("vm-0", 7); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at >= 0 {
			t.Fatalf("a stopped VM is still running, on host-%d", at)
		}
		if !world.Exists("vm-0") {
			t.Fatal("a stopped VM stopped existing: it is still a control record and its objects")
		}
		if running := world.Host(0).Machines(); len(running) != 0 {
			t.Fatalf("host-0 still runs %v after stopping it", running)
		}
		// A stopped VM is left where it is. Settle repairs what a fault left
		// half done, and a VM the deployment stopped on purpose is not that.
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if running := world.Host(0).Machines(); len(running) != 0 {
			t.Fatalf("settling started %v again, and nothing asked it to", running)
		}

		// The start requires the VM to come back at a checkpoint one of its own
		// writers published, reading exactly the bytes that checkpoint holds.
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the started VM is on host-%d, want the host it was started on", at)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a started VM reads back something its guest did not write: %v", err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStoppingAVMTwiceDoesNothingTheSecondTime, and starting one that is
// already running does nothing either. Every operation of a schedule is drawn
// rather than chosen, so each has to be a step that does nothing on a world it
// does not apply to rather than a failure.
func TestStoppingAVMTwiceDoesNothingTheSecondTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(2, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "twice/")

		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatalf("stopping a VM that is already stopped: %v", err)
		}
		if err := world.Start(ctx, "vm-0", 0); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatalf("starting a VM that is already running: %v", err)
		}
		if at := world.HostOf("vm-0"); at != 0 {
			t.Fatalf("the second start moved the VM to host-%d", at)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAStoppedVMIsStartedByASettleWhenItsHostIsLost: a stopped VM is not
// running anywhere, so losing the host it last ran on takes nothing from it —
// there were no frames left to lose. What it comes back as is the checkpoint
// its stop published, on whichever host starts it.
func TestAStoppedVMSurvivesLosingTheHostItRanOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(3, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "lost/")

		if err := world.StoreAll("vm-0", 5); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Kill(ctx, 0, sim.CrashProcess); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatalf("starting a stopped VM after its old host died: %v", err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
