package host_test

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// errRuntime is the failure a fake VMM process reports.
var errRuntime = errors.New("host_test: runtime failure")

// held builds a manager whose checkpoint index objects can be blocked, so a
// capture can be observed between its checkpoint and its publication. It
// returns the manager, its checkpoint store and the hold.
func held(t *testing.T, h *harness) (*volume.Manager, *checkpoint.Store, *holdStore) {
	t.Helper()
	hold := &holdStore{ObjectStore: h.objects}
	store := h.imageStore(t, hold)
	config := h.config()
	config.Store = store
	return h.manager(t, config), store, hold
}

// A capture returns as soon as the checkpoint exists: the VM is already resumed
// and writing again while the publication is still in flight, and the
// checkpoint keeps reading as the VM did at the moment it was taken.
func TestCaptureReturnsBeforeItsPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 5)
		defer h.close(t.Context())
		manager, store, hold := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		random := rand.New(rand.NewPCG(5, 17))
		workload(t, vm, want, random, 20)

		state := vmmState(3)
		runtime := &fakeRuntime{state: state}
		release := hold.hold(indexKey(control.Ref{VM: "vm", Sequence: secondSeq(vm)}))
		ckpt, err := host.Capture(t.Context(), vm, runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		at := want.clone()
		// The capture is the pause and nothing else: the publication owns the
		// checkpoints from here and retires them itself, so nothing releases the VM.
		runtime.check(t, "prepare", "resume")
		if ckpt.Ref() != (control.Ref{VM: "vm", Sequence: secondSeq(vm)}) {
			t.Fatalf("Ref = %+v, want vm/2", ckpt.Ref())
		}
		if !bytes.Equal(ckpt.State(), state) {
			t.Fatalf("State = %d bytes, want the %d bytes Prepare returned", len(ckpt.State()), len(state))
		}
		synctest.Wait()
		if got := vm.Status().Checkpoint; got != (control.Ref{VM: "vm", Sequence: rootSeq(vm)}) {
			t.Fatalf("Checkpoint while the publication is held = %+v, want the root checkpoint", got)
		}

		// The source writes on after Release; the checkpoint does not move with it.
		for range 5 {
			workload(t, vm, want, random, 10)
			at.checkCheckpoint(t, ckpt, "the checkpoint while the source writes on")
			want.check(t, vm, "the source while the checkpoint publishes")
		}

		release()
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		at.checkCheckpoint(t, ckpt, "the published checkpoint")
		want.check(t, vm, "the source after the publication landed")
		if got := vm.Status().Checkpoint; got != (control.Ref{VM: "vm", Sequence: secondSeq(vm)}) {
			t.Fatalf("Checkpoint after the publication = %+v, want vm/2", got)
		}
		published, err := host.State(t.Context(), store, ckpt.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, state) {
			t.Fatalf("published state is %d bytes, want the %d captured", len(published), len(state))
		}
		runtime.check(t, "prepare", "resume")
	})
}

// A capture whose Prepare fails resumes the VM and publishes nothing: the VM
// keeps the checkpoint it had and no second checkpoint exists to read.
func TestPrepareFailureReleasesAndCapturesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		workload(t, vm, want, rand.New(rand.NewPCG(2, 3)), 10)

		runtime := &fakeRuntime{state: vmmState(1), prepareErr: errRuntime}
		ckpt, err := host.Capture(t.Context(), vm, runtime, nil)
		if !errors.Is(err, errRuntime) {
			t.Fatalf("Capture with a failing Prepare = %v, want errRuntime", err)
		}
		if ckpt != nil {
			t.Fatalf("Capture with a failing Prepare returned a checkpoint %+v", ckpt.Ref())
		}
		runtime.check(t, "prepare", "release")
		synctest.Wait()
		if got := vm.Status().Checkpoint; got != (control.Ref{VM: "vm", Sequence: rootSeq(vm)}) {
			t.Fatalf("Checkpoint after a failed Prepare = %+v, want the root checkpoint", got)
		}
		if _, err := host.State(t.Context(), store, control.Ref{VM: "vm", Sequence: secondSeq(vm)}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("State of the checkpoint that was never taken = %v, want ErrNotFound", err)
		}
		want.check(t, vm, "the VM after a failed Prepare")
	})
}

