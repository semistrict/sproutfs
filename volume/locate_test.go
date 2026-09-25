package volume_test

import (
	"bytes"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/volume"
)

func locate(t *testing.T, vm *volume.VM, name string, offset, length uint64) []control.Extent {
	t.Helper()
	extents, err := vm.Volume(name).Locate(t.Context(), offset, length)
	if err != nil {
		t.Fatal(err)
	}
	return extents
}

// Bytes an overlay holds are private to the checkpoint that will publish them,
// bytes no one has written report no object at all, and each extent stays
// inside one page, which is what identifies a resident page.
func TestLocateReportsPrivateOverlayIdentities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())

		empty := locate(t, vm, "root", 0, 3<<20)
		if !reflect.DeepEqual(empty, []control.Extent{{Offset: 0, Length: 3 << 20, Identity: control.Identity{Zero: true}}}) {
			t.Fatalf("Locate of an untouched volume = %+v", empty)
		}

		page := bytes.Repeat([]byte{1}, checkpoint.SectorSize)
		if err := vm.Volume("root").Write(t.Context(), 0, page); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], page)
		private := control.Identity{Ref: control.Ref{VM: "vm", Sequence: counted(vm, 2)}, Volume: "root", Page: 0}
		// One storage page written makes the whole 2 MiB page the checkpoint's,
		// because that is what the checkpoint republishes.
		written := locate(t, vm, "root", 0, 3<<20)
		if !reflect.DeepEqual(written, []control.Extent{
			{Offset: 0, Length: checkpoint.PageSize2MiB, Identity: private},
			{Offset: checkpoint.PageSize2MiB, Length: 3<<20 - checkpoint.PageSize2MiB, Identity: control.Identity{Zero: true}},
		}) {
			t.Fatalf("Locate over an overlay = %+v", written)
		}

		// An overlay run crossing a page boundary is reported as one extent
		// per page.
		if err := vm.Volume("root").Write(t.Context(), checkpoint.PageSize2MiB-checkpoint.SectorSize, bytes.Repeat([]byte{2}, 2*checkpoint.SectorSize)); err != nil {
			t.Fatal(err)
		}
		copy(want["root"][checkpoint.PageSize2MiB-checkpoint.SectorSize:], bytes.Repeat([]byte{2}, 2*checkpoint.SectorSize))
		crossing := locate(t, vm, "root", checkpoint.PageSize2MiB-checkpoint.SectorSize, 2*checkpoint.SectorSize)
		if !reflect.DeepEqual(crossing, []control.Extent{
			{Offset: checkpoint.PageSize2MiB - checkpoint.SectorSize, Length: checkpoint.SectorSize,
				Identity: control.Identity{Ref: control.Ref{VM: "vm", Sequence: counted(vm, 2)}, Volume: "root", Page: 0}},
			{Offset: checkpoint.PageSize2MiB, Length: checkpoint.SectorSize,
				Identity: control.Identity{Ref: control.Ref{VM: "vm", Sequence: counted(vm, 2)}, Volume: "root", Page: 1}},
		}) {
			t.Fatalf("Locate across a page boundary = %+v", crossing)
		}

		// Publishing does not change what the bytes are called: the overlay
		// already reported the reference they were published under.
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if published := locate(t, vm, "root", 0, checkpoint.SectorSize); !reflect.DeepEqual(published, []control.Extent{
			{Offset: 0, Length: checkpoint.SectorSize, Identity: private},
		}) {
			t.Fatalf("Locate after publishing = %+v, want the same identity", published)
		}
		want.check(t, vm, "after the checkpoint")
	})
}

// A fork inherits its source's identities unchanged, so a host shares the
// pages it already has, and the two diverge only where the fork wrote.
func TestLocateIdentitiesAreInheritedAcrossAFork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		page := bytes.Repeat([]byte{3}, checkpoint.SectorSize)
		for _, offset := range []uint64{0, checkpoint.SectorSize} {
			if err := vm.Volume("root").Write(t.Context(), offset, page); err != nil {
				t.Fatal(err)
			}
			copy(want["root"][offset:], page)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		inherited := locate(t, vm, "root", 0, 2*checkpoint.SectorSize)

		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		forked := clone(want)
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(t.Context())
		if got := locate(t, fork, "root", 0, 2*checkpoint.SectorSize); !reflect.DeepEqual(got, inherited) {
			t.Fatalf("the fork's identities = %+v, want the source's %+v", got, inherited)
		}

		// Writing into a page and publishing gives that whole page the fork's
		// own identity, and leaves every page it did not touch inherited.
		changed := bytes.Repeat([]byte{4}, checkpoint.SectorSize)
		if err := fork.Volume("root").Write(t.Context(), 0, changed); err != nil {
			t.Fatal(err)
		}
		copy(forked["root"], changed)
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		forkOwned := control.Identity{Ref: control.Ref{VM: "fork", Sequence: counted(fork, 1)}, Volume: "root", Page: 0}
		diverged := locate(t, fork, "root", 0, 3<<20)
		if !reflect.DeepEqual(diverged, []control.Extent{
			{Offset: 0, Length: checkpoint.PageSize2MiB, Identity: forkOwned},
			{Offset: checkpoint.PageSize2MiB, Length: 3<<20 - checkpoint.PageSize2MiB, Identity: control.Identity{Zero: true}},
		}) {
			t.Fatalf("the fork's identities after writing = %+v", diverged)
		}
		if got := locate(t, vm, "root", 0, 2*checkpoint.SectorSize); !reflect.DeepEqual(got, inherited) {
			t.Fatalf("the source's identities changed to %+v", got)
		}
		want.check(t, vm, "the source")
		forked.check(t, fork, "the fork")
	})
}
