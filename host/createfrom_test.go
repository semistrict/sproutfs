package host_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// createFrom is the create a supervisor runs for a request that names another
// VM's checkpoint: the point over that checkpoint, pinned without the parent's
// writer, the fork, and the root published at the shape asked for.
func createFrom(t *testing.T, h *host.Host, id string, parent control.Ref, shape host.ColdShape) (*volume.VM, error) {
	t.Helper()
	point, err := h.Volumes().InheritPublished(t.Context(), "child", parent)
	if err != nil {
		return nil, err
	}
	vm, _, err := host.CreateFork(t.Context(), h.Volumes(), id, point)
	if err != nil {
		return nil, err
	}
	if err := h.Reshape(t.Context(), vm, shape); err != nil {
		return nil, errors.Join(err, vm.Close(t.Context()))
	}
	return vm, nil
}

// TestACreateStartsFromAStoppedVMsCheckpoint: a VM no host runs is created
// from on another host. Its stop's checkpoint is pinned in its record without
// its epoch, and the new VM publishes its own root at the shape its create
// asks for, with the stopped VM's disk and no memory: it boots cold.
func TestACreateStartsFromAStoppedVMsCheckpoint(t *testing.T) {
	h := newHostHarness(t)
	stoppedVM(t, h, "vm-1")
	before, err := h.hosts[1].Control().Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}

	shape := host.ColdShape{Memory: "ram0", Root: "root",
		MemoryBytes: 2 * migrationPageSize, RootBytes: 3 * migrationPageSize, VCPUs: 2}
	vm, err := createFrom(t, h.hosts[1], "vm-2", control.Ref{VM: "vm-1"}, shape)
	if err != nil {
		t.Fatalf("creating a VM from a stopped VM's checkpoint: %v", err)
	}
	if err := vm.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := h.hosts[1].Control().Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Epoch != before.Epoch || !slices.Equal(after.Pinned, []uint64{before.Selected}) {
		t.Fatalf("the stopped VM is at epoch %d pinning %v, want epoch %d pinning %d",
			after.Epoch, after.Pinned, before.Epoch, before.Selected)
	}

	opened, err := h.hosts[0].Volumes().Open(t.Context(), "vm-2")
	if err != nil {
		t.Fatalf("another host opening the created VM: %v", err)
	}
	defer opened.Close(t.Context())
	if _, err := host.State(t.Context(), h.hosts[0].Checkpoints(), opened.Status().Checkpoint); !errors.Is(err, checkpoint.ErrNoState) {
		t.Fatalf("the created VM's root has state to restore: %v", err)
	}
	if memory := volumeBytes(t, opened, "ram0"); !bytes.Equal(memory, make([]byte, 2*migrationPageSize)) {
		t.Fatalf("the created VM's memory holds %d..., want zeroes", memory[migrationPageSize])
	}
	disk := volumeBytes(t, opened, "root")
	if want := bytes.Repeat([]byte{7}, migrationPageSize); !bytes.Equal(disk[:migrationPageSize], want) {
		t.Fatalf("the created VM's disk holds %d..., want the stopped VM's", disk[0])
	}
	if len(disk) != 3*migrationPageSize {
		t.Fatalf("the created VM's disk is %d bytes, want %d", len(disk), 3*migrationPageSize)
	}
	if got := opened.VCPUs(); got != 2 {
		t.Fatalf("the created VM boots with %d processors, want 2", got)
	}
}

// TestACreateFromACheckpointRefusesWhatItCannotStartFrom: a create names an
// identity nobody has, and a checkpoint that is published and pinnable. A VM
// that exists is refused, and so is a checkpoint its record no longer selects
// and no pin keeps, which its writer may be reclaiming.
func TestACreateFromACheckpointRefusesWhatItCannotStartFrom(t *testing.T) {
	h := newHostHarness(t)
	stoppedVM(t, h, "vm-1")
	stopped, err := h.hosts[1].Control().Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := createFrom(t, h.hosts[1], "vm-2", control.Ref{VM: "vm-1"}, coldShape)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := createFrom(t, h.hosts[1], "vm-2", control.Ref{VM: "vm-1"}, coldShape); !errors.Is(err, volume.ErrExists) {
		t.Fatalf("creating a VM that exists = %v, want ErrExists", err)
	}

	// vm-2 publishes a checkpoint past its root, so its root is neither
	// selected nor pinned.
	opened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-2")
	if err != nil {
		t.Fatal(err)
	}
	root := opened.Status().Checkpoint
	if err := opened.Volume("root").Write(t.Context(), 0, bytes.Repeat([]byte{3}, checkpoint.SectorSize)); err != nil {
		t.Fatal(err)
	}
	if err := opened.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := createFrom(t, h.hosts[1], "vm-3", root, coldShape); !errors.Is(err, control.ErrNotPublished) {
		t.Fatalf("creating from a checkpoint nothing selects or pins = %v, want ErrNotPublished", err)
	}
	// The stopped VM's own checkpoint is pinned now, so it may be named.
	third, err := createFrom(t, h.hosts[1], "vm-3", control.Ref{VM: "vm-1", Sequence: stopped.Selected}, coldShape)
	if err != nil {
		t.Fatalf("creating from a pinned checkpoint by its sequence: %v", err)
	}
	if err := third.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