// A capture whose Resume fails releases the VM and publishes nothing: the guest
// did not come back, so nothing may be sealed on its behalf either.
func TestResumeFailureCapturesNothingAndReleases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		workload(t, vm, want, rand.New(rand.NewPCG(4, 5)), 10)

		runtime := &fakeRuntime{state: vmmState(1), resumeErr: errRuntime}
		ckpt, err := host.Capture(t.Context(), vm, runtime, nil)
		if !errors.Is(err, errRuntime) {
			t.Fatalf("Capture with a failing Resume = %v, want errRuntime", err)
		}
		if ckpt != nil {
			t.Fatalf("Capture with a failing Resume returned a checkpoint %+v", ckpt.Ref())
		}
		runtime.check(t, "prepare", "resume", "release")
		synctest.Wait()
		if got := vm.Status().Checkpoint; got != (control.Ref{VM: "vm", Sequence: rootSeq(vm)}) {
			t.Fatalf("Checkpoint after a failed Resume = %+v, want the root checkpoint", got)
		}
		if _, err := host.State(t.Context(), store, control.Ref{VM: "vm", Sequence: secondSeq(vm)}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("State of the checkpoint that was never taken = %v, want ErrNotFound", err)
		}
		want.check(t, vm, "the VM after a failed Resume")
	})
}

// A checkpoint publishes the sealed pager pages of every region, and the
// publication retires them once its checkpoint is selected: the pager is told
// its pages are the volume's now, which is what makes them clean under the new
// lineage.
func TestCapturePublishesSealedPagesAndRetiresThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 23)
		defer h.close(t.Context())
		manager, _, hold := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)

		// One whole 2 MiB pager page of a region the pager owns. Nothing was
		// written through the volume, so this is all the checkpoint has.
		source := newFakeSource(2 << 20)
		sealed := bytes.Repeat([]byte{0x5a}, 4096)
		source.set(1, sealed)
		copy(want["ram"][2<<20:], sealed)

		release := hold.hold(indexKey(control.Ref{VM: "vm", Sequence: secondSeq(vm)}))
		runtime := &fakeRuntime{state: vmmState(8), sources: map[string]volume.DirtySource{"ram": source}}
		ckpt, err := host.Capture(t.Context(), vm, runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if retires, _ := source.outcome(); retires != 0 {
			t.Fatalf("the checkpoint was retired %d times before its publication landed", retires)
		}
		// The checkpoint reads the sealed pages, so a fork of it sees them before
		// any object exists.
		want.checkCheckpoint(t, ckpt, "the sealed pages through the unpublished checkpoint")

		release()
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if retires, published := source.outcome(); retires != 1 || !published {
			t.Fatalf("the checkpoint was retired %d times, published=%v; want exactly one retire as published", retires, published)
		}
		want.check(t, vm, "the VM after its sealed pages were published")

		// A second host reads the sealed bytes out of the object store alone.
		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		opened, err := elsewhere.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, opened)
		want.check(t, opened, "the sealed pages read from the object store")
	})
}

// A publication that never lands hands every sealed page back to the guest, so
// the region's next checkpoint takes them again and nothing the guest wrote is
// lost.
func TestFailedPublicationReturnsTheSealedPagesToTheGuest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)

		source := newFakeSource(2 << 20)
		source.set(0, bytes.Repeat([]byte{3}, 4096))
		h.runtime.ObjectStore().Fail()
		defer h.runtime.ObjectStore().Recover()

		runtime := &fakeRuntime{state: vmmState(4), sources: map[string]volume.DirtySource{"ram": source}}
		ckpt, err := host.Capture(t.Context(), vm, runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("the publication during an outage = %v, want ErrUnavailable", err)
		}
		if retires, published := source.outcome(); retires != 1 || published {
			t.Fatalf("the checkpoint was retired %d times, published=%v; want exactly one retire as abandoned", retires, published)
		}
		if got := vm.Status().Checkpoint; got != (control.Ref{VM: "vm", Sequence: rootSeq(vm)}) {
			t.Fatalf("Checkpoint after the failed publication = %+v, want the root checkpoint", got)
		}
		if _, err := host.State(t.Context(), store, control.Ref{VM: "vm", Sequence: secondSeq(vm)}); err == nil {
			t.Fatal("the abandoned checkpoint is readable")
		}
		// The guest's own volume is untouched: the sealed bytes were never the
		// overlay's, and the region has them back.
		want.check(t, vm, "the VM after a failed publication")
	})
}

