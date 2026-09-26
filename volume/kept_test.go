package volume_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/volume"
)

// keptHistory publishes a VM's history around one kept checkpoint: an older
// checkpoint that published both pages, the kept one that rewrote page zero
// and so still reads page one out of the older one, and two more that rewrote
// both pages. Without the keep, the sweeps behind the last two delete the kept
// checkpoint and the older one. It returns the running VM, the older
// checkpoint and the kept one.
func keptHistory(t *testing.T, h *harness, manager *volume.Manager) (*volume.VM, control.Ref, control.Ref) {
	t.Helper()
	vm, write := forkedParent(t, h, manager, "vm")
	write(0, 1)
	write(checkpoint.PageSize2MiB, 2)
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	older := vm.Status().Checkpoint
	write(0, 3)
	ckpt, err := vm.Snapshot(t.Context(), volume.Prepared(nil, nil), volume.Terms{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	kept := ckpt.Ref()
	for _, value := range []byte{4, 5} {
		write(0, value)
		write(checkpoint.PageSize2MiB, value)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return vm, older, kept
}

// A kept checkpoint survives the sweeps behind every checkpoint after it, with
// every checkpoint it reads from, and a VM created from it reads exactly its
// bytes. The create pins it, and from then on it cannot be released.
func TestAKeptCheckpointSurvivesTheSweepsAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, older, kept := keptHistory(t, h, manager)
		for _, ref := range []control.Ref{older, kept} {
			if got := objectsUnder(t, h, store, ref); !slices.Equal(got, []string{"index", "part/0"}) {
				t.Fatalf("checkpoint %v kept %v, want its index and its part", ref, got)
			}
		}
		record := pins(t, h, "vm")
		if len(record.Kept) != 1 || record.Kept[0].Sequence != kept.Sequence || record.Kept[0].State {
			t.Fatalf("the record keeps %+v, want %d without state", record.Kept, kept.Sequence)
		}

		point, err := manager.InheritPublished(t.Context(), "child", kept)
		if err != nil {
			t.Fatalf("a point over a kept checkpoint the VM has moved past: %v", err)
		}
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := child.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		readsPage(t, h, "child", 0, 3)
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)
		if err := manager.Release(t.Context(), "vm", kept.Sequence); !errors.Is(err, control.ErrForked) {
			t.Fatalf("releasing a kept checkpoint a VM was created from = %v, want ErrForked", err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatal(err)
		}
	})
}

// Releasing a kept checkpoint no VM was created from deletes what only it held:
// itself, and the older checkpoint it alone still read from. The VM's writer
// is running, adopts the release, and publishes on as before. Nothing is left
// that the deployment check has to be told about.
func TestAReleasedCheckpointTakesWhatOnlyItHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, older, kept := keptHistory(t, h, manager)
		if err := manager.Release(t.Context(), "vm", kept.Sequence); err != nil {
			t.Fatalf("releasing a kept checkpoint: %v", err)
		}
		for _, ref := range []control.Ref{older, kept} {
			if got := objectsUnder(t, h, store, ref); len(got) != 0 {
				t.Fatalf("checkpoint %v still holds %v after the release", ref, got)
			}
		}
		if err := manager.Release(t.Context(), "vm", kept.Sequence); !errors.Is(err, control.ErrNotKept) {
			t.Fatalf("releasing it again = %v, want ErrNotKept", err)
		}
		if _, err := manager.InheritPublished(t.Context(), "child", kept); !errors.Is(err, control.ErrNotPublished) {
			t.Fatalf("a point over a released checkpoint = %v, want ErrNotPublished", err)
		}
		if err := vm.Volume("root").Write(t.Context(), 0, make([]byte, checkpoint.SectorSize)); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatalf("checkpointing the running VM after the release: %v", err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatal(err)
		}
	})
}

// A released checkpoint that is still the selected one stays: it is the VM's
// state. The sweep behind the next checkpoint takes it as it takes any
// checkpoint it replaced.
func TestAReleasedSelectedCheckpointGoesWithTheNextSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		defer vm.Close(t.Context())
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		ckpt, err := vm.Snapshot(t.Context(), volume.Prepared(nil, nil), volume.Terms{Keep: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Release(t.Context(), "vm", ckpt.Ref().Sequence); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, ckpt.Ref()); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the released selected checkpoint holds %v, want its index and its part", got)
		}
		write(0, 3)
		write(checkpoint.PageSize2MiB, 4)
		next, err := vm.Snapshot(t.Context(), volume.Prepared(nil, nil), volume.Terms{})
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Swept(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, ckpt.Ref()); len(got) != 0 {
			t.Fatalf("the replaced checkpoint still holds %v", got)
		}
	})
}

// A VM's delete spares only what a fork pinned: a kept checkpoint no VM was
// created from goes with the VM.
func TestDeletingAVMTakesItsKeptCheckpoints(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, older, kept := keptHistory(t, h, manager)
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []control.Ref{older, kept} {
			if got := objectsUnder(t, h, store, ref); len(got) != 0 {
				t.Fatalf("checkpoint %v still holds %v after the delete", ref, got)
			}
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatal(err)
		}
	})
}
