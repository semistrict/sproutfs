package volume_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

func clone(m model) model {
	copied := make(model, len(m))
	for name, data := range m {
		copied[name] = bytes.Clone(data)
	}
	return copied
}

// A checkpoint is immutable: it keeps reading as the VM did at the position it
// was taken however much the VM writes afterwards.
func TestSnapshotIsConsistentWhileWritesContinue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 11)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		random := rand.New(rand.NewPCG(11, 19))
		workload(t, vm, want, random, 20)

		checkpoint, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("vmm state"), nil), volume.Terms{})
		if err != nil {
			t.Fatal(err)
		}
		at := clone(want)
		first := control.Ref{VM: "vm", Sequence: counted(vm, 2)}
		if checkpoint.Ref() != first {
			t.Fatalf("Ref = %+v, want %v", checkpoint.Ref(), first)
		}
		if string(checkpoint.State()) != "vmm state" {
			t.Fatalf("State = %q", checkpoint.State())
		}
		for range 5 {
			workload(t, vm, want, random, 10)
			at.checkCheckpoint(t, checkpoint, "the checkpoint while writes continue")
			want.check(t, vm, "the live VM while the checkpoint publishes")
		}
		if err := checkpoint.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		at.checkCheckpoint(t, checkpoint, "the published checkpoint")
		if status := vm.Status(); status.Checkpoint != first {
			t.Fatalf("Status after the snapshot = %+v", status)
		}
		want.check(t, vm, "after the snapshot was published")
	})
}

// A fork starts as its parent's fork point and then goes its own way. Neither
// VM can see the other's writes, before or after the fork publishes its root.
func TestForkAndSourceDiverge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 13)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		random := rand.New(rand.NewPCG(13, 23))
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		workload(t, vm, want, random, 10)

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
		forked.check(t, fork, "the fresh fork")
		if status := vm.Status(); !status.Sealed {
			t.Fatalf("the parent is not sealed while a fork point holds its pages: %+v", status)
		}
		if status := fork.Status(); !status.Root {
			t.Fatalf("a fork that has not published its root reports %+v", status)
		}

		// Both diverge while the fork's root is still unpublished.
		workload(t, vm, want, random, 10)
		workload(t, fork, forked, random, 10)
		want.check(t, vm, "the source after diverging")
		forked.check(t, fork, "the fork after diverging")

		// The fork's first checkpoint is its root index: it publishes the pages
		// it inherited as its own, and the parent's seal ends there.
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		root := control.Ref{VM: "fork", Sequence: counted(fork, 1)}
		if status := fork.Status(); status.Checkpoint != root || status.Root {
			t.Fatalf("fork Status = %+v, want %v", status, root)
		}
		if status := vm.Status(); status.Sealed {
			t.Fatalf("the parent is still sealed after its child published: %+v", status)
		}
		workload(t, vm, want, random, 10)
		workload(t, fork, forked, random, 10)
		want.check(t, vm, "the source after the fork published")
		forked.check(t, fork, "the fork after it published")

		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "fork")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		forked.check(t, reopened, "the reopened fork")
		want.check(t, vm, "the source after the fork was reopened")
	})
}

// Nothing is published to take a fork. The parent's checkpoint is the one it
// already had, and a fork that ends before its own first checkpoint leaves no
// object behind at all.
func TestForkPublishesNothingOnTheParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("published")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("published"))
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Everything after this checkpoint is the parent's alone until one side
		// publishes it.
		if err := vm.Volume("state").Write(t.Context(), 0, []byte("unpublished")); err != nil {
			t.Fatal(err)
		}
		copy(want["state"], []byte("unpublished"))
		selected := vm.Status().Checkpoint
		before := h.objectKeys(t)

		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if point.Parent() != selected {
			t.Fatalf("the fork inherits %v, want the selected %v", point.Parent(), selected)
		}
		if pages := point.Pages("state"); len(pages) != 1 || pages[0] != 0 {
			t.Fatalf("the unpublished pages of state are %v, want page 0", pages)
		}
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		want.check(t, fork, "the fork before it published anything")
		if status := vm.Status(); status.Checkpoint != selected {
			t.Fatalf("the parent published a checkpoint for the fork: %+v", status)
		}
		// The fork's control record is the only object a fork writes; no index
		// and no checkpoint exists for either side.
		after := h.objectKeys(t)
		if extra := added(before, after); len(extra) != 1 || extra[0] != "control/fork" {
			t.Fatalf("forking wrote %v, want only the fork's control record", extra)
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The record goes with the close: a fork that never published is an
		// identity nothing could use again while it stood.
		if extra := added(before, h.objectKeys(t)); len(extra) != 0 {
			t.Fatalf("a fork that never checkpointed left %v", extra)
		}
		if status := vm.Status(); status.Sealed {
			t.Fatalf("the parent kept its seal after the fork closed: %+v", status)
		}
		// The parent can be sealed again, and its own next checkpoint publishes
		// the pages the fork never did.
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		want.check(t, vm, "the parent after the fork was abandoned")
	})
}

