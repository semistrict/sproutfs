package simtest_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// rootVolume is the single volume a handle-only VM owns here.
var rootVolume = []volume.VolumeSpec{{Name: "root", Size: 8192}}

// TestAKilledHostRestartsOnItsOwnDiskAndRewindsToItsLastCheckpoint: a host that
// closes gives everything it holds one last checkpoint, so an orderly shutdown
// loses nothing; a host that dies loses every write since each VM's last
// checkpoint, and that is the whole difference.
//
// The same script runs under three endings — the orderly close, a process crash
// that leaves the disk alone, and a power loss that resolves everything the disk
// had not synced. Whichever it was, the host starts again in this process on the
// disk it left behind, and what it recovers is what the deployment's object
// store holds: a handle write survives exactly when the close published it, and
// a guest's stores survive only as far as the guest's last checkpoint, because
// nothing but a capture ever publishes a frame.
func TestAKilledHostRestartsOnItsOwnDiskAndRewindsToItsLastCheckpoint(t *testing.T) {
	endings := []struct {
		name string
		// end takes host 0 away, and published says whether what its handles
		// were holding reached the store on the way out.
		end       func(ctx context.Context, w *simtest.World) error
		published bool
		// outcome is what the trace must call this ending, and "" is a close,
		// which is no kill at all.
		outcome string
	}{
		{name: "orderly close", published: true,
			end: func(ctx context.Context, w *simtest.World) error { return w.Shutdown(ctx, 0) }},
		{name: "process crash", outcome: "process",
			end: func(ctx context.Context, w *simtest.World) error { return w.Kill(ctx, 0, sim.CrashProcess) }},
		{name: "power loss", outcome: "power_loss",
			end: func(ctx context.Context, w *simtest.World) error { return w.Kill(ctx, 0, sim.PowerLoss) }},
	}
	for _, ending := range endings {
		t.Run(ending.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := newCrashRuntime(1)
				prefix := newPrefix(t, "ending/")
				ctx := sim.WithRuntime(t.Context(), runtime)
				topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
					VMs: []simtest.VMSpec{{ID: "vm-running", Host: 0,
						Volumes: []volume.VolumeSpec{{Name: "ram0", Size: 4 * simtest.PageSize}}}}}
				world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
					Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})
				// One VM written through its handle, which a close publishes,
				// and one run by a guest whose stores live in frames, which only
				// a capture ever publishes.
				written, err := world.Host(0).Volumes().Create(ctx, "vm-written", rootVolume)
				if err != nil {
					t.Fatal(err)
				}
				write(t, ctx, written, 0, "published before the ending")
				if err := written.Checkpoint(ctx); err != nil {
					t.Fatal(err)
				}
				write(t, ctx, written, 64, "held when the ending came")

				if err := world.StoreAll("vm-running", 1); err != nil {
					t.Fatal(err)
				}
				if err := world.Checkpoint(ctx, "vm-running"); err != nil {
					t.Fatal(err)
				}
				if err := world.StoreAll("vm-running", 2); err != nil {
					t.Fatal(err)
				}

				before := len(kills(runtime))
				if err := ending.end(ctx, world); err != nil {
					t.Fatal(err)
				}
				traced := kills(runtime)[before:]
				if ending.outcome == "" {
					if len(traced) != 0 {
						t.Fatalf("an orderly close traced %d kills", len(traced))
					}
				} else {
					if len(traced) != 1 {
						t.Fatalf("the ending traced %d kills, want one", len(traced))
					}
					if traced[0].Resource != "host-0" || traced[0].Outcome != ending.outcome {
						t.Fatalf("the kill was traced as %s/%s, want host-0/%s",
							traced[0].Resource, traced[0].Outcome, ending.outcome)
					}
				}

				// Back in this process, on the disk the ending left behind.
				if err := world.Restart(ctx, 0); err != nil {
					t.Fatal(err)
				}
				if got := world.Incarnation(0); got != 2 {
					t.Fatalf("the host came back as incarnation %d, want its second", got)
				}

				// A handle write is durable exactly when the close that
				// published it happened.
				recovered, err := world.Host(0).Volumes().Open(ctx, "vm-written")
				if err != nil {
					t.Fatal(err)
				}
				if got := read(t, ctx, recovered, 0, len("published before the ending")); got != "published before the ending" {
					t.Fatalf("the published write came back as %q", got)
				}
				held := read(t, ctx, recovered, 64, len("held when the ending came"))
				if ending.published && held != "held when the ending came" {
					t.Fatalf("an orderly close did not publish what the handle held: %q", held)
				}
				if !ending.published && held != string(make([]byte, len(held))) {
					t.Fatalf("a killed host published %q, which no checkpoint of it held", held)
				}
				if err := recovered.Close(ctx); err != nil {
					t.Fatal(err)
				}

				// A guest's stores reach the store through a capture and nothing
				// else, so the ending does not change what the guest is
				// recovered at: the checkpoint it last took. The world's own
				// model is what says so — it rewinds to exactly the bytes that
				// checkpoint published, page for page.
				if err := world.Settle(ctx); err != nil {
					t.Fatal(err)
				}
				if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
					t.Fatal(err)
				}
				if err := world.VerifyDurable(ctx, "vm-running"); err != nil {
					t.Fatal(err)
				}
				if err := world.VerifyLossWindow(ctx, "vm-running"); err != nil {
					t.Fatal(err)
				}
				if err := world.Close(ctx); err != nil {
					t.Error(err)
				}
				if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
					volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
					volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
					t.Error(err)
				}
			})
		})
	}
}