// A fork of a running parent starts on the parent's own host: the parent pauses
// for its state capture and the seal, publishes nothing, and goes on running
// while the child reads the sealed pages and diverges at once.
func TestForkOfARunningParentOnTheSameHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 11)
		defer h.close(t.Context())
		manager, _, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		random := rand.New(rand.NewPCG(11, 23))
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		selected := vm.Status().Checkpoint
		workload(t, vm, want, random, 10)

		state := vmmState(5)
		runtime := &fakeRuntime{state: state}
		point, err := host.Seal(t.Context(), vm, runtime)
		if err != nil {
			t.Fatal(err)
		}
		runtime.check(t, "prepare", "resume")
		if point.Parent() != selected {
			t.Fatalf("the fork inherits %v, want the parent's selected %v", point.Parent(), selected)
		}
		at := want.clone()

		fork, restored, err := host.CreateFork(t.Context(), manager, "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, fork)
		if !bytes.Equal(restored, state) {
			t.Fatalf("restored state is %d bytes, want the %d captured", len(restored), len(state))
		}
		forked := at.clone()
		forked.check(t, fork, "the fork of the parent's point")
		if got := vm.Status().Checkpoint; got != selected {
			t.Fatalf("the parent published %v to be forked, want the selected %v", got, selected)
		}

		workload(t, vm, want, random, 10)
		workload(t, fork, forked, random, 10)
		want.check(t, vm, "the source after the fork diverged")
		forked.check(t, fork, "the fork after it diverged")

		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := fork.Status().Checkpoint; got != (control.Ref{VM: "fork", Sequence: rootSeq(fork)}) {
			t.Fatalf("fork Checkpoint = %+v, want the fork's own root index", got)
		}
		forked.check(t, fork, "the fork once it published its root")
		want.check(t, vm, "the source once its child published")
	})
}

// Once the publication lands the checkpoint is a second host's to fork: a fresh
// manager with its own log client and its own page cache restores the same
// VMM state and the same volume bytes out of the object store.
func TestForkAfterPublicationOnASecondHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 13)
		defer h.close(t.Context())
		manager, _, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		workload(t, vm, want, rand.New(rand.NewPCG(13, 29)), 25)

		state := vmmState(2)
		ckpt, err := host.Capture(t.Context(), vm, &fakeRuntime{state: state}, nil)
		if err != nil {
			t.Fatal(err)
		}
		at := want.clone()
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}

		second := h.imageStore(t, h.objects)
		config := h.config()
		config.Store = second
		elsewhere := h.manager(t, config)
		defer elsewhere.Close(t.Context())

		// The second host restores the VMM state from the object store alone.
		published, err := host.State(t.Context(), second, ckpt.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, state) {
			t.Fatalf("state read on the second host is %d bytes, want the %d captured", len(published), len(state))
		}

		// A host that never held the parent rebuilds the point from the
		// published checkpoint alone.
		point, err := elsewhere.Inherit(t.Context(), ckpt.Ref())
		if err != nil {
			t.Fatal(err)
		}
		fork, restored, err := host.CreateFork(t.Context(), elsewhere, "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, fork)
		if len(restored) != 0 {
			t.Fatalf("a point rebuilt from the store carried %d state bytes", len(restored))
		}
		// Checkpointing publishes the fork's own root index, after which every
		// read is served from the second host's store rather than the point.
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		at.check(t, fork, "the fork on the second host")
	})
}

// A fork exists only where it was taken until it publishes its own root index.
// Another host is told the fork's root is missing until then, and can open it
// afterwards.
func TestForkIsPendingElsewhereUntilItsRoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		if err := vm.Volume("ram").Write(t.Context(), 0, []byte("parent")); err != nil {
			t.Fatal(err)
		}
		copy(want["ram"], []byte("parent"))
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

		point, err := host.Seal(t.Context(), vm, &fakeRuntime{state: vmmState(4)})
		if err != nil {
			t.Fatal(err)
		}
		fork, _, err := host.CreateFork(t.Context(), manager, "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, fork)
		forked := want.clone()
		if err := fork.Volume("ram").Write(t.Context(), 4096, []byte("local")); err != nil {
			t.Fatal(err)
		}
		copy(forked["ram"][4096:], []byte("local"))
		forked.check(t, fork, "the fork on the host that took it")

		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		if _, err := elsewhere.Open(t.Context(), "fork"); !errors.Is(err, volume.ErrForkPending) {
			t.Fatalf("using the fork elsewhere = %v, want ErrForkPending", err)
		}

		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := elsewhere.Open(t.Context(), "fork")
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, reopened)
		forked.check(t, reopened, "the fork reopened elsewhere once it published its root")
	})
}