// Until it publishes its own root index a fork exists only on the host that
// created it: it reads and writes there, and anywhere else it reports that its
// root is not published.
func TestForkBeforeItPublishesItsRoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("parent")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("parent"))
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

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
		forked.check(t, fork, "the fork of an unpublished root")
		if err := fork.Volume("root").Write(t.Context(), 4096, []byte("local")); err != nil {
			t.Fatal(err)
		}
		copy(forked["root"][4096:], []byte("local"))
		forked.check(t, fork, "the fork after writing locally")

		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		if _, err := elsewhere.Open(t.Context(), "fork"); !errors.Is(err, volume.ErrForkPending) {
			t.Fatalf("opening the fork elsewhere = %v, want ErrForkPending", err)
		}

		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		forked.check(t, fork, "the fork once it published its root")
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := elsewhere.Open(t.Context(), "fork")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		forked.check(t, reopened, "the fork reopened elsewhere")
	})
}

// One checkpoint of a VM's pages is outstanding at a time, so a VM a fork
// point holds refuses another fork and refuses a capture, and says so before
// anything pauses its guest.
func TestForkPointRefusesASecondSeal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil)); !errors.Is(err, volume.ErrSealed) {
			t.Fatalf("a second fork point = %v, want ErrSealed", err)
		}
		sealed := func(context.Context) ([]byte, map[string]volume.DirtySource, error) {
			t.Fatal("a capture paused a guest whose pages a fork point holds")
			return nil, nil, nil
		}
		if _, err := vm.Snapshot(t.Context(), sealed, volume.Terms{}); !errors.Is(err, volume.ErrSealed) {
			t.Fatalf("a capture of a sealed VM = %v, want ErrSealed", err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil)); err != nil {
			t.Fatalf("forking after the point was retired: %v", err)
		}
	})
}

// Forking onto an identity that already has a control record is refused rather
// than adopting it, and the VM that holds that identity is left exactly as it
// was: a refused fork must not make an existing VM unopenable.
func TestForkRefusesAnExistingIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		other, _ := createVM(t, manager, "other")
		defer other.Close(t.Context())
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Fork(t.Context(), "other", point); !errors.Is(err, volume.ErrExists) {
			t.Fatalf("Fork onto an existing VM = %v, want ErrExists", err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := other.Checkpoint(t.Context()); err != nil {
			t.Fatalf("the VM the refused fork named: %v", err)
		}
		if err := other.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		reopened, err := elsewhere.Open(t.Context(), "other")
		if err != nil {
			t.Fatalf("opening the VM a refused fork named: %v", err)
		}
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A fork that could not build its child's handle leaves no record of the child
// behind. The record is written before the handle, and a child's record selects
// a root only the child's own first checkpoint publishes: one left behind is an
// identity nothing can ever use again, because creating it reports that it
// exists and opening it reports a fork still pending.
//
// The manager below closes while the child's record is being written — past the
// check that would have refused the fork, and short of the handle that would
// have owned what it wrote.
func TestAForkThatCannotAttachLeavesNoChildRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		vm, _ := createVM(t, manager, "vm")
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
		forked := make(chan error, 1)
		go func() {
			_, err := manager.Fork(context.WithoutCancel(t.Context()), "child", point)
			forked <- err
		}()
		synctest.Wait()
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-forked; !errors.Is(err, volume.ErrClosed) {
			t.Fatalf("forking through a closing manager = %v, want ErrClosed", err)
		}
		if _, err := h.controlClient(t, h.objects).Read(t.Context(), "child"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the failed fork left the child's record behind: %v", err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
