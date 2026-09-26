package simtest_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestARemoteForkWhoseParentIsCutOffEndsAtItsHold is the migration's rule for
// a fork's child on another host. The child fetches the pages its parent
// sealed from the parent's host, and asks for them until they arrive, as a
// migration's destination does. Here the parent's host is cut off while its
// pod is still listed: its process runs and it holds the fork point behind a
// partition. Nothing it says can end the child's receive, and silence is no
// evidence. Its hold is: the host promised the point for that long and no
// longer. The receive ends then, the fork does not happen, and the parent
// keeps running where it was.
//
// The proof that it is the hold and not a timeout is the clock: the receive
// ends when the hold does, well inside the harness's patience, while the
// parent's host is still up and still holding the point.
func TestARemoteForkWhoseParentIsCutOffEndsAtItsHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 17,
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
		// A hold well inside the harness's patience, so that whichever ends the
		// wait is plain from the clock.
		const hold = simtest.Deadline / 3
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf, Hold: hold})
		choose := func(int) int { return 0 }
		// A checkpoint to inherit, and then stores that live only in the
		// parent's pages: those are what the child has to fetch from its
		// parent's host.
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}

		// The child's post-copy is held at its first frame, so the parent's
		// host is cut off while the child is in it.
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		forked := make(chan error, 1)
		go func() {
			forked <- world.Fork(ctx, simtest.VMSpec{ID: "vm-1-a", Parent: "vm-1", Host: 1,
				Volumes: topology.VMs[0].Volumes})
		}()
		waitFor(t, func() bool {
			return slices.Contains(world.Host(0).Status().Serving, "vm-1-a")
		}, "the fork never handed its child over")
		began := time.Now()
		isolated := simtest.IsolatedHost(0)
		if err := isolated.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-forked; err != nil {
			t.Fatal(err)
		}
		// The hold began as the handoffs arrived, a moment before this clock
		// did, so the receive ends no later than the hold from here and not
		// much sooner.
		if waited := time.Since(began); waited < hold-time.Second || waited > hold {
			t.Fatalf("the child's receive ended %s after its parent's host was cut off, want the end of its %s hold",
				waited, hold)
		}
		// The fork did not happen: the child is nowhere, and its parent runs
		// where it was.
		if world.Exists("vm-1-a") {
			t.Fatal("the child of a fork whose parent's host was cut off exists")
		}
		if host := world.HostOf("vm-1"); host != 0 {
			t.Fatalf("the parent is on host %d", host)
		}
		// The parent's host had nothing to do with it: it is up and still
		// holds the point, and its own clock has not reached its deadline.
		if !slices.Contains(world.Host(0).Status().Serving, "vm-1-a") {
			t.Fatal("the parent's host stopped holding the point before its own deadline")
		}
		// When its clock gets there, the host retires the point on its own,
		// which is the promise the deployment acted on, and the parent can be
		// checkpointed again.
		if retired := world.Clock(0).Advance(hold); retired == 0 {
			t.Fatal("the parent's host keeps no deadline for the point it holds")
		}
		world.Clock(0).Settle()
		if serving := world.Host(0).Status().Serving; len(serving) != 0 {
			t.Fatalf("the parent's host still holds %v past its hold", serving)
		}
		if err := isolated.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := isolated.Holds(ctx, world); err != nil {
			t.Error(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
			t.Error(err)
		}
		if err := world.VerifyHandovers(); err != nil {
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
