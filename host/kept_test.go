package host_test

import (
	"bytes"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// keptRunning runs one VM on host 0 whose guest wrote a byte into its memory
// and one onto its disk, keeps the checkpoint capture takes of that, and then
// moves on: the guest writes other bytes and two more checkpoints publish
// them. The VM is still running when it returns, with the kept checkpoint.
func keptRunning(t *testing.T, h *hostHarness, capture func(*volume.VM, *machine) (*volume.Checkpoint, error)) control.Ref {
	t.Helper()
	pagers := newPager(t, h.configs[0].Resources)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", coldVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 1, 9)
	guest.store("root", 0, 7)
	ckpt, err := capture(vm, guest)
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, value := range []byte{11, 12} {
		guest.store("ram0", 1, value)
		guest.store("root", 0, value)
		if err := guest.checkpoint(t.Context(), vm); err != nil {
			t.Fatal(err)
		}
	}
	return ckpt.Ref()
}

// createFromKept is a create's own steps over a kept checkpoint, on host 1: the
// point pinned without the parent's writer, the fork, and the root, which
// reports the state the guest starts from. The caller closes the VM.
func createFromKept(t *testing.T, h *hostHarness, kept control.Ref, shape host.ColdShape) (*volume.VM, []byte) {
	t.Helper()
	point, err := h.hosts[1].Volumes().InheritPublished(t.Context(), "vm-2", kept)
	if err != nil {
		t.Fatalf("a point over a kept checkpoint of a running VM: %v", err)
	}
	vm, _, err := host.CreateFork(t.Context(), h.hosts[1].Volumes(), "vm-2", point)
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.hosts[1].CreateRoot(t.Context(), vm, point, shape)
	if err != nil {
		t.Fatalf("publishing the root of a VM created from a kept checkpoint: %v", err)
	}
	return vm, state
}

// TestACreateFromAKeptCheckpointResumesItsGuest: a VM created from a kept
// checkpoint that holds VMM state resumes from that state, with the memory and
// the disk that checkpoint's pause left, not the bytes its VM wrote since. Its
// root is published, so any host opens it there.
func TestACreateFromAKeptCheckpointResumesItsGuest(t *testing.T) {
	h := newHostHarness(t)
	kept := keptRunning(t, h, func(vm *volume.VM, guest *machine) (*volume.Checkpoint, error) {
		return host.Capture(t.Context(), vm, guest, nil, volume.Terms{Keep: true})
	})
	vm, state := createFromKept(t, h, kept, coldShape)
	if string(state) != "vmm-state" {
		t.Fatalf("the created VM starts from state %q, want the kept checkpoint's", state)
	}
	if memory := volumeBytes(t, vm, "ram0"); memory[migrationPageSize] != 9 {
		t.Fatalf("the created VM's memory holds %d..., want the kept checkpoint's 9", memory[migrationPageSize])
	}
	if disk := volumeBytes(t, vm, "root"); disk[0] != 7 {
		t.Fatalf("the created VM's disk holds %d..., want the kept checkpoint's 7", disk[0])
	}
	if vm.Status().Root {
		t.Fatal("the created VM has not published its own root")
	}
	root := vm.Status().Checkpoint
	if err := vm.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opened, err := h.hosts[0].Volumes().Open(t.Context(), "vm-2")
	if err != nil {
		t.Fatalf("another host opening the created VM: %v", err)
	}
	defer opened.Close(t.Context())
	if state, err := host.State(t.Context(), h.hosts[0].Checkpoints(), root); string(state) != "vmm-state" {
		t.Fatalf("the created VM's root names state %q (%v), want the kept checkpoint's", state, err)
	}
	// Any host lists what the VM keeps, and the checkpoint a VM was created
	// from is reported forked.
	listed, err := h.hosts[1].Kept(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Kept) != 1 || listed.Kept[0].Checkpoint != kept.Sequence || !listed.Kept[0].State ||
		!listed.Kept[0].Forked || listed.Kept[0].Time.IsZero() {
		t.Fatalf("host-1 lists %+v, want checkpoint %d kept with state, dated and forked", listed, kept.Sequence)
	}
}

// TestACreateFromAKeptCheckpointWithoutStateBootsCold: a kept checkpoint of the
// disks alone has memory no registers describe, so a VM created from it boots
// cold: its disk is the kept checkpoint's and its memory is zeroes. A shape
// asks for the same cold boot of a checkpoint that does hold state, and so does
// a device the state does not describe, such as an ephemeral disk the create
// adds.
func TestACreateFromAKeptCheckpointWithoutStateBootsCold(t *testing.T) {
	for _, test := range []struct {
		name    string
		capture func(*volume.VM, *machine) (*volume.Checkpoint, error)
		shape   host.ColdShape
	}{
		{name: "disks", shape: coldShape,
			capture: func(vm *volume.VM, guest *machine) (*volume.Checkpoint, error) {
				return host.CaptureDisks(t.Context(), vm, guest, nil, volume.Terms{Keep: true})
			}},
		{name: "shape", shape: host.ColdShape{Memory: "ram0", Root: "root", VCPUs: 2},
			capture: func(vm *volume.VM, guest *machine) (*volume.Checkpoint, error) {
				return host.Capture(t.Context(), vm, guest, nil, volume.Terms{Keep: true})
			}},
		{name: "device", shape: host.ColdShape{Memory: "ram0", Root: "root", Devices: true},
			capture: func(vm *volume.VM, guest *machine) (*volume.Checkpoint, error) {
				return host.Capture(t.Context(), vm, guest, nil, volume.Terms{Keep: true})
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHostHarness(t)
			kept := keptRunning(t, h, test.capture)
			vm, state := createFromKept(t, h, kept, test.shape)
			defer vm.Close(t.Context())
			if state != nil {
				t.Fatalf("the created VM starts from %d bytes of state, want a cold boot", len(state))
			}
			if memory := volumeBytes(t, vm, "ram0"); !bytes.Equal(memory, make([]byte, len(memory))) {
				t.Fatalf("a cold boot kept memory: %d...", memory[migrationPageSize])
			}
			if disk := volumeBytes(t, vm, "root"); disk[0] != 7 {
				t.Fatalf("the created VM's disk holds %d..., want the kept checkpoint's 7", disk[0])
			}
		})
	}
}
