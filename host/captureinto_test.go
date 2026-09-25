package host_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// capturedParent is a running VM whose guest stored into three pages of its
// memory that no checkpoint holds, registered with host 0.
func capturedParent(t *testing.T, h *hostHarness, pagers *hostPagers) (*volume.VM, *machine) {
	t.Helper()
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	return vm, guest
}

// TestCaptureIntoPublishesANewVMThatNeverBoots: a running VM is captured into
// a new VM. The new VM's root publishes the pages the pause sealed and the VMM
// state the pause saved, and no VMM ever runs it. The source keeps running,
// its seal ends with the capture, and it checkpoints as before. Any host can
// then open the new VM where the pause left the source.
func TestCaptureIntoPublishesANewVMThatNeverBoots(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.start(t)
	vm, guest := capturedParent(t, h, pagers[0])
	defer h.hosts[0].RemoveMachine("parent")

	captured, err := h.hosts[0].CaptureInto(t.Context(), "parent", "captured")
	if err != nil {
		t.Fatalf("capturing a running VM into a new one: %v", err)
	}
	if captured.VM != "captured" {
		t.Fatalf("the capture reports %v, want a checkpoint of the new VM", captured)
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the source is still sealed after the capture: %+v", status)
	}
	if running := h.hosts[0].Machines(); !slices.Equal(running, []string{"parent"}) {
		t.Fatalf("the host runs %v, want only the source", running)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("the host still holds %v after the capture", serving)
	}
	record, err := h.hosts[1].Control().Read(t.Context(), "captured")
	if err != nil {
		t.Fatal(err)
	}
	if record.Selected != captured.Sequence || !record.Created {
		t.Fatalf("the new VM's record = %+v, want checkpoint %d published", record, captured.Sequence)
	}
	parent, err := h.hosts[1].Control().Read(t.Context(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	if !parent.IsPinned(parent.Selected) {
		t.Fatalf("the source pins %v, not the checkpoint %d the new VM inherits", parent.Pinned, parent.Selected)
	}
	state, err := host.State(t.Context(), h.hosts[1].Checkpoints(), captured)
	if err != nil {
		t.Fatalf("reading the new VM's VMM state: %v", err)
	}
	if string(state) != "vmm-state" {
		t.Fatalf("the new VM's root holds the state %q, want the one the pause saved", state)
	}

	// The source keeps running and checkpoints again.
	guest.store("ram0", 0, 9)
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatalf("the source could not checkpoint after the capture: %v", err)
	}

	opened, err := h.hosts[1].Volumes().Open(t.Context(), "captured")
	if err != nil {
		t.Fatalf("another host opening the new VM: %v", err)
	}
	defer opened.Close(t.Context())
	memory := volumeBytes(t, opened, "ram0")
	for page := range 3 {
		want := bytes.Repeat([]byte{byte(page + 1)}, migrationPageSize)
		if got := memory[page*migrationPageSize : (page+1)*migrationPageSize]; !bytes.Equal(got, want) {
			t.Fatalf("the new VM reads page %d as %d..., want the source's %d at the pause", page, got[0], page+1)
		}
	}
}

// TestCaptureIntoAnExistingVMIsRefused: the new VM's identity must be free. A
// capture into one that exists takes nothing: the source is unsealed again and
// checkpoints as before, and the VM that was there is left as it was.
func TestCaptureIntoAnExistingVMIsRefused(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.start(t)
	vm, guest := capturedParent(t, h, pagers[0])
	defer h.hosts[0].RemoveMachine("parent")
	taken, err := h.hosts[1].Volumes().Create(t.Context(), "taken", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	before := taken.Status().Checkpoint
	if err := taken.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := h.hosts[0].CaptureInto(t.Context(), "parent", "taken"); !errors.Is(err, volume.ErrExists) {
		t.Fatalf("capturing into a VM that exists = %v, want ErrExists", err)
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("a refused capture left the source sealed: %+v", status)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatalf("the source could not checkpoint after a refused capture: %v", err)
	}
	record, err := h.hosts[1].Control().Read(t.Context(), "taken")
	if err != nil {
		t.Fatal(err)
	}
	if record.Selected != before.Sequence {
		t.Fatalf("a refused capture moved the existing VM to %d, want %d", record.Selected, before.Sequence)
	}
}

// TestCaptureIntoNeedsARunningSource: a capture pauses a guest, so a VM this
// host does not run is refused before anything is created.
func TestCaptureIntoNeedsARunningSource(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	if _, err := h.hosts[0].CaptureInto(t.Context(), "absent", "captured"); !errors.Is(err, host.ErrNotMigratable) {
		t.Fatalf("capturing a VM this host does not run = %v, want ErrNotMigratable", err)
	}
	if _, err := h.hosts[0].Control().Read(t.Context(), "captured"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("reading the new VM after a refused capture = %v, want ErrNotFound", err)
	}
}
