package volume_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/volume"
)

// coldSpecs is a VM shaped the way a guest is: a memory volume and a root
// volume, both of whole pages, so a cold boot has a memory to discard and a
// disk to keep.
var coldSpecs = []volume.VolumeSpec{
	{Name: "ram0", Size: 2 * checkpoint.PageSize},
	{Name: "root", Size: 2 * checkpoint.PageSize},
}

// coldVM creates a VM of coldSpecs with a page of each volume written and a
// checkpoint carrying VMM state published over it, which is what a running
// guest that has been stopped leaves behind.
func coldVM(t *testing.T, manager *volume.Manager, id string) (*volume.VM, model) {
	t.Helper()
	vm, err := manager.Create(t.Context(), id, coldSpecs)
	if err != nil {
		t.Fatal(err)
	}
	want := newModel(coldSpecs)
	for _, name := range []string{"ram0", "root"} {
		data := bytes.Repeat([]byte{byte(len(name))}, checkpoint.SectorSize)
		if err := vm.Volume(name).Write(t.Context(), 0, data); err != nil {
			t.Fatal(err)
		}
		copy(want[name], data)
	}
	ckpt, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("registers and devices"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	return vm, want
}

// A cold boot discards the guest's memory and the VMM state in one publication:
// the checkpoint it publishes names no page of the memory volume and no state,
// so the VM comes back with its memory reading as zeroes and nothing to restore,
// while its root volume is exactly what the last checkpoint published.
func TestDiscardMemoryPublishesNoMemoryPagesAndNoState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		store := h.imageStore(t, h.objects)
		config := h.config()
		config.Store = store
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, want := coldVM(t, manager, "vm")

		if err := vm.DiscardMemory(t.Context(), "ram0", nil); err != nil {
			t.Fatalf("DiscardMemory: %v", err)
		}
		clear(want["ram0"])
		want.check(t, vm, "after discarding the memory")

		selected := vm.Status().Checkpoint
		index, err := store.Open(t.Context(), selected)
		if err != nil {
			t.Fatal(err)
		}
		if index.HasState() {
			t.Fatalf("the checkpoint %s a cold boot published still names VMM state", selected)
		}
		if _, err := store.ReadState(t.Context(), index); !errors.Is(err, checkpoint.ErrNoState) {
			t.Fatalf("reading the state of %s = %v, want %v", selected, err, checkpoint.ErrNoState)
		}
		extents, err := index.Locate(t.Context(), "ram0", 0, 2*checkpoint.PageSize)
		if err != nil {
			t.Fatal(err)
		}
		zeroes := []control.Extent{{Offset: 0, Length: 2 * checkpoint.PageSize, Identity: control.ZeroIdentity}}
		if !slices.Equal(extents, zeroes) {
			t.Fatalf("the memory volume of %s locates as %+v, want one hole over the whole volume", selected, extents)
		}

		// The root volume is what the last checkpoint published, which is what
		// the guest's filesystem sees as a power cut after that checkpoint.
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		want.check(t, reopened, "after reopening the cold-booted VM")
	})
}

// The pages the discarded memory held are unreferenced by the new root, so the
// checkpoint that held them is reclaimed by the ordinary set difference and the
// checkpoint that dropped them is small.
func TestDiscardMemoryReclaimsTheMemorysParts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, objects := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, _ := coldVM(t, manager, "vm")
		held := vm.Status().Checkpoint
		if got := objectsUnder(t, h, objects, held); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the checkpoint holding the memory published %v", got)
		}

		if err := vm.DiscardMemory(t.Context(), "ram0", nil); err != nil {
			t.Fatalf("DiscardMemory: %v", err)
		}
		// The memory's pages and the VMM state leave the index together, so the
		// only thing the checkpoint that held them is still read for is the root
		// volume's page — which the cold boot carries into its own parts and
		// leaves that checkpoint holding nothing.
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("after the cold boot")); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, objects, held); len(got) != 0 {
			t.Fatalf("the checkpoint that held the discarded memory kept %v", got)
		}
	})
}

// A cold boot is the one moment a VM's shape can change: the memory volume takes
// whatever size the caller asks for, up or down, and the root volume may grow,
// in the same publication that discards the memory. The pages a grown volume
// gains read as zeroes.
func TestDiscardMemoryResizesInTheSamePublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := coldVM(t, manager, "vm")

		sizes := map[string]uint64{"ram0": 4 * checkpoint.PageSize, "root": 3 * checkpoint.PageSize}
		if err := vm.DiscardMemory(t.Context(), "ram0", sizes); err != nil {
			t.Fatalf("DiscardMemory: %v", err)
		}
		want["ram0"] = make([]byte, 4*checkpoint.PageSize)
		want["root"] = append(want["root"], make([]byte, checkpoint.PageSize)...)
		for _, v := range vm.Volumes() {
			if v.Size() != sizes[v.Name()] {
				t.Fatalf("%s is %d bytes after the cold boot, want %d", v.Name(), v.Size(), sizes[v.Name()])
			}
		}
		want.check(t, vm, "after growing memory and disk")

		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		want.check(t, reopened, "after reopening the resized VM")
	})
}

// The memory is discarded whatever size it takes, so it may shrink. Every other
// volume may only grow: the filesystem's end is not the volume's to cut.
func TestDiscardMemoryShrinksMemoryAndRefusesToShrinkTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := coldVM(t, manager, "vm")

		refused := map[string]uint64{"root": checkpoint.PageSize}
		if err := vm.DiscardMemory(t.Context(), "ram0", refused); !errors.Is(err, volume.ErrInvalidRange) {
			t.Fatalf("shrinking the root volume = %v, want %v", err, volume.ErrInvalidRange)
		}
		if got := vm.Volume("root").Size(); got != 2*checkpoint.PageSize {
			t.Fatalf("the refused shrink left the root volume at %d bytes", got)
		}
		want.check(t, vm, "after a refused shrink")

		if err := vm.DiscardMemory(t.Context(), "ram0", map[string]uint64{"ram0": checkpoint.PageSize}); err != nil {
			t.Fatalf("shrinking the memory: %v", err)
		}
		want["ram0"] = make([]byte, checkpoint.PageSize)
		if got := vm.Volume("ram0").Size(); got != checkpoint.PageSize {
			t.Fatalf("the memory volume is %d bytes after shrinking it", got)
		}
		want.check(t, vm, "after shrinking the memory")
	})
}

// A volume this VM does not have is not a memory to discard, and a size for one
// is not a resize: both are refused before anything is published.
func TestDiscardMemoryRefusesAnUnknownVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := coldVM(t, manager, "vm")
		before := vm.Status().Checkpoint

		if err := vm.DiscardMemory(t.Context(), "ram1", nil); !errors.Is(err, volume.ErrUnknownVolume) {
			t.Fatalf("discarding a volume that does not exist = %v, want %v", err, volume.ErrUnknownVolume)
		}
		if err := vm.DiscardMemory(t.Context(), "ram0",
			map[string]uint64{"scratch": checkpoint.PageSize}); !errors.Is(err, volume.ErrUnknownVolume) {
			t.Fatalf("resizing a volume that does not exist = %v, want %v", err, volume.ErrUnknownVolume)
		}
		if got := vm.Status().Checkpoint; got != before {
			t.Fatalf("a refused cold boot published %s", got)
		}
		want.check(t, vm, "after two refused cold boots")
	})
}
