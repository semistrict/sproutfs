package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/volume"
)

// ColdShape is what a cold start does to one VM: the volume its guest's memory
// is in, which is discarded, the volume its filesystem is on, and the sizes the
// two take from here. A zero size keeps the size the VM has.
//
// The names are the caller's because this package does not decide which of a
// VM's volumes is its memory: a supervisor that boots Firecracker knows, and a
// test that models a guest says so.
type ColdShape struct {
	Memory, Root string
	// MemoryBytes is any size the host admits, up or down: the memory is being
	// discarded anyway. RootBytes may only grow — the end of a filesystem is not
	// the volume's to cut — and the pages it grows into read as zeroes, which is
	// what a filesystem grown in place expects.
	MemoryBytes, RootBytes uint64
	// VCPUs is how many processors the guest boots with from here, zero to keep
	// the count the VM has.
	VCPUs int
}

// changes reports a shape that changes anything about the VM, which only a
// cold boot may.
func (s ColdShape) changes() bool {
	return s.MemoryBytes != 0 || s.RootBytes != 0 || s.VCPUs != 0
}

// sizes is the shape as the volume manager takes it: the volumes whose size is
// being set, by name. A shape that names no size resizes nothing.
func (s ColdShape) sizes() (map[string]uint64, error) {
	if s.Memory == "" {
		return nil, fmt.Errorf("%w: a cold start needs the name of the volume the memory is in",
			ErrInvalidConfig)
	}
	if s.RootBytes != 0 && s.Root == "" {
		return nil, fmt.Errorf("%w: a cold start that grows the disk needs the name of the volume it is on",
			ErrInvalidConfig)
	}
	sizes := make(map[string]uint64, 2)
	if s.MemoryBytes != 0 {
		sizes[s.Memory] = s.MemoryBytes
	}
	if s.RootBytes != 0 {
		sizes[s.Root] = s.RootBytes
	}
	return sizes, nil
}

