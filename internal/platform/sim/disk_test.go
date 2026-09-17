package sim_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

func TestDiskPowerLossPreservesOnlySyncedContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{})
		file := openSyncedThenDirty(t, disk, "tail.log", []byte("durable"), []byte("volatile"))

		if err := disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Size(t.Context()); !errors.Is(err, platform.ErrStaleHandle) {
			t.Fatalf("old handle error = %v, want ErrStaleHandle", err)
		}
		file, err := disk.Open(t.Context(), "tail.log", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertFileContent(t, file, "durable")
	})
}

func TestCanceledDiskOperationPreservesPendingFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		disk.FailNext(sim.DiskRemove, 1)
		for range 100 {
			if err := disk.Remove(ctx, "tail.log"); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Remove = %v, want Canceled", err)
			}
		}
		if events := runtime.Trace().Events(); len(events) != 0 {
			t.Fatalf("canceled operations entered the disk: %v", events)
		}
		if err := disk.Remove(t.Context(), "tail.log"); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("next Remove = %v, want pending injected fault", err)
		}
	})
}

// openSyncedThenDirty creates name holding durable bytes that survive a power
// loss, then overwrites them with unsynced volatile bytes.
func openSyncedThenDirty(t *testing.T, disk *sim.Disk, name string, durable, volatile []byte) platform.File {
	t.Helper()
	file, err := disk.Open(t.Context(), name, platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(t.Context(), durable, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(t.Context(), volatile, 0); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestDiskCanInjectTornWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{})
		file, err := disk.Open(context.Background(), "tail.log", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		runtime.Trace().Reset()
		disk.TearNextWrite(3)
		n, err := file.WriteAt(t.Context(), []byte("abcdef"), 0)
		if n != 3 || !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("WriteAt = (%d, %v), want (3, ErrInjectedFault)", n, err)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "disk", string(sim.DiskWrite), "torn")
	})
}

func TestDiskRejectsRangesThatCannotFitTheInMemoryImage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := sim.New(sim.Config{}).NewDisk("node-1", sim.DiskConfig{})
		file, err := disk.Open(t.Context(), "tail.log", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteAt(t.Context(), []byte{1}, math.MaxInt64); !errors.Is(err, platform.ErrInvalidRange) {
			t.Fatalf("WriteAt error = %v, want ErrInvalidRange", err)
		}
		if err := file.Truncate(t.Context(), math.MaxInt64); !errors.Is(err, platform.ErrInvalidRange) {
			t.Fatalf("Truncate error = %v, want ErrInvalidRange", err)
		}
		if n, err := file.WriteAt(t.Context(), nil, math.MaxInt64); err != nil || n != 0 {
			t.Fatalf("zero-length WriteAt = (%d, %v), want (0, nil)", n, err)
		}
	})
}

func TestDiskInjectsEveryOperationAndTracesTheFault(t *testing.T) {
	tests := []struct {
		operation sim.DiskOperation
		run       func(context.Context, *sim.Disk, platform.File) error
	}{
		{sim.DiskOpen, func(ctx context.Context, disk *sim.Disk, _ platform.File) error {
			_, err := disk.Open(ctx, "other", platform.OpenOptions{Create: true})
			return err
		}},
		{sim.DiskRead, func(ctx context.Context, _ *sim.Disk, file platform.File) error {
			_, err := file.ReadAt(ctx, make([]byte, 1), 0)
			return err
		}},
		{sim.DiskWrite, func(ctx context.Context, _ *sim.Disk, file platform.File) error {
			_, err := file.WriteAt(ctx, []byte("x"), 0)
			return err
		}},
		{sim.DiskTruncate, func(ctx context.Context, _ *sim.Disk, file platform.File) error {
			return file.Truncate(ctx, 0)
		}},
		{sim.DiskSync, func(ctx context.Context, _ *sim.Disk, file platform.File) error {
			return file.Sync(ctx)
		}},
		{sim.DiskSize, func(ctx context.Context, _ *sim.Disk, file platform.File) error {
			_, err := file.Size(ctx)
			return err
		}},
		{sim.DiskList, func(ctx context.Context, disk *sim.Disk, _ platform.File) error {
			_, err := disk.List(ctx, "f")
			return err
		}},
		{sim.DiskRemove, func(ctx context.Context, disk *sim.Disk, _ platform.File) error {
			return disk.Remove(ctx, "file")
		}},
		{sim.DiskRename, func(ctx context.Context, disk *sim.Disk, _ platform.File) error {
			return disk.Rename(ctx, "file", "renamed")
		}},
	}
	for _, test := range tests {
		t.Run(string(test.operation), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{})
				disk := runtime.NewDisk("node-1", sim.DiskConfig{})
				file, err := disk.Open(t.Context(), "file", platform.OpenOptions{Create: true})
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				runtime.Trace().Reset()
				disk.FailNext(test.operation, 1)
				if err := test.run(t.Context(), disk, file); !errors.Is(err, platform.ErrInjectedFault) {
					t.Fatalf("%s error = %v, want ErrInjectedFault", test.operation, err)
				}
				assertTraceEvent(t, runtime.Trace().Events(), "disk", string(test.operation), "injected_fault")
			})
		})
	}
}

