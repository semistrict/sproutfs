package simtest_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestASurveyEndsTheHoldsOfAFanOutOntoItselfThatNothingReleased: a fan-out onto
// the parent's own host takes both children in, and the orchestrator restarts
// before it releases either. The children are served nothing, so the page
// server knows nothing of them, but the host holds the fork point for each and
// the parent stays sealed.
//
// The host reports both holds, and each child owes nothing, because it was
// taken in over the point. So the survey at the next step releases them, and
// the parent is checkpointable again long before the host's own deadline would
// have retired the holds.
func TestASurveyEndsTheHoldsOfAFanOutOntoItselfThatNothingReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 23,
			Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
				ConnectLatency: time.Microsecond},
			ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
				PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
				BytesPerSecond: 1 << 40}})
		prefix, err := platform.NewObjectPrefix("sproutfs/")
		if err != nil {
			t.Fatal(err)
		}
		volumes := []volume.VolumeSpec{
			{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
			{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}
		topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
			VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: volumes}}}
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
		if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		// Stores that live only in the parent's pages, which the point holds.
		if err := world.Store(ctx, "vm-1", 3, choose); err != nil {
			t.Fatal(err)
		}
		before := world.VM("vm-1").Status().Checkpoint.Sequence

		forgotten := simtest.ForgottenReleases()
		if err := forgotten.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		children := []simtest.VMSpec{
			{ID: "vm-1-a", Parent: "vm-1", Host: 0, Volumes: volumes},
			{ID: "vm-1-b", Parent: "vm-1", Host: 0, Volumes: volumes},
		}
		if err := world.FanOut(ctx, "vm-1", children); err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			if host := world.HostOf(child.ID); host != 0 {
				t.Fatalf("%s runs on host %d, want the parent's own", child.ID, host)
			}
		}
		// Nothing released either hold, so the parent is still sealed, and its
		// host reports both holds as handovers that owe nothing.
		if !world.VM("vm-1").Status().Sealed {
			t.Fatal("the parent is not sealed while its host holds the point for two children")
		}
		if err := world.VerifyHandovers(); err != nil {
			t.Fatal(err)
		}
		status := world.Host(0).Status()
		if want := []string{"vm-1-a", "vm-1-b"}; !slices.Equal(status.Serving, want) {
			t.Fatalf("the parent's host reports serving %v, want %v", status.Serving, want)
		}
		for _, child := range children {
			if owed := status.Outstanding[child.ID]; owed != 0 {
				t.Fatalf("%s was taken in and its hold still owes %d pages", child.ID, owed)
			}
		}
		if err := forgotten.End(ctx, world); err != nil {
			t.Fatal(err)
		}

		// The next step's survey.
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if err := forgotten.Holds(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-1"); err != nil {
			t.Fatalf("checkpointing the parent after the survey: %v", err)
		}
		if after := world.VM("vm-1").Status().Checkpoint.Sequence; after == before {
			t.Fatalf("the parent is still at checkpoint %d after the survey", after)
		}
		deadline := 4 * host.DefaultCheckpointInterval
		if waited := time.Since(began); waited >= deadline {
			t.Fatalf("the parent was durable again after %s, which is the hold deadline of %s "+
				"rather than the survey ending the holds", waited, deadline)
		}

		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
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