// TestAKilledHostIsTakenOverByAnotherHostAtItsLastCheckpoint: the restart is one
// of two ways a VM comes back, and the other is the one a deployment reaches
// first — a surviving host takes the record over while the dead one is still
// dead. What it opens is the checkpoint that record selects, which is the last
// one the killed host published and nothing of what it held.
func TestAKilledHostIsTakenOverByAnotherHostAtItsLastCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCrashRuntime(1)
		prefix := newPrefix(t, "takeover/")
		ctx := sim.WithRuntime(t.Context(), runtime)
		topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
			VMs: []simtest.VMSpec{{ID: "vm-running", Host: 0,
				Volumes: []volume.VolumeSpec{{Name: "ram0", Size: 2 * simtest.PageSize}}}}}
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})
		vm, err := world.Host(0).Volumes().Create(ctx, "vm-1", rootVolume)
		if err != nil {
			t.Fatal(err)
		}
		write(t, ctx, vm, 0, "published")
		if err := vm.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		write(t, ctx, vm, 64, "never published")
		if err := world.StoreAll("vm-running", 1); err != nil {
			t.Fatal(err)
		}
		if err := world.Checkpoint(ctx, "vm-running"); err != nil {
			t.Fatal(err)
		}
		if err := world.StoreAll("vm-running", 2); err != nil {
			t.Fatal(err)
		}
		if err := world.Kill(ctx, 0, sim.PowerLoss); err != nil {
			t.Fatal(err)
		}

		survivor, err := world.Host(1).Volumes().Open(ctx, "vm-1")
		if err != nil {
			t.Fatalf("the surviving host could not take the dead host's VM over: %v", err)
		}
		if got := read(t, ctx, survivor, 0, len("published")); got != "published" {
			t.Fatalf("the takeover reads %q, want the killed host's last checkpoint", got)
		}
		if got := read(t, ctx, survivor, 64, len("never published")); got != string(make([]byte, len("never published"))) {
			t.Fatalf("the takeover reads %q, which no checkpoint holds", got)
		}
		if err := survivor.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// The handle the dead host left open cannot come back and publish over
		// the writer that took its place.
		if err := vm.Checkpoint(ctx); err == nil {
			t.Fatalf("the dead host's handle published %s", vm.Status().Checkpoint)
		}
		// The guest's VM comes back on the survivor at the checkpoint its record
		// selects, which is what the world's model requires of every takeover.
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
			volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
			volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
			t.Error(err)
		}
	})
}

func read(t *testing.T, ctx context.Context, vm *volume.VM, offset uint64, length int) string {
	t.Helper()
	buffer := make([]byte, length)
	if err := vm.Volume("root").Read(ctx, offset, buffer); err != nil {
		t.Fatal(err)
	}
	return string(buffer)
}

func write(t *testing.T, ctx context.Context, vm *volume.VM, offset uint64, data string) {
	t.Helper()
	if err := vm.Volume("root").Write(ctx, offset, []byte(data)); err != nil {
		t.Fatal(err)
	}
}
