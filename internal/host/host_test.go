package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// TestHostOutageStartupAndTakeoverAfterWriterShutdown is the host assembly end
// to end: a host that cannot reach object storage creates nothing, one that can
// creates a VM, snapshots it, forks the checkpoint, and a second host takes the
// VM over by advancing its control record's epoch, which fences the first.
func TestHostOutageStartupAndTakeoverAfterWriterShutdown(t *testing.T) {
	h := newHostHarness(t)
	h.runtime.ObjectStore().Fail()
	h.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := h.hosts[1].Volumes()
	if _, err := m.Create(ctx, "unavailable", rootVolume); !errors.Is(err, platform.ErrUnavailable) {
		t.Fatalf("creation ignored unavailable metadata authority: %v", err)
	}
	h.runtime.ObjectStore().Recover()
	source, err := m.Create(ctx, "source", rootVolume)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, source, 0, "captured")
	if err := source.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	point, err := source.ForkPoint(ctx, volume.Prepared([]byte("vmm state"), nil))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := m.Fork(ctx, "fork", point)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, fork, 0, "fork-end")
	// The fork's first checkpoint is its own root index, which is what makes it
	// openable anywhere else.
	if err := fork.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	taken, err := h.hosts[0].Volumes().Open(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	// The takeover advanced the epoch. The old handle can still serve reads from
	// what it holds, but its next checkpoint has nowhere to go.
	if got := read(t, ctx, source, 0, len("captured")); got != "captured" {
		t.Fatalf("fenced handle lost its own view: %q", got)
	}
	write(t, ctx, source, 0, "stale")
	if err := source.Checkpoint(ctx); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("old writer retained authority: %v", err)
	}
	if status := source.Status(); !errors.Is(status.Err, volume.ErrNeedsRecovery) {
		t.Fatalf("the fenced handle's status = %+v, want a handle that must be reopened", status)
	}
	// Closing a fenced handle releases it; its final checkpoint has nowhere to
	// go and is reported rather than returned.
	if err := source.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, taken, 0, len("captured")); got != "captured" {
		t.Fatalf("takeover opened the wrong checkpoint: %q", got)
	}
	if err := taken.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	epoch := fork.Epoch()
	// Leave the fork handle owned by the host: shutting the host down closes it,
	// which publishes its final checkpoint.
	if err := h.hosts[1].Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fork.Volume("root").Write(ctx, 0, []byte("late")); !errors.Is(err, volume.ErrClosed) {
		t.Fatalf("shutdown left writable handle: %v", err)
	}
	if _, err := m.Open(ctx, "fork"); !errors.Is(err, volume.ErrClosed) {
		t.Fatalf("closed host opened a VM: %v", err)
	}
	status := h.hosts[1].Status()
	if !status.Closed || status.Cache.ResidentBytes != 0 {
		t.Fatalf("host retained owned resources: %+v", status)
	}
	assertPageServerReleased(t, h.configs[1].Migration.Address)
	// A new process takes the fork over. It owns no local state: everything it
	// needs is the control record and the checkpoint that record selects.
	restarted, err := host.StartHost(t.Context(), h.configs[1])
	if err != nil {
		t.Fatal(err)
	}
	h.hosts[1] = restarted
	opened, err := restarted.Volumes().Open(ctx, "fork")
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.Epoch(); got != epoch+1 {
		t.Fatalf("epoch after the takeover = %d, want %d", got, epoch+1)
	}
	if got := read(t, ctx, opened, 0, len("fork-end")); got != "fork-end" {
		t.Fatalf("fork data lost: %q", got)
	}
	write(t, ctx, opened, 16, "restarted")
	if _, err := restarted.Volumes().Open(ctx, "source"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("deletion lost: %v", err)
	}
}

// TestHostCloseIsWhatMakesTheLastWritesDurable is the loss model: nothing is
// durable between checkpoints, so a host that closes a VM publishes everything
// it held and a later open reads it back.
func TestHostCloseIsWhatMakesTheLastWritesDurable(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	vm, err := h.hosts[0].Volumes().Create(ctx, "closing", rootVolume)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, vm, 0, "written but not published")
	if got := h.hosts[0].Status().Volumes.DirtyBytes; got != 4096 {
		t.Fatalf("dirty bytes before the checkpoint = %d, want 4096", got)
	}
	if err := vm.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := h.hosts[1].Volumes().Open(ctx, "closing")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, again, 0, len("written but not published")); got != "written but not published" {
		t.Fatalf("close did not publish the handle's writes: %q", got)
	}
	if err := again.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHostStartupRejectsAnIncompleteConfigurationAndHonorsCancellation(t *testing.T) {
	h := newHostHarness(t)
	config := h.configs[0]
	config.ObjectStore = nil
	if started, err := host.StartHost(t.Context(), config); !errors.Is(err, host.ErrInvalidConfig) || started != nil {
		t.Fatalf("host started without an object store: %v", err)
	}
	assertPageServerReleased(t, config.Migration.Address)
	ctx, cancel := context.WithCancel(t.Context())
	started, err := host.StartHost(ctx, h.configs[0])
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.hosts = append(h.hosts, started)
	cancel()
	wait, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := started.Close(wait); err != nil {
		t.Fatal(err)
	}
	if status := started.Status(); !status.Closed {
		t.Fatalf("parent cancellation left the host running: %+v", status)
	}
	assertPageServerReleased(t, h.configs[0].Migration.Address)
}
