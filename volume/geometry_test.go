package volume_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/volume"
)

// mixedSpecs is a VM whose memory is published in 4 KiB pages and whose disk is
// published in 2 MiB pages, which is what the storage layer must carry through
// one checkpoint, one fork and one sweep.
var mixedSpecs = []volume.VolumeSpec{
	{Name: "ram0", Size: 2 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize4KiB},
	{Name: "disk", Size: 2 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
}

// A page size is the host's choice and it is durable, so one this build cannot
// divide by is refused before the VM's first object is written.
func TestCreateRefusesAPageSizeNoVolumeMayHave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		for _, pageSize := range []uint64{0, 1, 512, 8 << 10, 1 << 20, 4 << 20} {
			specs := []volume.VolumeSpec{{Name: "ram0", Size: 2 * checkpoint.PageSize2MiB, PageSize: pageSize}}
			_, err := manager.Create(t.Context(), "refused", specs)
			if !errors.Is(err, volume.ErrInvalidConfig) {
				t.Fatalf("creating a VM whose page is %d bytes: %v", pageSize, err)
			}
		}
		// Nothing was created, so the identity is still free.
		vm, err := manager.Create(t.Context(), "refused", mixedSpecs)
		if err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A create of an identity the deployment already records opens it, and the
// volumes it opens must be the ones the caller asked for: a page size is what
// every page number of a volume is in, so a caller that asks for another one
// has changed its mind about what the VM is and is refused rather than handed a
// handle that numbers pages differently from the checkpoint it reads.
func TestCreatingOverAVMOfAnotherGeometryIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "geometry", mixedSpecs)
		if err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		moved := []volume.VolumeSpec{
			{Name: "ram0", Size: 2 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
			{Name: "disk", Size: 2 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
		}
		_, err = manager.Create(t.Context(), "geometry", moved)
		if !errors.Is(err, volume.ErrInvalidConfig) {
			t.Fatalf("creating over a VM whose memory is 4 KiB pages: %v", err)
		}
		if got := err.Error(); !strings.Contains(got, "ram0 of geometry is published in 4096-byte pages, not 2097152") {
			t.Fatalf("the refusal reads %q", got)
		}
		// The same geometry opens it, which is how a create interrupted after
		// its record is finished by repeating it.
		again, err := manager.Create(t.Context(), "geometry", mixedSpecs)
		if err != nil {
			t.Fatal(err)
		}
		if got := again.Volume("ram0").PageSize(); got != checkpoint.PageSize4KiB {
			t.Fatalf("the reopened memory is %d-byte pages, want %d", got, checkpoint.PageSize4KiB)
		}
		if err := again.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// One VM's volumes may be published in different page sizes, and everything
// this package does is in each volume's own unit: what a checkpoint
// republishes, what Locate names, what a fork inherits, and what a sweep leaves
// behind. The 4 KiB volume is written one page at a time over several
// checkpoints, which is what makes the checkpoints before mostly dead and puts
// compaction and reclamation through the small geometry too.
func TestAMixedGeometryVMCheckpointsForksAndReclaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "mixed", mixedSpecs)
		if err != nil {
			t.Fatal(err)
		}
		want := newModel(mixedSpecs)
		write := func(vm *volume.VM, m model, name string, offset uint64, value byte, length int) {
			t.Helper()
			data := bytes.Repeat([]byte{value}, length)
			if err := vm.Volume(name).Write(t.Context(), offset, data); err != nil {
				t.Fatal(err)
			}
			copy(m[name][offset:], data)
		}
		// Ten rounds, each rewriting one 4 KiB page of the memory and a slice of
		// the disk, so the checkpoints before them stop being read.
		for round := range uint64(10) {
			write(vm, want, "ram0", round*checkpoint.PageSize4KiB, byte(0x40+round), checkpoint.PageSize4KiB)
			write(vm, want, "disk", round*checkpoint.SectorSize, byte(0x80+round), checkpoint.SectorSize)
			if err := vm.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
			want.check(t, vm, fmt.Sprintf("round %d", round))
		}
		// A store into one 4 KiB page names that page and leaves the rest of its
		// 2 MiB alone, while the disk is named one 2 MiB page at a time.
		write(vm, want, "ram0", 3*checkpoint.PageSize4KiB, 0xee, checkpoint.PageSize4KiB)
		next := control.Ref{VM: "mixed", Sequence: counted(vm, 12)}
		// The ten pages the rounds wrote are ten 4 KiB extents, each named by the
		// checkpoint of its own round, and the one just written is the only one
		// the next checkpoint owns: its 511 neighbours in the same 2 MiB keep
		// what they had.
		extents, err := vm.Volume("ram0").Locate(t.Context(), 0, 10*checkpoint.PageSize4KiB)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(extents); got != 10 {
			t.Fatalf("ten 4 KiB pages of the memory locate as %d extents: %+v", got, extents)
		}
		seen := make(map[control.Ref]bool, len(extents))
		for page, extent := range extents {
			if extent.Length != checkpoint.PageSize4KiB || extent.Identity.Page != uint64(page) {
				t.Fatalf("extent %d of the memory is %+v, want one 4 KiB page numbered %d",
					page, extent, page)
			}
			if page == 3 && extent.Identity.Ref != next {
				t.Fatalf("the page just written is %v, want the next checkpoint %v",
					extent.Identity.Ref, next)
			}
			if page != 3 && extent.Identity.Ref == next {
				t.Fatalf("page %d took the identity of the page written beside it", page)
			}
			if seen[extent.Identity.Ref] {
				t.Fatalf("page %d shares the identity %v with a page of another round",
					page, extent.Identity.Ref)
			}
			seen[extent.Identity.Ref] = true
		}
		disk, err := vm.Volume("disk").Locate(t.Context(), 0, 2*checkpoint.PageSize2MiB)
		if err != nil {
			t.Fatal(err)
		}
		if len(disk) != 2 || disk[0].Length != checkpoint.PageSize2MiB {
			t.Fatalf("the disk locates as %+v, want two 2 MiB pages", disk)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// A fork inherits each volume's geometry from the checkpoint it reads,
		// and publishes its own pages in the same units.
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range mixedSpecs {
			if got := child.Volume(spec.Name).PageSize(); got != spec.PageSize {
				t.Fatalf("the fork's %s is %d-byte pages, want %d", spec.Name, got, spec.PageSize)
			}
		}
		forked := clone(want)
		write(child, forked, "ram0", 17*checkpoint.PageSize4KiB, 0x5c, checkpoint.PageSize4KiB)
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		forked.check(t, child, "the fork after its root")
		want.check(t, vm, "the parent after the fork")
		// The parent's own next checkpoint republishes what the fork point
		// sealed, and the sweep behind it runs over both geometries.
		write(vm, want, "ram0", 33*checkpoint.PageSize4KiB, 0x77, checkpoint.PageSize4KiB)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		want.check(t, vm, "the parent after its next checkpoint")
		for _, held := range []*volume.VM{child, vm} {
			if err := held.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Whatever the two geometries left behind, it is still a deployment:
		// every checkpoint a record selects or pins is whole, and nothing is
		// unreachable but what a create leaves.
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix,
			volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch); err != nil {
			t.Fatalf("a deployment of mixed geometries is inconsistent:\n%v", err)
		}
		// Reopening reads the geometries back out of the store rather than from
		// anything this process remembered.
		reader := h.manager(t, h.config())
		defer reader.Close(t.Context())
		reopened, err := reader.Open(t.Context(), "mixed")
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range mixedSpecs {
			if got := reopened.Volume(spec.Name).PageSize(); got != spec.PageSize {
				t.Fatalf("the reopened %s is %d-byte pages, want %d", spec.Name, got, spec.PageSize)
			}
		}
		want.check(t, reopened, "the reopened VM")
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