// A parent can be forked again and again. One fork point holds the parent's
// pages until its child has them, so the second fork is a second pause, and
// each child gets its own VMM state and goes its own way.
func TestTwoForksOfOneParentDivergeIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 17)
		defer h.close(t.Context())
		manager, _, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		random := rand.New(rand.NewPCG(17, 31))
		workload(t, vm, want, random, 20)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

		state := vmmState(6)
		point, err := host.Seal(t.Context(), vm, &fakeRuntime{state: state})
		if err != nil {
			t.Fatal(err)
		}
		at := want.clone()
		first, firstState, err := host.CreateFork(t.Context(), manager, "first", point)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, first)
		// The parent's pages are the first child's until it publishes them.
		if _, err := host.Seal(t.Context(), vm, &fakeRuntime{state: state}); !errors.Is(err, volume.ErrSealed) {
			t.Fatalf("a second seal while a fork point holds the parent = %v, want ErrSealed", err)
		}
		if err := first.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

		next, err := host.Seal(t.Context(), vm, &fakeRuntime{state: state})
		if err != nil {
			t.Fatal(err)
		}
		second, secondState, err := host.CreateFork(t.Context(), manager, "second", next)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, second)
		if !bytes.Equal(firstState, state) || !bytes.Equal(secondState, state) {
			t.Fatal("the two forks did not restore the captured VMM state")
		}

		firstModel, secondModel := at.clone(), want.clone()
		for range 3 {
			workload(t, vm, want, random, 8)
			workload(t, first, firstModel, random, 8)
			workload(t, second, secondModel, random, 8)
		}
		if err := first.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := second.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		want.check(t, vm, "the source")
		firstModel.check(t, first, "the first fork")
		secondModel.check(t, second, "the second fork")
	})
}

// A fork is an ordinary VM: it can be captured and forked again, and the second
// generation carries its own VMM state and its own bytes.
func TestForkOfAFork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 19)
		defer h.close(t.Context())
		manager, store, _ := held(t, h)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer closeVM(t, vm)
		random := rand.New(rand.NewPCG(19, 37))
		workload(t, vm, want, random, 20)

		parentState := vmmState(1)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := host.Seal(t.Context(), vm, &fakeRuntime{state: parentState})
		if err != nil {
			t.Fatal(err)
		}
		child, restored, err := host.CreateFork(t.Context(), manager, "child", point)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, child)
		if !bytes.Equal(restored, parentState) {
			t.Fatal("the child did not restore the captured VMM state")
		}
		childModel := want.clone()
		workload(t, child, childModel, random, 15)

		// A fork whose root is not published yet has no checkpoint of its own to
		// be forked from: its first checkpoint is what makes it an ordinary VM.
		childState := vmmState(200)
		if _, err := host.Seal(t.Context(), child, &fakeRuntime{state: childState}); !errors.Is(err, volume.ErrForkPending) {
			t.Fatalf("sealing a fork before its root = %v, want ErrForkPending", err)
		}
		childCheckpoint, err := host.Capture(t.Context(), child, &fakeRuntime{state: childState}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if childCheckpoint.Ref() != (control.Ref{VM: "child", Sequence: rootSeq(child)}) {
			t.Fatalf("the child's checkpoint = %+v, want the child's own root index", childCheckpoint.Ref())
		}
		if err := childCheckpoint.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		at := childModel.clone()

		childPoint, err := host.Seal(t.Context(), child, &fakeRuntime{state: childState})
		if err != nil {
			t.Fatal(err)
		}
		grandchild, childRestored, err := host.CreateFork(t.Context(), manager, "grandchild", childPoint)
		if err != nil {
			t.Fatal(err)
		}
		defer closeVM(t, grandchild)
		if !bytes.Equal(childRestored, childState) {
			t.Fatal("the grandchild did not restore the child's captured VMM state")
		}
		grandchildModel := at.clone()
		workload(t, grandchild, grandchildModel, random, 10)
		workload(t, child, childModel, random, 10)

		if err := grandchild.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		grandchildModel.check(t, grandchild, "the grandchild")
		childModel.check(t, child, "the child")
		want.check(t, vm, "the source")

		published, err := host.State(t.Context(), store, childCheckpoint.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, childState) {
			t.Fatalf("the child's published state is %d bytes, want %d", len(published), len(childState))
		}
	})
}
