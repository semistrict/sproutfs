//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// keptMarker lives on the guest's devtmpfs, which is guest memory and nothing
// else: a guest that boots does not have it, and one restored from a
// checkpoint's memory has whatever it held at that checkpoint's pause.
const keptMarker = "/dev/kept-marker"

// TestAVMCreatedFromAKeptCheckpointResumesItsGuest: a running guest is
// captured into a kept checkpoint and goes on running past it, through two more
// checkpoints that replace it. A VM created from the kept checkpoint on another
// host — its parent still running, and nothing of the kept checkpoint's memory
// resident there — resumes the guest from that checkpoint's VMM state with that
// checkpoint's memory: the marker it reads is the one written before the pause,
// not the one its parent wrote after it, and its agent answers over its own
// vsock.
func TestAVMCreatedFromAKeptCheckpointResumesItsGuest(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	cluster := newMigrationCluster(t, ctx)
	parent := newGuestVMIn(t, ctx, cluster, "kept-parent")
	running := bootGuestWithAgent(t, ctx, binaryPath, parent)
	writeMarker(t, ctx, running, "before")

	kept, err := parent.Snapshot(ctx, prepareAndResume(running), volume.Terms{Keep: true})
	if err != nil {
		t.Fatalf("capturing %s into a kept checkpoint: %v\n%s", parent.ID(), err, consoleText(running))
	}
	if err := kept.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, ctx, running, "after")
	for range 2 {
		later, err := parent.Snapshot(ctx, prepareAndResume(running), volume.Terms{})
		if err != nil {
			t.Fatal(err)
		}
		if err := later.Swept(ctx); err != nil {
			t.Fatal(err)
		}
	}

	point, err := cluster.destination.InheritPublished(ctx, "kept-child", kept.Ref())
	if err != nil {
		t.Fatalf("a point over a kept checkpoint of a running VM: %v", err)
	}
	if !point.HasState() {
		t.Fatal("the kept capture holds no VMM state")
	}
	child, err := cluster.destination.Fork(ctx, "kept-child", point)
	if err != nil {
		t.Fatal(err)
	}
	// The child's root names the kept checkpoint's state and memory, and the
	// guest is restored from the state that root names.
	if err := child.Checkpoint(ctx); err != nil {
		t.Fatalf("publishing the root of the VM created from the kept checkpoint: %v", err)
	}
	root, err := cluster.store.Open(ctx, child.Status().Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	state, err := cluster.store.ReadState(ctx, root)
	if err != nil {
		t.Fatalf("the root of the created VM names no VMM state: %v", err)
	}
	config := migrationConfig(t, binaryPath, newMigrationPager(t, ctx), child)
	config.Starter.(*vmmachine.Firecracker).VsockCID = guestVsockCID
	config.RestoreState = state
	resumed, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatalf("restoring the created VM from the kept checkpoint's state: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	if err := resumed.Release(ctx); err != nil {
		t.Fatalf("resuming the created VM: %v\n%s", err, consoleText(resumed))
	}

	if got := readMarker(t, ctx, resumed); got != "before\n" {
		t.Fatalf("the created VM's guest reads %q, want the kept checkpoint's %q\n%s",
			got, "before\n", consoleText(resumed))
	}
	if got := readMarker(t, ctx, running); got != "after\n" {
		t.Fatalf("the parent reads %q after the create, want its own %q", got, "after\n")
	}
}

// writeMarker writes one value into the guest's memory over its vsock.
func writeMarker(t *testing.T, ctx context.Context, p *vmmachine.Process, value string) {
	t.Helper()
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "echo " + value + " > " + keptMarker, Timeout: 30})
	if err != nil || result.Exit != 0 {
		t.Fatalf("writing the marker: %+v, %v\n%s", result, err, consoleText(p))
	}
}

// readMarker reads the marker back over the guest's own vsock.
func readMarker(t *testing.T, ctx context.Context, p *vmmachine.Process) string {
	t.Helper()
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "cat " + keptMarker, Timeout: 30})
	if err != nil || result.Exit != 0 {
		t.Fatalf("reading the marker: %+v, %v\n%s", result, err, consoleText(p))
	}
	return result.Stdout
}
