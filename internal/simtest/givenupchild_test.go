package simtest_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestAForkChildWhoseReceiveOutlivesItsFanOutNeverRuns: the first child's
// receive hangs up on its caller as its destination starts the guest, so the
// fan-out fails there and gives up the hold of both children. The receive goes
// on, and the parent wrote nothing no checkpoint has, so it gets every page the
// child inherited. Its claim then finds the hold gone, and the destination
// discards the child rather than run it. Nothing of either child is left, and
// the parent runs on where it was.
func TestAForkChildWhoseReceiveOutlivesItsFanOutNeverRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(19)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := givenUpWorld(t, ctx, runtime)
		outlived := simtest.OutlivedReceive(1, time.Second)
		if err := outlived.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.FanOut(ctx, "vm-1", givenUpChildren()); err != nil {
			t.Fatal(err)
		}
		if err := outlived.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			return !slices.Contains(world.Host(1).Status().Receiving, "vm-1-a")
		}, "the receive that outlived its fan-out never ended")
		if started := world.ReceivedGuests("vm-1-a"); started != 1 {
			t.Fatalf("the destination started %d guests of the first child, want the one it discarded", started)
		}
		requireNoChildren(t, ctx, world)
	})
}

// TestAForkChildWhoseAnswerWasLostIsDeleted: the destination took the first
// child in and claimed its hold, and the answer never reached the fan-out, so
// the fan-out failed there. Its give-up on the parent's host comes after the
// claim and says so, and the child, which runs under an identity only the
// failed fan-out knew, is deleted. The second child is never started.
func TestAForkChildWhoseAnswerWasLostIsDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(23)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := givenUpWorld(t, ctx, runtime)
		lost := simtest.LostReceiveAnswer(1)
		if err := lost.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.FanOut(ctx, "vm-1", givenUpChildren()); err != nil {
			t.Fatal(err)
		}
		if err := lost.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if started := world.ReceivedGuests("vm-1-a"); started != 1 {
			t.Fatalf("the destination started %d guests of the first child, want the one it took in", started)
		}
		requireNoChildren(t, ctx, world)
	})
}

// givenUpWorld is two hosts and a parent on the first that has published
// everything it wrote, so a child of it needs nothing from its host but the
// hold.
func givenUpWorld(t *testing.T, ctx context.Context, runtime *sim.Runtime) *simtest.World {
	t.Helper()
	world := retryWorld(t, ctx, runtime, 2)
	if err := world.Checkpoint(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	return world
}

// givenUpChildren is a fan-out of two children of vm-1 onto the second host.
func givenUpChildren() []simtest.VMSpec {
	volumes := []volume.VolumeSpec{
		{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}
	return []simtest.VMSpec{
		{ID: "vm-1-a", Parent: "vm-1", Host: 1, Volumes: volumes},
		{ID: "vm-1-b", Parent: "vm-1", Host: 1, Volumes: volumes}}
}

// requireNoChildren is what a fan-out that did not happen owes: neither child
// exists or runs anywhere, the second was never started, the parent's host
// holds nothing for either, every identity is free, and the parent runs on
// where it was with every write it made.
func requireNoChildren(t *testing.T, ctx context.Context, world *simtest.World) {
	t.Helper()
	if err := world.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		for _, vm := range world.Host(index).Volumes().VMs() {
			if vm.ID() != "vm-1" {
				t.Fatalf("host %d holds %s after its fan-out failed", index, vm.ID())
			}
		}
	}
	for _, child := range []string{"vm-1-a", "vm-1-b"} {
		if world.Exists(child) {
			t.Fatalf("%s exists after its fan-out failed", child)
		}
	}
	if started := world.ReceivedGuests("vm-1-b"); started != 0 {
		t.Fatalf("the second child was started %d times, want never", started)
	}
	if serving := world.Host(0).Status().Serving; len(serving) != 0 {
		t.Fatalf("the parent's host still holds %v after the fan-out failed", serving)
	}
	if orphans := world.Orphans(); len(orphans) != 0 {
		t.Fatalf("the identities %v are still not free", orphans)
	}
	if at := world.HostOf("vm-1"); at != 0 {
		t.Fatalf("the parent is on host %d, want host-0, where it was", at)
	}
	if err := world.VerifyHandovers(); err != nil {
		t.Error(err)
	}
	requireIntact(t, ctx, world)
}

// TestALocalForkChildWhoseReceiveOutlivesItsFanOutNeverRuns is the same for a
// child on its parent's own host, which claims its hold as it is bound to the
// fork point. The parent has written pages no checkpoint has, so the point
// seals them, and the child would map them rather than fetch them. The claim
// finds the hold given up, the child is discarded, and the parent's seal ends
// with it.
func TestALocalForkChildWhoseReceiveOutlivesItsFanOutNeverRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(29)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 2)
		outlived := simtest.OutlivedReceive(0, time.Second)
		if err := outlived.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		children := givenUpChildren()
		for index := range children {
			children[index].Host = 0
		}
		if err := world.FanOut(ctx, "vm-1", children); err != nil {
			t.Fatal(err)
		}
		if err := outlived.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			return !slices.Contains(world.Host(0).Status().Receiving, "vm-1-a")
		}, "the receive that outlived its fan-out never ended")
		if started := world.ReceivedGuests("vm-1-a"); started != 1 {
			t.Fatalf("the host started %d guests of the first child, want the one it discarded", started)
		}
		requireNoChildren(t, ctx, world)
	})
}
