package simtest_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
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
				{Name: "ram0", Size: 4 * simtest.PageSize}, {Name: "disk", Size: 2 * simtest.PageSize}}}}}
		k := knobs.Defaults()
		k.ResidentPages, k.DirtyPages, k.LogicalPages = 32, 32, 128
		k.ReadAheadPages, k.WriteAheadPages = 1, 1
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), runtime)
		world, err := simtest.Start(ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: k, Prefix: prefix, Log: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		choose := func(int) int { return 0 }
		// A checkpoint to come back to, and then stores that live only in the
		// source's frames, which are exactly the pages the destination has to
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
		// its record selects: the stores since it were only in the frames of
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
// what lets a test stand at an instant inside an operation rather than guess at
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