// OpenCold opens a VM nothing runs and discards its memory, which is what a
// cold start is: the checkpoint it publishes names no page of the memory volume
// and no VMM state, so the guest started over it has nothing to restore and
// boots its kernel from the volumes that are left. Those are exactly what the
// last checkpoint published, so the guest's filesystem sees the boot as a power
// cut after that checkpoint and its journal recovers what a journal recovers.
//
// The VM's shape may change here and nowhere else, because this is the one
// moment nothing in memory describes it. A shape this host could not map is
// refused before anything is discarded, as is a disk that would shrink; either
// way the VM is left at the checkpoint it was opened on, and the handle goes
// with the refusal.
//
// The caller boots the returned handle. A VM whose boot then fails is an
// ordinary failed open — its memory is gone, which is what was asked for.
func (h *Host) OpenCold(ctx context.Context, vmID string, shape ColdShape) (*volume.VM, error) {
	if _, err := shape.sizes(); err != nil {
		return nil, err
	}
	vm, err := h.volumes.Open(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if err := h.Reshape(ctx, vm, shape); err != nil {
		return nil, errors.Join(fmt.Errorf("cold starting %s", vmID), err, closing(ctx, vm))
	}
	slog.InfoContext(ctx, "host: a VM was cold started", "vm", vmID,
		"checkpoint", vm.Status().Checkpoint.Sequence, "memory", shape.MemoryBytes, "disk", shape.RootBytes,
		"vcpus", vm.VCPUs())
	return vm, nil
}

// Reshape discards the memory of a VM nothing runs and publishes it at the
// shape given, in one checkpoint. It is the cold start's publication, and a
// create's first one: a VM created from a template takes its own RAM, disk and
// processor count there, before its first boot.
//
// A VM the pager could not map every memory region of is one that would be
// discovered at the attachment of whichever memory region ran into the cap,
// with its memory already gone. The question is asked at the shape it is being
// given, before anything is published. The handle stays the caller's either way.
func (h *Host) Reshape(ctx context.Context, vm *volume.VM, shape ColdShape) error {
	sizes, err := shape.sizes()
	if err != nil {
		return err
	}
	if err := h.AdmitMemoryRegions(coldMemoryRegions(vm, sizes)); err != nil {
		return err
	}
	if err := vm.DiscardMemory(ctx, shape.Memory, volume.Shape{Sizes: sizes, VCPUs: shape.VCPUs}); err != nil {
		return fmt.Errorf("discarding the memory of %s: %w", vm.ID(), err)
	}
	return nil
}

// coldMemoryRegions is the size of every memory region this VM would have at the shape it is
// being given: the new size where one is named and the size it has otherwise.
func coldMemoryRegions(vm *volume.VM, sizes map[string]uint64) []MemoryRegion {
	memoryRegions := make([]MemoryRegion, 0, len(vm.Volumes()))
	for _, v := range vm.Volumes() {
		size := v.Size()
		if next, found := sizes[v.Name()]; found {
			size = next
		}
		memoryRegions = append(memoryRegions, memoryRegionOf(v.Name(), size))
	}
	return memoryRegions
}

// Starting is the VMM state a VM opened on this host starts from: the state its
// selected checkpoint carries, which its guest is restored from, or none, which
// its guest is cold booted with. A checkpoint published without state — the
// interval's checkpoint of the disks, a template's, a VM that never ran — has
// memory no registers describe, so starting over it is a power cut at that
// checkpoint: the memory is discarded here, in a checkpoint of its own, and the
// guest boots its kernel over the disks and recovers what its journal recovers.
//
// memory names the volume the guest's memory is in. A VM the state cannot be
// read for is left open; closing it is the caller's.
func (h *Host) Starting(ctx context.Context, vm *volume.VM, memory string) ([]byte, error) {
	state, err := State(ctx, h.Checkpoints(), vm.Status().Checkpoint)
	if err == nil {
		return state, nil
	}
	if !errors.Is(err, checkpoint.ErrNoState) {
		return nil, fmt.Errorf("reading the VMM state of %s: %w", vm.ID(), err)
	}
	opened := vm.Status().Checkpoint.Sequence
	if err := vm.DiscardMemory(ctx, memory, volume.Shape{}); err != nil {
		return nil, fmt.Errorf("discarding the memory of %s: %w", vm.ID(), err)
	}
	slog.InfoContext(ctx, "host: a VM with no VMM state is cold booted", "vm", vm.ID(),
		"opened", opened, "checkpoint", vm.Status().Checkpoint.Sequence)
	return nil, nil
}

// CreateRoot publishes the first checkpoint of a VM just forked from a
// published checkpoint (volume.Manager.InheritPublished, or a template's
// point), and reports the VMM state its guest starts from.
//
// A point with VMM state, and a shape that changes nothing, resumes: the root
// names the state and the memory the point's checkpoint holds, and the guest
// is restored from them where that checkpoint's pause left it, as a fork of a
// running VM is. Anything else boots cold: a point without state has memory no
// registers describe, and a shape is a cold boot's alone, because nothing in
// memory must describe the VM's shape when it changes. The root is then the
// cold boot's publication at that shape (Reshape), and the state is nil.
//
// The handle stays the caller's either way.
func (h *Host) CreateRoot(ctx context.Context, vm *volume.VM, point *volume.ForkPoint, shape ColdShape) ([]byte, error) {
	if !point.HasState() || shape.changes() {
		return nil, h.Reshape(ctx, vm, shape)
	}
	if err := vm.Checkpoint(ctx); err != nil {
		return nil, fmt.Errorf("publishing the root of %s: %w", vm.ID(), err)
	}
	state, err := State(ctx, h.Checkpoints(), vm.Status().Checkpoint)
	if err != nil {
		return nil, fmt.Errorf("reading the VMM state of %s: %w", vm.ID(), err)
	}
	return state, nil
}
