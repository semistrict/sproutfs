package vmmachine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
)

// simulated runs f in a synctest bubble with a context whose runtime enables
// the guards SPROUTFS_SIM_BUG names, so a guard's test runs the guard.
func simulated(t *testing.T, f func(t *testing.T, ctx context.Context)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f(t, sim.WithRuntime(t.Context(), sim.New(sim.Config{})))
	})
}

// TestARestoreMovesTheGuestsClockOnWhereTheVMMCan: a restore's load request
// carries what the Starter added, the state, the RAM and PMEM sockets, a guest
// left paused, and on x86_64 the clock moved on by the time the state spent
// stopped. Firecracker refuses that on aarch64, so there the clock stays.
func TestARestoreMovesTheGuestsClockOnWhereTheVMMCan(t *testing.T) {
	simulated(t, func(t *testing.T, ctx context.Context) {
		starter := map[string]any{"vsock_override": map[string]any{"uds_path": "/vm/vsock.sock"}}
		pmem := []ManagedPmem{{ID: "root", Root: true, Socket: "/vm/root.sock"}}
		for arch, moves := range map[string]bool{"amd64": true, "arm64": false} {
			load, err := loadRequest(ctx, starter, "/vm/state/restore.state", "/vm/ram.sock", pmem, arch)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"vsock_override": map[string]any{"uds_path": "/vm/vsock.sock"},
				"snapshot_path":  "/vm/state/restore.state",
				"mem_backend":    map[string]any{"backend_type": "Sproutfs", "backend_path": "/vm/ram.sock"},
				"pmem_overrides": []map[string]any{{"id": "root", "socket_path": "/vm/root.sock"}},
				"resume_vm":      false,
				"clock_realtime": moves,
			}
			if !reflect.DeepEqual(load, want) {
				t.Fatalf("on %s the load request is\n%v\nwant\n%v", arch, load, want)
			}
		}
	})
}

// TestARestoreRefusesAStarterThatNamesTheClock: whether the clock moves is this
// package's to say, as everything else it loads is.
func TestARestoreRefusesAStarterThatNamesTheClock(t *testing.T) {
	simulated(t, func(t *testing.T, ctx context.Context) {
		_, err := loadRequest(ctx, map[string]any{"clock_realtime": false}, "/vm/state/restore.state",
			"/vm/ram.sock", nil, "amd64")
		if err == nil || !strings.Contains(err.Error(), `the Starter's load request names "clock_realtime"`) {
			t.Fatalf("got %v, want the Starter's clock refused", err)
		}
	})
}
