package volume_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/volume"
)

// ephemeralSpecs is a VM with a disk every checkpoint holds and one no
// checkpoint holds.
var ephemeralSpecs = []volume.VolumeSpec{
	{Name: "root", Size: 2 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
	{Name: "scratch", Size: 3 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB, Ephemeral: true},
}

// requireZeroed requires one volume of a VM to be the ephemeral disk it was
// created as, at size, reading as zeroes.
func requireZeroed(t *testing.T, vm *volume.VM, name string, size uint64, what string) {
	t.Helper()
	v := vm.Volume(name)
	if v == nil || !v.Ephemeral() || v.Size() != size {
		t.Fatalf("%s: %s is %+v, want an ephemeral disk of %d bytes", what, name, v, size)
	}
	got := make([]byte, size)
	if err := v.Read(t.Context(), 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, size)) {
		t.Fatalf("%s: %s reads bytes, want zeroes", what, name)
	}
}

// Nothing reaches an ephemeral disk through a volume: a write, a discard and a
// pager's sealed pages are all refused, so no checkpoint of the VM ever holds a
// page of it. A VM opened again gets it back zeroed at its size.
func TestAnEphemeralVolumeIsNeverWrittenOrPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "boxed", ephemeralSpecs)
		if err != nil {
			t.Fatal(err)
		}
		scratch := vm.Volume("scratch")
		if err := scratch.Write(t.Context(), 0, []byte("upper layer")); !errors.Is(err, volume.ErrEphemeral) {
			t.Fatalf("a write to the ephemeral disk returned %v, want ErrEphemeral", err)
		}
		if err := scratch.Discard(t.Context(), 0, checkpoint.SectorSize); !errors.Is(err, volume.ErrEphemeral) {
			t.Fatalf("a discard of the ephemeral disk returned %v, want ErrEphemeral", err)
		}
		sealed := map[string]volume.DirtySource{
			"scratch": sealedPages{size: checkpoint.PageSize2MiB, pages: []uint64{1}, fill: 0x5a}}
		if _, err := vm.Snapshot(t.Context(), volume.Prepared(nil, sealed)); !errors.Is(err, volume.ErrEphemeral) {
			t.Fatalf("a snapshot of the ephemeral disk's pages returned %v, want ErrEphemeral", err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("durable")); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		reopened, err := h.manager(t, h.config()).Open(t.Context(), "boxed")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		requireZeroed(t, reopened, "scratch", 3*checkpoint.PageSize2MiB, "the reopened VM")
		checkStore(t, h)
	})
}

// A fork's child gets its parent's ephemeral disk zeroed, and a fork may give
// it ephemeral disks of its own: a new one, and the parent's at another size.
// Its root records both, so the child opened anywhere has them. A fork cannot
// add a disk every checkpoint holds, or turn one of those ephemeral.
func TestAForkGetsEphemeralDisksZeroed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		parent, err := manager.Create(t.Context(), "parent", ephemeralSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close(t.Context())
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		for _, refused := range []volume.VolumeSpec{
			{Name: "extra", Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
			{Name: "root", Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB, Ephemeral: true},
		} {
			if _, err := manager.Fork(t.Context(), "refused", point, refused); !errors.Is(err, volume.ErrInvalidConfig) {
				t.Fatalf("a fork adding %+v returned %v, want ErrInvalidConfig", refused, err)
			}
		}

		plain, err := manager.Fork(t.Context(), "plain", point)
		if err != nil {
			t.Fatal(err)
		}
		defer plain.Close(t.Context())
		requireZeroed(t, plain, "scratch", 3*checkpoint.PageSize2MiB, "a plain fork")

		given, err := manager.Fork(t.Context(), "given", point,
			volume.VolumeSpec{Name: "scratch", Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB,
				Ephemeral: true},
			volume.VolumeSpec{Name: "extra", Size: 4 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB,
				Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := given.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := given.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := h.manager(t, h.config()).Open(t.Context(), "given")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		requireZeroed(t, reopened, "scratch", checkpoint.PageSize2MiB, "the reopened fork")
		requireZeroed(t, reopened, "extra", 4*checkpoint.PageSize2MiB, "the reopened fork")
		checkStore(t, h)
	})
}
