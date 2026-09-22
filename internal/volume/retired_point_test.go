package volume_test

import (
	"errors"
	"math/rand/v2"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/volume"
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
