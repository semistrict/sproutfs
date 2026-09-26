package volume_test

import (
	"errors"
	"math/rand/v2"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// A fork point lasts as long as the children taken from it: every child is one
// hold, and the last hold to go ends the seal and gives the parent's sealed
// pages back to its running guest. A hold taken after that has nothing to keep.
// The point it names holds no seal, its checkpoint reads through to what the
// parent published before the pause, and the parent is storing into those pages
// again — so a child started from it would inherit a mixture of the pause and
// whatever the parent has done since.
//
// Every caller in the product takes its holds before any of them is given up,
// and says so where it does it. This is what makes that an invariant of the
// point rather than a convention of its callers.
func TestARetiredForkPointRefusesAnotherHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 17)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		workload(t, vm, want, rand.New(rand.NewPCG(17, 29)), 10)

		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := point.Hold(); err != nil {
			t.Fatalf("a hold on a point nothing has retired: %v", err)
		}
		// The point's own hold and the one above, both given up.
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if status := vm.Status(); status.Sealed {
			t.Fatalf("the parent is still sealed after its point was retired: %+v", status)
		}
		if err := point.Hold(); !errors.Is(err, volume.ErrRetired) {
			t.Fatalf("a hold on a retired point = %v, want ErrRetired", err)
		}
	})
}

// A point over a published checkpoint seals nothing: it is the checkpoint
// itself, pinned, and no running guest owns its pages again when a hold goes.
// So it outlives every child taken from it, one after another. A template's
// point is this kind, and a host keeps it for every create of that template:
// the first VM publishing its root gave up the last hold, and a point retired
// by that refused every later create on the host.
func TestAPointOverAPublishedCheckpointOutlivesItsChildren(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "template")
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		parent := vm.Status().Checkpoint
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := manager.Inherit(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			child, err := manager.Fork(t.Context(), id, point)
			if err != nil {
				t.Fatalf("forking %s from a point over a published checkpoint: %v", id, err)
			}
			// Its root is what gives its hold on the point up.
			if err := child.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := child.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A fork refused by a retired point leaves no child behind. The child's record
// is written before its hold is taken, and a record whose root nothing will
// ever publish is an identity nothing can use again: creating it reports that
// it exists, and a deployment lists it as a stopped VM for ever.
func TestAForkARetiredPointRefusesLeavesNoChildRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Fork(t.Context(), "child", point); !errors.Is(err, volume.ErrRetired) {
			t.Fatalf("forking from a retired point = %v, want ErrRetired", err)
		}
		if _, err := h.controlClient(t, h.objects).Read(t.Context(), "child"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the refused fork left the child's record behind: %v", err)
		}
	})
}