func TestDiskListsFilesByPrefixInLexicographicOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := sim.New(sim.Config{}).NewDisk("node-1", sim.DiskConfig{})
		for _, name := range []string{"log-c", "other", "log-a"} {
			file, err := disk.Open(t.Context(), name, platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		names, err := disk.List(t.Context(), "log-")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(names, []string{"log-a", "log-c"}) {
			t.Fatalf("List = %v, want [log-a log-c]", names)
		}
	})
}

func TestDiskOperationCancelsWhileWaitingBehindQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{WriteLatency: time.Hour})
		file, err := disk.Open(t.Context(), "file", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		firstCtx, stopFirst := context.WithCancel(t.Context())
		defer stopFirst()
		entered := make(chan struct{})
		firstResult := make(chan error, 1)
		go func() {
			close(entered)
			_, writeErr := file.WriteAt(firstCtx, []byte("first"), 0)
			firstResult <- writeErr
		}()
		receiveWithin(t, entered)
		synctest.Wait()
		select {
		case err := <-firstResult:
			t.Fatalf("first queued write finished early: %v", err)
		default:
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if _, err := file.WriteAt(ctx, []byte("second"), 0); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued WriteAt error = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("queued cancellation elapsed = %v, want 1s virtual time", elapsed)
		}
		stopFirst()
		if err := receiveWithin(t, firstResult); !errors.Is(err, context.Canceled) {
			t.Fatalf("first WriteAt error = %v, want Canceled", err)
		}
	})
}

func receiveWithin[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	select {
	case value := <-channel:
		return value
	case <-ctx.Done():
		t.Fatalf("channel receive: %v", context.Cause(ctx))
		var zero T
		return zero
	}
}

func assertTraceEvent(t *testing.T, events []sim.Event, kind, operation, outcome string) {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind && event.Operation == operation && event.Outcome == outcome {
			return
		}
	}
	t.Fatalf("trace has no %s/%s/%s event: %+v", kind, operation, outcome, events)
}

// Two clients queue several operations on one disk. Admission, not Go channel
// waiter arrival, must determine which client's next operation owns the disk.
func TestScheduledDiskQueueIgnoresTaskCreationOrder(t *testing.T) {
	for seed := uint64(1); seed <= 8; seed++ {
		var recordings [2]sim.Recording
		for order := range 2 {
			synctest.Test(t, func(t *testing.T) {
				scheduler := sim.NewScheduler(seed)
				runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait})
				disk := runtime.NewDisk("shared", sim.DiskConfig{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					var wg sync.WaitGroup
					clients := []int{0, 1, 2}
					if order == 1 {
						slices.Reverse(clients)
					}
					for _, client := range clients {
						wg.Add(1)
						go func() {
							defer wg.Done()
							file, err := disk.Open(t.Context(), fmt.Sprintf("file-%d", client), platform.OpenOptions{Create: true})
							if err != nil {
								t.Error(err)
								return
							}
							defer file.Close()
							want := bytes.Repeat([]byte{byte(client + 1)}, 17+client)
							if _, err := file.WriteAt(t.Context(), want, 0); err != nil {
								t.Error(err)
								return
							}
							if err := file.Sync(t.Context()); err != nil {
								t.Error(err)
								return
							}
							got := make([]byte, len(want))
							if _, err := file.ReadAt(t.Context(), got, 0); err != nil {
								t.Error(err)
								return
							}
							if !bytes.Equal(got, want) {
								t.Error("queued disk operation changed another client's bytes")
							}
						}()
					}
					wg.Wait()
				}()
				if err := scheduler.Run(done); err != nil {
					t.Fatal(err)
				}
				var err error
				recordings[order], err = scheduler.Recording(runtime.Trace())
				if err != nil {
					t.Fatal(err)
				}
			})
		}
		if !bytes.Equal(recordings[0].Execution, recordings[1].Execution) || !bytes.Equal(recordings[0].Adapters, recordings[1].Adapters) {
			t.Fatalf("seed %d: disk queue depends on goroutine creation order", seed)
		}
	}
}
