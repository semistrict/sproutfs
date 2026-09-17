package simtest_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// forkedStopTopology is two hosts, one VM and a fork of it on the other host,
// which is the shape a soak's round leaves behind: most of the population is
// children, and the round's stops and starts fall on them as readily as on the
// VMs something created.
func forkedStopTopology() simtest.Topology {
	ram := []volume.VolumeSpec{{Name: "ram0", Size: 4 * simtest.PageSize}}
	return simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{
			{ID: "vm-0", Host: 0, Volumes: ram},
			{ID: "vm-1", Parent: "vm-0", Host: 1, Volumes: ram},
		}}
}

func startForkedWorld(t *testing.T, runtime *sim.Runtime, prefix string) *simtest.World {
	t.Helper()
	topology := forkedStopTopology()
	world, err := simtest.Start(sim.WithRuntime(t.Context(), runtime), simtest.Config{
		Runtime: runtime, Topology: topology, Knobs: campaignKnobs(t, runtime, topology),
		Prefix: newPrefix(t, prefix), Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return world
}

// TestStoppingAndStartingOneVMOverAndOverKeepsWhatItsGuestWrote: a soak stops
// and starts its share of the population every round, so one identity goes
// round this loop many times over the same pair of hosts. Every turn has to
// publish exactly what the guest held and come back at it: a turn that lost the
// writes since the last checkpoint would look like a host loss, and a turn that
// kept a handle, a registration or a frame would be a host that cannot take the
// next one.
func TestStoppingAndStartingOneVMOverAndOverKeepsWhatItsGuestWrote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(11, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "over-and-over/")

		for turn := range 8 {
			// Written after the last checkpoint, so only the host's frames hold
			// it: a stop that published nothing would lose it as a kill does.
			if err := world.StoreAll("vm-0", byte(turn+1)); err != nil {
				t.Fatal(err)
			}
			if err := world.Stop(ctx, "vm-0"); err != nil {
				t.Fatal(err)
			}
			if running := world.Started(); len(running) != 0 {
				t.Fatalf("turn %d: %v is still running after the stop", turn, running)
			}
			at := (turn + 1) % 2
			if err := world.Start(ctx, "vm-0", at); err != nil {
				t.Fatal(err)
			}
			if on := world.HostOf("vm-0"); on != at {
				t.Fatalf("turn %d: the started VM is on host-%d, want host-%d", turn, on, at)
			}
			if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAForksChildIsStoppedAndStartedLikeAnyOtherVM: a child holds a pin on the
// lineage it was forked from and reads the checkpoint that pin protects for
// every page it has not written. A stop publishes what its guest holds and
// closes it; a start elsewhere opens it from the record, which is where the
// inherited half has to come back from the objects the pin kept rather than
// from the frames the stop gave away.
func TestAForksChildIsStoppedAndStartedLikeAnyOtherVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(12, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := startForkedWorld(t, runtime, "child-stop/")

		if err := world.StoreAll("vm-0", 3); err != nil {
			t.Fatal(err)
		}
		if err := world.Fork(ctx, forkedStopTopology().VMs[1]); err != nil {
			t.Fatal(err)
		}
		if on := world.HostOf("vm-1"); on != 1 {
			t.Fatalf("the child is on host-%d, want the host it was forked onto", on)
		}
		// The child diverges from what it inherited, so what it comes back at
		// is its own and not its parent's.
		if err := world.StoreAll("vm-1", 4); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-1", 0); err != nil {
			t.Fatal(err)
		}
		if on := world.HostOf("vm-1"); on != 0 {
			t.Fatalf("the started child is on host-%d, want host-0", on)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("a started child reads back something its guest did not write: %v", err)
		}
		// And the parent is untouched by any of it.
		if err := world.Checkpoint(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStoppingAParentAndStartingItAgainLeavesItsChildrenAlone: the soak stops a
// share of every round's population, and a parent is as likely to be drawn as
// anything else. A parent's stop closes the VMM process whose frames its
// children read the instant out of — which is why it is refused while an
// instant holds it — so once every child has what it inherited the stop must
// cost them nothing at all.
func TestStoppingAParentAndStartingItAgainLeavesItsChildrenAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(13, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := startForkedWorld(t, runtime, "parent-stop/")

		if err := world.StoreAll("vm-0", 5); err != nil {
			t.Fatal(err)
		}
		if err := world.Fork(ctx, forkedStopTopology().VMs[1]); err != nil {
			t.Fatal(err)
		}
		if err := world.StoreAll("vm-0", 6); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatalf("the child of a stopped parent: %v", err)
		}
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		// Deleting them both is what the end of a soak does, and what it leaves
		// is the lineage the fork pinned.
		for _, id := range []string{"vm-0", "vm-1"} {
			if err := world.Delete(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDeletingAStoppedVMRemovesIt: the end of a soak deletes every VM, and a
// share of them are stopped — no host runs them, so there is no host to route
// the delete to and any host that is up removes the record and the objects.
func TestDeletingAStoppedVMRemovesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(14, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "stopped-delete/")

		if err := world.StoreAll("vm-0", 8); err != nil {
			t.Fatal(err)
		}
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Delete(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if stopped := world.Stopped(); len(stopped) != 0 {
			t.Fatalf("%v is still stopped after being deleted", stopped)
		}
		// Nothing starts it again: it is not a VM any more.
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if on := world.HostOf("vm-0"); on >= 0 {
			t.Fatalf("a deleted VM was started on host-%d", on)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestADeliberateStartIsNotATakeover: Takeovers counts the VMs a host opened
// again because whatever was running them stopped — a lost host, a migration
// that could not be undone, a post-copy that did not finish — and a campaign
// asserts on it to say its fault reached the takeover it is about rather than
// being absorbed. A start runs the same open, and counting it there would let a
// stop and a start drawn by the schedule satisfy an assertion about a fault.
func TestADeliberateStartIsNotATakeover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(15, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, _ := startStopWorld(t, runtime, "start-not-takeover/")

		took := world.Takeovers()
		if err := world.Stop(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Start(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if world.Takeovers() != took {
			t.Fatalf("a stop and a start counted %d takeovers", world.Takeovers()-took)
		}
		// And losing the host it was started on still counts as one.
		if err := world.Kill(ctx, 1, sim.CrashProcess); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if world.Takeovers() == took {
			t.Fatal("losing the host running a VM counted no takeover")
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
