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

// TestLosingTheSourceOfAMigrationEndsIt is the deployment's half of the one
// post-copy rule. A destination asks the host that handed it the VM for the
// pages no checkpoint holds until they arrive: they exist nowhere else, and
// nothing in the destination can tell a source that stumbled from one that
// died. What ends the asking when the source really is gone is the control
// plane, which is the one thing that knows — it discards the half-received VM
// and the VM comes back at the checkpoint its control record selects.
//
// The proof that it is the rule and not a timeout is the clock: the migration
// ends in the moment the host is lost, not at the end of anything's patience.
func TestLosingTheSourceOfAMigrationEndsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 11,
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
		// A checkpoint to come back to, and then stores that live only in the
		// source's pages, which are exactly the pages the destination has to
		// fetch and exactly what the loss costs.
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}

		// The destination's post-copy is held at its first frame, so the source
		// is lost while the migration is in it rather than before or after.
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		migrated := make(chan error, 1)
		go func() { migrated <- world.Migrate(ctx, "vm-1", 1) }()
		// A source serves the VM's pages from the moment it hands it over until
		// the destination reports having them all, so this is the post-copy
		// itself rather than a guess at when one has started.
		waitFor(t, func() bool {
			return slices.Contains(world.Host(0).Status().Serving, "vm-1")
		}, "the migration never reached its post-copy")
		began := time.Now()
		if err := world.LoseHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-migrated; err != nil {
			t.Fatal(err)
		}
		// Nothing waited anything out: losing the host is the answer, and the
		// answer arrived with it.
		if waited := time.Since(began); waited > simtest.Deadline/4 {
			t.Fatalf("the migration ended %s after its source was lost, which is a timeout rather than the rule", waited)
		}
		// The VM is running again on the host that is left, at the checkpoint
		// its record selects: the stores since it were only in the pages of
		// the host that is gone.
		if host := world.HostOf("vm-1"); host != 1 {
			t.Fatalf("the VM whose source was lost is on host %d", host)
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

// TestAMigrationWhoseSourceIsCutOffEndsAtItsHold is the same rule for a source
// the deployment cannot reach and cannot call lost, because it is not: its
// process runs, its pod is still listed, and it holds every page the
// destination is waiting for behind a partition. Nothing it says can end the
// migration, and silence is no evidence. Its hold is: the source promised the
// pages for that long and no longer, so once the hold is over they are gone
// whether or not anything can reach it. The migration ends then, and the VM
// comes back at the checkpoint its record selects.
//
// The proof that it is the hold and not a timeout is the clock again: the
// migration ends when the hold does, well inside the harness's patience, while
// the source is still up and still serving.
func TestAMigrationWhoseSourceIsCutOffEndsAtItsHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 13,
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
				{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage}, {Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
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
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}

		// The post-copy is held at its first frame, so the source is cut off
		// while the migration is in it.
		stall := simtest.StalledStream(1)
		if err := stall.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		took := world.Takeovers()
		migrated := make(chan error, 1)
		go func() { migrated <- world.Migrate(ctx, "vm-1", 1) }()
		waitFor(t, func() bool {
			return slices.Contains(world.Host(0).Status().Serving, "vm-1")
		}, "the migration never reached its post-copy")
		began := time.Now()
		isolated := simtest.IsolatedHost(0)
		if err := isolated.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := stall.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := <-migrated; err != nil {
			t.Fatal(err)
		}
		// The hold began as the handoff arrived, a moment before this clock
		// did, so the migration ends no later than the hold from here and not
		// much sooner.
		if waited := time.Since(began); waited < hold-time.Second || waited > hold {
			t.Fatalf("the migration ended %s after its source was cut off, want the end of its %s hold", waited, hold)
		}
		if host := world.HostOf("vm-1"); host != 1 {
			t.Fatalf("the VM whose source was cut off is on host %d", host)
		}
		if world.Takeovers() != took+1 {
			t.Fatalf("the VM was taken over %d times, want once", world.Takeovers()-took)
		}
		// The source had nothing to do with it: it is up and still serving, and
		// its own clock has not reached its deadline.
		if !slices.Contains(world.Host(0).Status().Serving, "vm-1") {
			t.Fatal("the source stopped serving the VM before its own deadline")
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
		// When its clock gets there, the source gives the pages up on its own,
		// which is the promise the deployment acted on.
		if released := world.Clock(0).Advance(hold); released == 0 {
			t.Fatal("the source keeps no deadline for the pages it handed over")
		}
		world.Clock(0).Settle()
		if serving := world.Host(0).Status().Serving; len(serving) != 0 {
			t.Fatalf("the source still serves %v past its hold", serving)
		}
		if err := isolated.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := isolated.Holds(ctx, world); err != nil {
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

// waitFor runs the world on until what is waited for is true, in simulated
// time: a poll costs the deployment nothing and the clock everything, which is
// what lets a test stand at a moment inside an operation rather than guess at
// one.
func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(simtest.Deadline)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}
