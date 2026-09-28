package simtest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// TestExplainTheLifeOfAVM is the scenario the interactive explainer animates
// (TASK-65), run by the real code: a VM writes and is checkpointed, is forked
// onto a second host, is migrated there, and its first host is lost and
// started again. Each step is marked in the simulator's own trace, beside every
// object-store request, network exchange, disk write and process event the
// step caused. With SPROUTFS_EXPLAIN=1 the trace is printed as one JSON event
// per line, each after "EXPLAIN ", which is how the explainer's page reads it
// out of this test compiled to WebAssembly.
func TestExplainTheLifeOfAVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(41)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 2)
		step := func(name string, run func() error) {
			t.Helper()
			runtime.Trace().Record(sim.Event{Kind: "step", Resource: "world", Operation: name, Outcome: "begin"})
			if err := run(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			runtime.Trace().Record(sim.Event{Kind: "step", Resource: "world", Operation: name, Outcome: "end"})
		}
		choose := func(int) int { return 0 }
		step("store", func() error { return world.Store(ctx, "vm-1", 3, choose) })
		step("checkpoint", func() error { return world.Checkpoint(ctx, "vm-1") })
		step("fork", func() error {
			return world.Fork(ctx, simtest.VMSpec{ID: "vm-1-a", Parent: "vm-1", Host: 1, Volumes: []volume.VolumeSpec{
				{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
				{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}})
		})
		step("migrate", func() error { return world.Migrate(ctx, "vm-1", 1) })
		step("lose-host", func() error { return world.LoseHost(ctx, 0) })
		step("restart-host", func() error { return world.RestartHost(ctx, 0) })
		step("settle", func() error { return world.Settle(ctx) })
		if at := world.HostOf("vm-1"); at != 1 {
			t.Fatalf("vm-1 is on host %d, want host-1, where it was migrated", at)
		}
		if at := world.HostOf("vm-1-a"); at != 1 {
			t.Fatalf("the child is on host %d, want host-1, where it was forked", at)
		}
		if os.Getenv("SPROUTFS_EXPLAIN") == "1" {
			explain(t, runtime.Trace().Events())
		}
		requireIntact(t, ctx, world)
	})
}

// explain prints the trace for the explainer's page, one JSON event a line.
func explain(t *testing.T, events []sim.Event) {
	t.Helper()
	for _, event := range events {
		line, err := json.Marshal(struct {
			Sequence  uint64 `json:"seq"`
			AtNS      int64  `json:"at_ns"`
			Kind      string `json:"kind"`
			Resource  string `json:"resource"`
			Operation string `json:"op"`
			Outcome   string `json:"outcome"`
			Bytes     int    `json:"bytes"`
		}{event.Sequence, event.At.UnixNano(), event.Kind, event.Resource, event.Operation, event.Outcome, event.Bytes})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("EXPLAIN %s\n", line)
	}
}
