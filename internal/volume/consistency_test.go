package volume_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// check runs the deployment check and reports every violation it found, so a
// failure names the objects rather than a count.
func checkStore(t *testing.T, h *harness, allow ...volume.Allowance) {
	t.Helper()
	if err := volume.CheckDeployment(context.Background(), h.objects, h.prefix, allow...); err != nil {
		t.Fatalf("the deployment is inconsistent:\n%v", err)
	}
}

// violations runs the check and returns what it found, which is what a test
// asserting a particular leak looks at.
func violations(t *testing.T, h *harness, allow ...volume.Allowance) []volume.Violation {
	t.Helper()
	err := volume.CheckDeployment(context.Background(), h.objects, h.prefix, allow...)
	if err == nil {
		return nil
	}
	inconsistent := new(volume.InconsistentError)
	if !errors.As(err, &inconsistent) {
		t.Fatalf("CheckDeployment = %v, want an *InconsistentError", err)
	}
	return inconsistent.Violations
}

// A deployment a writer took through checkpoints, a fork, a reopen and a delete
// holds nothing but what its records name, once every leftover a lost host
// would have left is named.
func TestCheckDeploymentAcceptsAQuiescedDeployment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 5)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(5, 17))
		vm, want := createVM(t, manager, "parent")
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		forked := clone(want)
		workload(t, fork, forked, random, 20)
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The point the child was forked at is pinned in the parent's record,
		// which is what keeps every checkpoint the child reads through.
		checkStore(t, h, volume.AllowUnreferencedCheckpoint)
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "child"); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "parent"); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The child took everything of its own with it. The parent left the
		// checkpoint it was forked at, because a pin is permanent and its
		// delete could not establish that the child was gone — objects under a
		// VM with no record, which is what the collector is for.
		checkStore(t, h, volume.AllowUnrecordedVM)
		found := violations(t, h)
		for _, violation := range found {
			if violation.Class != volume.AllowUnrecordedVM ||
				!strings.HasPrefix(violation.Key, "sproutfs/vm/parent/ckpt/") {
				t.Fatalf("unexpected violation: %v", violation)
			}
		}
		if len(found) == 0 {
			t.Fatal("the deleted parent's pinned checkpoints went with it")
		}
	})
}

// A VM reopened by a second handle leaves the checkpoint it opened on behind:
// the new handle reclaims only what it published itself. Nothing else is
// unreferenced, so the check names exactly that class.
func TestCheckDeploymentFindsTheCheckpointAnOpenLeavesBehind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 7)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(7, 19))
		vm, want := createVM(t, manager, "reopened")
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		again, err := manager.Open(t.Context(), "reopened")
		if err != nil {
			t.Fatal(err)
		}
		workload(t, again, want, random, 20)
		for range 2 {
			if err := again.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := again.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		found := violations(t, h, volume.AllowUnreferencedCheckpoint)
		if len(found) == 0 {
			t.Fatal("the checkpoint the second open left behind was not reported")
		}
		for _, violation := range found {
			if violation.Class != volume.AllowSupersededEpoch {
				t.Fatalf("unexpected violation: %v", violation)
			}
			if !strings.HasPrefix(violation.Key, "sproutfs/vm/reopened/ckpt/") {
				t.Fatalf("unexpected key: %v", violation)
			}
		}
		checkStore(t, h, volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch)
	})
}

// A pin is a promise that a checkpoint is still whole, so a record pinning a
// sequence the store has no index for is durable state disagreeing with itself.
// No host loss produces it: the pin is written on a checkpoint that is already
// published, and nothing after that deletes a pinned one.
func TestCheckDeploymentFindsAPinOnACheckpointThatIsNotThere(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 11)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(11, 23))
		vm, want := createVM(t, manager, "parent")
		workload(t, vm, want, random, 10)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		records := h.controlClient(t, h.objects)
		handle, err := records.Open(t.Context(), "parent")
		if err != nil {
			t.Fatal(err)
		}
		missing := counted(vm, 9)
		if _, err := handle.Pin(t.Context(), missing); err != nil {
			t.Fatal(err)
		}
		handle.Close()
		found := violations(t, h, volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch)
		if len(found) != 1 || found[0].Class != 0 {
			t.Fatalf("violations = %v, want one pin on a checkpoint that is not there", found)
		}
		if got := found[0].Err.Error(); !strings.Contains(got, "a pinned checkpoint") {
			t.Fatalf("unexpected violation: %q", got)
		}
	})
}

// A part the selected checkpoint's root names and the store does not hold is
// durable state disagreeing with itself, whatever was lost. The part taken away
// is one the checkpoint before the selected one published, which the selected
// one still reads most of its pages through: that checkpoint opens, and what it
// names does not read.
func TestCheckDeploymentFindsAMissingPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 17)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(17, 31))
		vm, want := createVM(t, manager, "vm")
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		workload(t, vm, want, random, 1)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		key, err := platform.NewObjectKey("sproutfs/vm/vm/ckpt/" +
			strconv.FormatUint(counted(vm, 2), 10) + "/part/0")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.objects.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
			t.Fatal(err)
		}
		found := violations(t, h, volume.AllowUnreferencedCheckpoint)
		if len(found) != 1 || found[0].Class != 0 {
			t.Fatalf("violations = %v, want one missing part", found)
		}
		if got := found[0].Err.Error(); !strings.Contains(got, "does not read") {
			t.Fatalf("unexpected violation: %q", got)
		}
	})
}

// The root index a create publishes is this handle's own publication, so the
// first checkpoint over it reclaims it like any other checkpoint it replaced.
// Nothing must be left in the store that the record does not name.
func TestCreateLeavesNoRootIndexBehind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 19)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(19, 37))
		vm, want := createVM(t, manager, "vm")
		for range 2 {
			workload(t, vm, want, random, 10)
			if err := vm.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		checkStore(t, h)
	})
}

// A fork closed before it ever published its root published nothing and never
// will: this handle was the only thing that could. Its record goes with it, the
// way the unwinding inside Fork removes one, so the identity is usable again.
// The pin it left on its parent stays, because nothing gives a pin back.
func TestAnAbandonedForkTakesItsRecordWithIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 23)
		manager := h.manager(t, h.config())
		random := rand.New(rand.NewPCG(23, 41))
		vm, want := createVM(t, manager, "parent")
		workload(t, vm, want, random, 10)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(t.Context(), "abandoned", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		records := h.controlClient(t, h.objects)
		if _, err := records.Read(t.Context(), "abandoned"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the abandoned fork's record survived its close: %v", err)
		}
		// The identity is free again, which is the whole point of removing it.
		again, err := manager.Create(t.Context(), "abandoned", testSpecs)
		if err != nil {
			t.Fatal(err)
		}
		if err := again.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "abandoned"); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The parent still pins the point it was forked at, and still selects
		// it, so nothing is left over at all.
		checkStore(t, h)
		if !pins(t, h, "parent").IsPinned(vm.Status().Checkpoint.Sequence) {
			t.Fatal("the abandoned fork took the parent's pin with it")
		}
	})
}
