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

// A VM nobody runs is forked from the checkpoint its stop published. Nothing
// holds its epoch, so the pin is written without one, and it is what keeps
// that checkpoint whole when the stopped VM is deleted: the child still reads
// every page it inherited.
func TestAPointOverAStoppedVMPinsTheCheckpointItInherits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		stopped := vm.Status().Checkpoint
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		before := pins(t, h, "vm")

		point, err := manager.InheritPublished(t.Context(), control.Ref{VM: "vm"})
		if err != nil {
			t.Fatalf("a point over a stopped VM's checkpoint: %v", err)
		}
		if point.Parent() != stopped {
			t.Fatalf("the point inherits %v, want %v", point.Parent(), stopped)
		}
		after := pins(t, h, "vm")
		if after.Epoch != before.Epoch || !slices.Equal(after.Pinned, []uint64{stopped.Sequence}) {
			t.Fatalf("the stopped VM's record is at epoch %d pinning %v, want epoch %d pinning %d",
				after.Epoch, after.Pinned, before.Epoch, stopped.Sequence)
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

		// A delete sweeps every checkpoint no pin keeps.
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, stopped); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the checkpoint the child inherits kept %v", got)
		}
		readsPage(t, h, "child", 0, 1)
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)
	})
}

// A VM that turns out to be running when it is pinned without its writer
// keeps publishing. Its next selection adopts the pin, so the sweeps behind
// its next checkpoints spare the checkpoint the child inherits, which is one
// that writer published and would otherwise reclaim.
func TestARunningVMPinnedWithoutItsWriterSparesThePin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		defer vm.Close(t.Context())
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		published := vm.Status().Checkpoint
		point, err := manager.InheritPublished(t.Context(), control.Ref{VM: "vm", Sequence: published.Sequence})
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
		if err := child.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, value := range []byte{3, 4} {
			write(0, value)
			write(checkpoint.PageSize2MiB, value)
			if err := vm.Checkpoint(t.Context()); err != nil {
				t.Fatalf("checkpointing the running VM after the pin: %v", err)
			}
		}
		if got := objectsUnder(t, h, store, published); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the checkpoint the child inherits kept %v", got)
		}
		readsPage(t, h, "child", 0, 1)
		readsPage(t, h, "child", checkpoint.PageSize2MiB, 2)
	})
}

// A point without the writer is refused for what it could not keep: a fork
// whose root never published, and a checkpoint the record no longer selects
// and no pin keeps.
func TestAPointWithoutTheWriterRefusesWhatItCannotPin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		defer vm.Close(t.Context())
		write(0, 1)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := vm.Status().Checkpoint
		write(0, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.InheritPublished(t.Context(), first); !errors.Is(err, control.ErrNotPublished) {
			t.Fatalf("a point over a checkpoint the record no longer selects = %v, want ErrNotPublished", err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(t.Context())
		if _, err := manager.InheritPublished(t.Context(), control.Ref{VM: "fork"}); !errors.Is(err, control.ErrNotPublished) {
			t.Fatalf("a point over a fork whose root never published = %v, want ErrNotPublished", err)
		}
	})
}
