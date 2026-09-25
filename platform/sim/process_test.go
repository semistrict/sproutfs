package sim_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestProcessCrashCancelsOwnedGoroutinesAndSupportsRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		process := runtime.NewProcess(sim.ProcessConfig{ID: "log-1"})
		stopped := make(chan struct{})
		if err := process.Start(t.Context(), func(ctx context.Context) {
			<-ctx.Done()
			close(stopped)
		}); err != nil {
			t.Fatal(err)
		}
		if err := process.Go(t.Context(), func(ctx context.Context) { <-ctx.Done() }); err != nil {
			t.Fatal(err)
		}
		if err := process.Crash(t.Context(), sim.CrashProcess); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stopped:
		default:
			t.Fatal("process function did not observe cancellation")
		}
		if err := process.Go(t.Context(), func(context.Context) {}); !errors.Is(err, platform.ErrProcessStopped) {
			t.Fatalf("Go after crash error = %v, want ErrProcessStopped", err)
		}
		if err := process.Start(t.Context(), func(context.Context) {}); err != nil {
			t.Fatal(err)
		}
		got, err := process.Generation(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := uint64(2); got != want {
			t.Fatalf("generation = %d, want %d", got, want)
		}
		if err := process.Crash(t.Context(), sim.CrashProcess); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProcessCrashHonorsContextAndFinishesAfterCooperativeShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		process := sim.New(sim.Config{}).NewProcess(sim.ProcessConfig{ID: "log-1"})
		release := make(chan struct{})
		if err := process.Start(t.Context(), func(context.Context) { <-release }); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := process.Crash(ctx, sim.CrashProcess); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Crash error = %v, want DeadlineExceeded", err)
		}
		close(release)
		if err := process.Crash(t.Context(), sim.CrashProcess); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProcessFailureModesHaveExactDiskSemantics(t *testing.T) {
	tests := []struct {
		name    string
		mode    sim.FailureMode
		outcome string
		check   func(*testing.T, *sim.Disk, platform.File)
	}{
		{
			name: "process crash", mode: sim.CrashProcess, outcome: "process",
			check: func(t *testing.T, disk *sim.Disk, old platform.File) {
				assertFileContent(t, old, "volatile")
				file, err := disk.Open(t.Context(), "state", platform.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				assertFileContent(t, file, "volatile")
			},
		},
		{
			name: "power loss", mode: sim.PowerLoss, outcome: "power_loss",
			check: func(t *testing.T, disk *sim.Disk, old platform.File) {
				if _, err := old.Size(t.Context()); !errors.Is(err, platform.ErrStaleHandle) {
					t.Fatalf("old handle Size error = %v, want ErrStaleHandle", err)
				}
				file, err := disk.Open(t.Context(), "state", platform.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				assertFileContent(t, file, "durable")
			},
		},
		{
			name: "machine destruction", mode: sim.DestroyMachine, outcome: "destroy_machine",
			check: func(t *testing.T, disk *sim.Disk, old platform.File) {
				if _, err := old.Size(t.Context()); !errors.Is(err, platform.ErrStaleHandle) {
					t.Fatalf("old handle Size error = %v, want ErrStaleHandle", err)
				}
				if _, err := disk.Open(t.Context(), "state", platform.OpenOptions{}); !errors.Is(err, platform.ErrNotFound) {
					t.Fatalf("Open after destroy error = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "recoverable disk failure", mode: sim.FailDisk, outcome: "disk_failure",
			check: func(t *testing.T, disk *sim.Disk, old platform.File) {
				if _, err := old.Size(t.Context()); !errors.Is(err, platform.ErrDiskFailed) {
					t.Fatalf("Size while disk failed error = %v, want ErrDiskFailed", err)
				}
				disk.Recover()
				assertFileContent(t, old, "volatile")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{})
				disk := runtime.NewDisk("node-1", sim.DiskConfig{})
				file := openSyncedThenDirty(t, disk, "state", []byte("durable"), []byte("volatile"))
				defer file.Close()

				process := runtime.NewProcess(sim.ProcessConfig{ID: "log-1", Disk: disk})
				if err := process.Start(t.Context(), func(ctx context.Context) { <-ctx.Done() }); err != nil {
					t.Fatal(err)
				}
				runtime.Trace().Reset()
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				if err := process.Crash(ctx, test.mode); err != nil {
					t.Fatal(err)
				}
				assertTraceEvent(t, runtime.Trace().Events(), "process", "crash", test.outcome)
				test.check(t, disk, file)
			})
		})
	}
}

// TestProcessStopLeavesTheDiskAloneAndIsNotTracedAsAKill: a process that ends
// because it was told to is not a machine that died. Its disk keeps everything,
// including what no Sync reached, and the trace calls it a stop — which is what
// lets a campaign that kills hosts read back exactly the kills it staged rather
// than every shutdown in the run.
func TestProcessStopLeavesTheDiskAloneAndIsNotTracedAsAKill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{PowerLossFaults: true})
		file := openSyncedThenDirty(t, disk, "state", []byte("durable"), []byte("volatile"))
		defer file.Close()
		process := runtime.NewProcess(sim.ProcessConfig{ID: "log-1", Disk: disk})
		stopped := make(chan struct{})
		if err := process.Start(t.Context(), func(ctx context.Context) {
			<-ctx.Done()
			close(stopped)
		}); err != nil {
			t.Fatal(err)
		}
		runtime.Trace().Reset()
		if err := process.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stopped:
		default:
			t.Fatal("the process function did not observe cancellation")
		}
		assertTraceEvent(t, runtime.Trace().Events(), "process", "stop", "ok")
		for _, event := range runtime.Trace().Events() {
			if event.Kind == "process" && event.Operation == "crash" {
				t.Fatalf("an orderly stop was traced as a kill: %+v", event)
			}
		}
		assertFileContent(t, file, "volatile")
		// The process can be started again, on the disk it left untouched.
		if err := process.Start(t.Context(), func(context.Context) {}); err != nil {
			t.Fatal(err)
		}
		if err := process.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func assertFileContent(t *testing.T, file platform.File, want string) {
	t.Helper()
	size, err := file.Size(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, size)
	if size > 0 {
		if _, err := file.ReadAt(t.Context(), buffer, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(buffer, []byte(want)) {
		t.Fatalf("file content = %q, want %q", buffer, want)
	}
}

func TestRandomChoicesAreStableAndNamespaced(t *testing.T) {
	t.Parallel()
	first := sim.New(sim.Config{Seed: 99})
	second := sim.New(sim.Config{Seed: 99})
	a := first.Random("network").Uint64("link/a-b/1")
	b := second.Random("network").Uint64("link/a-b/1")
	if a != b {
		t.Fatalf("same seed and key produced %d and %d", a, b)
	}
	if other := first.Random("disk").Uint64("link/a-b/1"); other == a {
		t.Fatal("different namespaces produced the same choice")
	}
}
