package volume_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// readsPage checks that one VM still reads the page it inherited, opening it as
// a host that never held it would.
func readsPage(t *testing.T, h *harness, id string, offset uint64, want byte) {
	t.Helper()
	elsewhere := h.manager(t, h.config())
	defer elsewhere.Close(t.Context())
	vm, err := elsewhere.Open(t.Context(), id)
	if err != nil {
		t.Fatalf("opening %s: %v", id, err)
	}
	defer vm.Close(t.Context())
	got := make([]byte, checkpoint.SectorSize)
	if err := vm.Volume("root").Read(t.Context(), offset, got); err != nil {
		t.Fatalf("reading %s at %d: %v", id, offset, err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{want}, checkpoint.SectorSize)) {
		t.Fatalf("%s read %d... at %d, want %d...", id, got[0], offset, want)
	}
}

// chain publishes a grandparent, a child forked from it and a grandchild forked
// from that, each reading one page through its ancestors: the grandparent
// published both pages, the child rewrote page zero, and the grandchild rewrote
// page zero again. Every writer is closed when it reports.
func chain(t *testing.T, h *harness, manager *volume.Manager) {
	t.Helper()
	vm, write := forkedParent(t, h, manager, "vm")
	write(0, 1)
	write(checkpoint.PageSize2MiB, 2)
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Fork(t.Context(), "child", point)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Volume("root").Write(t.Context(), 0,
		bytes.Repeat([]byte{3}, checkpoint.SectorSize)); err != nil {
		t.Fatal(err)
	}
	if err := child.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	childPoint, err := child.ForkPoint(t.Context(), volume.Prepared(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := manager.Fork(t.Context(), "grandchild", childPoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := grandchild.Volume("root").Write(t.Context(), 0,
		bytes.Repeat([]byte{4}, checkpoint.SectorSize)); err != nil {
		t.Fatal(err)
	}
	if err := grandchild.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, open := range []*volume.VM{grandchild, child, vm} {
		if err := open.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// Deleting a VM leaves every descendant of it readable: the objects a pin
// protects are not the deleted VM's to take away, whatever the record says
// about them afterwards.
func TestDeletingAParentLeavesItsChildReadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, open := range []*volume.VM{child, vm} {
			if err := open.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}

		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.controlClient(t, h.objects).Read(t.Context(), "vm"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("reading the deleted parent's record = %v, want ErrNotFound", err)
		}
		readsPage(t, h, "child", 0, 1)
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)

		// Deleting again is how a caller finishes a delete it never saw
		// finish, and a deleted VM that was forked is exactly a VM with no
		// record whose objects are still read. The repeat must not take them.
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatalf("repeating the delete: %v", err)
		}
		readsPage(t, h, "child", 0, 1)
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)
	})
}

// A fork chain deleted in any order leaves every survivor readable: a pin is
// permanent, so no delete of one link can reach the objects another link reads.
func TestDeletingAForkChainInAnyOrderLeavesTheSurvivorsReadable(t *testing.T) {
	for _, order := range [][]string{
		{"vm", "child", "grandchild"},
		{"grandchild", "child", "vm"},
		{"child", "vm", "grandchild"},
		{"child", "grandchild", "vm"},
	} {
		t.Run(order[0]+"-"+order[1]+"-"+order[2], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				defer h.close(t.Context())
				manager, _ := rewritingManager(t, h)
				defer manager.Close(t.Context())
				chain(t, h, manager)
				alive := map[string]bool{"vm": true, "child": true, "grandchild": true}
				// Every page each survivor reads, by the value it or an ancestor
				// published there.
				pages := map[string][2]byte{
					"vm":         {1, 2},
					"child":      {3, 2},
					"grandchild": {4, 2},
				}
				for _, id := range order {
					if err := manager.Delete(t.Context(), id); err != nil {
						t.Fatalf("deleting %s: %v", id, err)
					}
					delete(alive, id)
					for survivor := range alive {
						want := pages[survivor]
						readsPage(t, h, survivor, 0, want[0])
						readsPage(t, h, survivor, checkpoint.PageSize2MiB, want[1])
					}
				}
			})
		})
	}
}

// A VM nothing was ever forked from takes its objects with it: its identity
// must be usable again, and every object of it is named by that identity and a
// sequence that starts again from the creating epoch's first.
func TestDeletingANeverForkedVMSweepsItsObjects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		if got := h.objectKeys(t); len(got) != 0 {
			t.Fatalf("the deployment kept %v, want nothing of the deleted VM", got)
		}
	})
}

// A record that cannot be read says nothing about what its VM's objects are:
// its pins are exactly what a sweep would have to spare, so a delete that
// cannot read them is refused rather than run over checkpoints a descendant
// may still be reading.
func TestDeletingAVMWithACorruptRecordIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, open := range []*volume.VM{child, vm} {
			if err := open.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		before := h.objectKeys(t)

		key, err := platform.NewObjectKey(h.prefix.String() + control.RecordPrefix + "vm")
		if err != nil {
			t.Fatal(err)
		}
		garbage := []byte("this is not a control record")
		if _, err := h.objects.Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader(garbage), Size: int64(len(garbage))}); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "vm"); !errors.Is(err, control.ErrCorrupt) {
			t.Fatalf("deleting a VM whose record is corrupt = %v, want ErrCorrupt", err)
		}
		for _, key := range before {
			if !slices.Contains(h.objectKeys(t), key) {
				t.Fatalf("the refused delete removed %s", key)
			}
		}
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)
	})
}
