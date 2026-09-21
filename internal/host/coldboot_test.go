package host_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/volume"
)

// coldVolumes is a VM shaped the way a guest is: memory, and a root volume its
// filesystem lives on. A cold boot discards the first and keeps the second.
var coldVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: 4 * migrationPageSize, PageSize: migrationPageSize},
	{Name: "root", Size: 2 * migrationPageSize, PageSize: migrationPageSize},
}

// coldShape names the two volumes a cold boot acts on, with no resize.
var coldShape = host.ColdShape{Memory: "ram0", Root: "root"}

// stoppedVM leaves one VM behind exactly as a stop does: its guest wrote into
// memory and onto its disk, a checkpoint published both with the VMM state that
// was running over them, and then the process, the pages and the handle went.
func stoppedVM(t *testing.T, h *hostHarness, id string) {
	t.Helper()
	pagers := newPager(t, h.configs[0].Resources)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), id, coldVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 1, 9)
	guest.store("root", 0, 7)
	if err := h.hosts[0].AddMachine(id, guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Stop(t.Context(), id); err != nil {
		t.Fatalf("stopping %s: %v", id, err)
	}
}

// requireCold reports the bytes one volume of a VM holds, and fails when the
// read does.
func volumeBytes(t *testing.T, vm *volume.VM, name string) []byte {
	t.Helper()
	held := vm.Volume(name)
	if held == nil {
		t.Fatalf("%s has no volume %s", vm.ID(), name)
	}
	data := make([]byte, held.Size())
	if err := held.Read(t.Context(), 0, data); err != nil {
		t.Fatalf("reading %s of %s: %v", name, vm.ID(), err)
	}
	return data
}

// TestColdStartDiscardsMemoryAndKeepsTheDisk: a cold start of a stopped VM
// publishes a checkpoint that holds neither the guest's memory nor the VMM state
// that was running over it, so the guest that opens it has nothing to restore
// and boots its kernel instead. Its root volume is exactly what the last
// checkpoint published, which is what its filesystem sees as a power cut after
// that checkpoint.
func TestColdStartDiscardsMemoryAndKeepsTheDisk(t *testing.T) {
	h := newHostHarness(t)
	stoppedVM(t, h, "vm-1")

	vm, err := h.hosts[1].OpenCold(t.Context(), "vm-1", coldShape)
	if err != nil {
		t.Fatalf("cold starting a stopped VM: %v", err)
	}
	defer vm.Close(t.Context())

	selected := vm.Status().Checkpoint
	if _, err := host.State(t.Context(), h.hosts[1].Checkpoints(), selected); !errors.Is(err, checkpoint.ErrNoState) {
		t.Fatalf("the checkpoint %s a cold start published has state to restore: %v", selected, err)
	}
	if memory := volumeBytes(t, vm, "ram0"); !bytes.Equal(memory, make([]byte, 4*migrationPageSize)) {
		t.Fatalf("a cold-started VM's memory holds %d..., want zeroes", memory[migrationPageSize])
	}
	disk := volumeBytes(t, vm, "root")
	if want := bytes.Repeat([]byte{7}, migrationPageSize); !bytes.Equal(disk[:migrationPageSize], want) {
		t.Fatalf("a cold-started VM's disk holds %d..., want the byte its guest wrote", disk[0])
	}
}

// TestColdStartResizesMemoryAndGrowsTheDisk: a cold boot is the one moment a
// VM's shape can change, because nothing in memory describes it any more. The
// memory takes whatever size is asked for and the disk grows into pages that
// read as zeroes, which is what a filesystem grown in place expects.
func TestColdStartResizesMemoryAndGrowsTheDisk(t *testing.T) {
	h := newHostHarness(t)
	stoppedVM(t, h, "vm-1")

	shape := host.ColdShape{Memory: "ram0", Root: "root",
		MemoryBytes: 2 * migrationPageSize, RootBytes: 3 * migrationPageSize}
	vm, err := h.hosts[1].OpenCold(t.Context(), "vm-1", shape)
	if err != nil {
		t.Fatalf("cold starting with a new shape: %v", err)
	}
	defer vm.Close(t.Context())

	if got := vm.Volume("ram0").Size(); got != 2*migrationPageSize {
		t.Fatalf("the memory is %d bytes after a cold start that asked for %d", got, 2*migrationPageSize)
	}
	if got := vm.Volume("root").Size(); got != 3*migrationPageSize {
		t.Fatalf("the disk is %d bytes after a cold start that asked for %d", got, 3*migrationPageSize)
	}
	disk := volumeBytes(t, vm, "root")
	if want := bytes.Repeat([]byte{7}, migrationPageSize); !bytes.Equal(disk[:migrationPageSize], want) {
		t.Fatalf("the grown disk lost the bytes its guest wrote: %d...", disk[0])
	}
	if grown := disk[2*migrationPageSize:]; !bytes.Equal(grown, make([]byte, migrationPageSize)) {
		t.Fatalf("the pages a grown disk gained hold %d..., want zeroes", grown[0])
	}
}

// TestColdStartRefusesToShrinkTheDisk: the end of a filesystem is not the
// volume's to cut, so a disk may only grow; the VM is left exactly as it was.
func TestColdStartRefusesToShrinkTheDisk(t *testing.T) {
	h := newHostHarness(t)
	stoppedVM(t, h, "vm-1")

	shape := host.ColdShape{Memory: "ram0", Root: "root", RootBytes: migrationPageSize}
	vm, err := h.hosts[1].OpenCold(t.Context(), "vm-1", shape)
	if !errors.Is(err, volume.ErrInvalidRange) {
		if vm != nil {
			vm.Close(t.Context())
		}
		t.Fatalf("shrinking the disk at a cold start = %v, want %v", err, volume.ErrInvalidRange)
	}
	// The refusal released the handle, so the VM is still there to be opened
	// warm at exactly the checkpoint the stop published.
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatalf("opening the VM a refused cold start left behind: %v", err)
	}
	defer reopened.Close(t.Context())
	if got := reopened.Volume("root").Size(); got != 2*migrationPageSize {
		t.Fatalf("a refused cold start left the disk at %d bytes", got)
	}
	memory := volumeBytes(t, reopened, "ram0")
	if want := bytes.Repeat([]byte{9}, migrationPageSize); !bytes.Equal(memory[migrationPageSize:2*migrationPageSize], want) {
		t.Fatalf("a refused cold start discarded the memory anyway: %d...", memory[migrationPageSize])
	}
	if _, err := host.State(t.Context(), h.hosts[1].Checkpoints(), reopened.Status().Checkpoint); err != nil {
		t.Fatalf("a refused cold start left the VM with no state to restore: %v", err)
	}
}
