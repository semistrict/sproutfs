package volume_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// reclaimSpecs is one volume of two whole pages, so a test can leave one page
// untouched while it rewrites the other.
var reclaimSpecs = []volume.VolumeSpec{{Name: "root", Size: 2 * checkpoint.PageSize}}

// objectsUnder lists the objects one checkpoint still has in the store, by the
// suffix that follows its own prefix, in ascending order.
func objectsUnder(t *testing.T, h *harness, store platform.ObjectStore, ref control.Ref) []string {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(fmt.Sprintf("%svm/%s/ckpt/%d/", h.prefix.String(), ref.VM, ref.Sequence))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	token := ""
	for {
		page, err := store.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			names = append(names, strings.TrimPrefix(object.Key.String(), prefix.String()))
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.Sort(names)
	return names
}

// rewritingManager builds a manager over the harness's object store. Every
// publication republishes the pages it touched whole, which is what leaves the
// checkpoint before it holding objects nothing reads any more.
func rewritingManager(t *testing.T, h *harness) (*volume.Manager, platform.ObjectStore) {
	t.Helper()
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: h.objects, ObjectPrefix: h.prefix})
	if err != nil {
		t.Fatal(err)
	}
	config := h.config()
	config.Store = store
	return h.manager(t, config), h.objects
}

// Selecting a checkpoint reclaims the one it replaced: the objects the new
// index no longer references go, and the ones it still references stay.
func TestPublishingReclaimsWhatItReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		want := newModel(reclaimSpecs)
		root := vm.Volume("root")
		write := func(offset uint64, value byte) {
			t.Helper()
			page := bytes.Repeat([]byte{value}, checkpoint.SectorSize)
			if err := root.Write(t.Context(), offset, page); err != nil {
				t.Fatal(err)
			}
			copy(want["root"][offset:], page)
		}

		// The first checkpoint writes both pages.
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := vm.Status().Checkpoint
		if got := objectsUnder(t, h, store, first); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the first checkpoint published %v", got)
		}

		// The second repacks page 0 and inherits page 1, which still lives in
		// the first checkpoint's parts, so that whole checkpoint stays.
		write(0, 3)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		second := vm.Status().Checkpoint
		if got := objectsUnder(t, h, store, second); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the second checkpoint published %v", got)
		}
		if got := objectsUnder(t, h, store, first); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the first checkpoint lost %v while the second still read its parts", got)
		}
		want.check(t, vm, "after the second checkpoint")

		// Repacking both pages leaves nothing reading either earlier
		// checkpoint, and the selection deletes them whole.
		write(0, 4)
		write(checkpoint.PageSize, 4)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, first); len(got) != 0 {
			t.Fatalf("a checkpoint nothing reads kept %v", got)
		}
		if got := objectsUnder(t, h, store, second); len(got) != 0 {
			t.Fatalf("the replaced checkpoint kept %v", got)
		}
		want.check(t, vm, "after the replaced checkpoints were reclaimed")

		// Nothing reclaims the checkpoint a handle opened on, because a handle
		// cannot account for what the writer before it left behind.
		write(0, 4)
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		want.check(t, reopened, "reopened after reclamation")
		opened := reopened.Status().Checkpoint
		if opened == second {
			t.Fatalf("the close published nothing: still at %v", opened)
		}
		inheritedObjects := objectsUnder(t, h, store, opened)
		page := bytes.Repeat([]byte{5}, checkpoint.SectorSize)
		if err := reopened.Volume("root").Write(t.Context(), 0, page); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], page)
		if err := reopened.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, opened); !slices.Equal(got, inheritedObjects) {
			t.Fatalf("a new handle reclaimed the checkpoint it opened on: %v, want %v", got, inheritedObjects)
		}
		want.check(t, reopened, "after the reopened handle published")
	})
}

// A checkpoint a fork inherited is pinned in the parent's control record, and
// reclamation leaves it whole — objects and index alike — because the fork's
// own lineage runs through it.
func TestReclamationSparesAPinnedCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		root := vm.Volume("root")
		write := func(value byte) {
			t.Helper()
			if err := root.Write(t.Context(), 0, bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
				t.Fatal(err)
			}
		}
		write(1)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		write(2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
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
		pinned := point.Parent()
		record, err := h.controlClient(t, h.objects).Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !record.IsPinned(pinned.Sequence) {
			t.Fatalf("the parent's record pins %v, want the forked %d", record.Pinned, pinned.Sequence)
		}
		before := objectsUnder(t, h, store, pinned)
		if !slices.Equal(before, []string{"index", "part/0"}) {
			t.Fatalf("the forked checkpoint published %v", before)
		}

		// Two more publications, so the pinned checkpoint is the one the next
		// selection would otherwise reclaim.
		write(3)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, pinned); !slices.Equal(got, before) {
			t.Fatalf("the pinned checkpoint lost %v", got)
		}
		// The fork reads through the pinned lineage on a host that never held it.
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		opened, err := elsewhere.Open(t.Context(), "fork")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close(t.Context())
		got := make([]byte, checkpoint.SectorSize)
		if err := opened.Volume("root").Read(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{2}, checkpoint.SectorSize)) {
			t.Fatalf("the fork read %d..., want the pinned checkpoint's 2...", got[0])
		}
	})
}

// The pin comes before the fork exists: a fork whose own control record cannot
// be written still leaves its lineage pinned, because a fork that existed while
// its lineage was unpinned could have that lineage reclaimed under it.
func TestForkPinsBeforeTheChildExists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		fault := &faultStore{ObjectStore: h.objects}
		config := h.config()
		config.Control, config.Store = h.controlClient(t, fault), h.imageStore(t, fault)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("parent")); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		pinned := point.Parent()
		// Nothing may write the child's control record.
		fault.block(func(key platform.ObjectKey) bool { return key.String() == h.prefix.String()+"control/fork" })
		if _, err := manager.Fork(t.Context(), "fork", point); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("forking without a child record = %v, want the injected fault", err)
		}
		fault.block(nil)
		record, err := h.controlClient(t, h.objects).Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !record.IsPinned(pinned.Sequence) {
			t.Fatalf("the parent's record pins %v, want the forked %d even though the child was never created",
				record.Pinned, pinned.Sequence)
		}
		if _, err := h.controlClient(t, h.objects).Read(t.Context(), "fork"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the failed fork left a control record: %v", err)
		}
		// Retrying the fork is idempotent in the pin and creates the child.
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(t.Context())
		again, err := h.controlClient(t, h.objects).Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(again.Pinned, []uint64{pinned.Sequence}) {
			t.Fatalf("the retried fork left pins %v", again.Pinned)
		}
	})
}

// Checkpoint sequences are epoch-major, so a takeover publishes under sequences
// no fenced predecessor could ever have used, whatever it was uploading.
func TestSequencesAdvanceWithTheEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		defer vm.Close(t.Context())
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("first")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("first"))
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := vm.Status().Checkpoint
		if first.Sequence != counted(vm, 2) {
			t.Fatalf("the creating handle published %d, want the second sequence of its epoch", first.Sequence)
		}

		next, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close(t.Context())
		if err := next.Volume("root").Write(t.Context(), 4096, []byte("second")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"][4096:], []byte("second"))
		if err := next.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		second := next.Status().Checkpoint
		if second.Sequence != control.Sequence(next.Epoch(), 1) {
			t.Fatalf("the takeover published %d, want the first sequence of epoch %d", second.Sequence, next.Epoch())
		}
		if second.Sequence <= first.Sequence {
			t.Fatalf("the takeover published %d, below its predecessor's %d", second.Sequence, first.Sequence)
		}
		// The fenced handle's next sequence is still inside its own epoch, so it
		// could never name a checkpoint the takeover published.
		if control.EpochOf(first.Sequence) >= control.EpochOf(second.Sequence) {
			t.Fatalf("checkpoints %d and %d share an epoch", first.Sequence, second.Sequence)
		}
		want.check(t, next, "after the takeover published under its own epoch")
	})
}

// A publication whose control-record reply is lost has already selected its
// checkpoint. The writer recognises its own work and carries on rather than
// repeating the upload, and the next open sees that checkpoint.
func TestLostSelectionReplyIsReconciled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		fault := &faultStore{ObjectStore: h.objects}
		store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: fault, ObjectPrefix: h.prefix, Concurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		config := h.config()
		config.Control, config.Store = h.controlClient(t, fault), store
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		vm, want := createVM(t, manager, "vm")
		if err := vm.Volume("root").Write(t.Context(), 0, []byte("selected")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("selected"))
		// One part, then the index object, then the control record: the third
		// write lands and its reply is the one that goes missing.
		fault.arm(3, true)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatalf("a checkpoint whose selection reply was lost: %v", err)
		}
		if !fault.disarm() {
			t.Fatal("the selection was never reached")
		}
		selected := vm.Status().Checkpoint
		if selected.Sequence != counted(vm, 2) {
			t.Fatalf("the reconciled checkpoint is %v", selected)
		}
		want.check(t, vm, "after a lost selection reply")
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		if got := reopened.Status().Checkpoint; got != selected {
			t.Fatalf("the reopened VM is at %v, want the lost selection %v", got, selected)
		}
		want.check(t, reopened, "reopened after a lost selection reply")
	})
}

// A fork that ends before it ever published a root leaves the parent's pin
// standing: the point was forked, and nothing here can establish that the
// lineage started from it went nowhere — the child could have been handed to
// another host, which is exactly how a fork onto another host looks from here.
// The parent's next checkpoint spares that checkpoint, and a collector is what
// eventually gives it back.
func TestAnAbandonedForkLeavesThePinStanding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())
		write := func(value byte) {
			t.Helper()
			for _, offset := range []uint64{0, checkpoint.PageSize} {
				if err := vm.Volume("root").Write(t.Context(), offset,
					bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
					t.Fatal(err)
				}
			}
		}
		write(1)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		pinned := point.Parent()
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}

		// The fork ends before it ever published a root, so it leaves no object
		// of its own behind.
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		record, err := h.controlClient(t, h.objects).Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !record.IsPinned(pinned.Sequence) {
			t.Fatalf("the parent pins %v after the fork it was taken for ended, want %d",
				record.Pinned, pinned.Sequence)
		}

		// The parent's next checkpoint repacks both pages and spares the pinned
		// checkpoint whole.
		write(2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, pinned); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the pinned checkpoint kept %v, want its index object and its part", got)
		}
	})
}

// A fork that published a root over its parent's lineage keeps that lineage
// pinned: its own index names the parent's checkpoints, so releasing the pin would
// let the parent's next checkpoint delete the objects the child reads.
func TestPublishedForkKeepsThePinItsIndexNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())
		write := func(offset uint64, value byte) {
			t.Helper()
			if err := vm.Volume("root").Write(t.Context(), offset,
				bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
				t.Fatal(err)
			}
		}
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		pinned := point.Parent()
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		root, err := h.imageStore(t, h.objects).Open(t.Context(), fork.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(root.Checkpoints(), pinned) {
			t.Fatalf("the fork's root names %v, want the parent's pinned %v", root.Checkpoints(), pinned)
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		record, err := h.controlClient(t, h.objects).Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !record.IsPinned(pinned.Sequence) {
			t.Fatalf("the parent's record pins %v, want the %d its child's root reads",
				record.Pinned, pinned.Sequence)
		}

		// The parent rewrites every page it published, so nothing of its own
		// reads that checkpoint: only the pin keeps the child readable.
		write(0, 3)
		write(checkpoint.PageSize, 3)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, pinned); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the pinned checkpoint the child reads kept %v", got)
		}
		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		opened, err := elsewhere.Open(t.Context(), "fork")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close(t.Context())
		got := make([]byte, checkpoint.SectorSize)
		if err := opened.Volume("root").Read(t.Context(), checkpoint.PageSize, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{2}, checkpoint.SectorSize)) {
			t.Fatalf("the fork read %d..., want the pinned checkpoint's 2...", got[0])
		}
	})
}

// A same-host fork pins the checkpoint its child inherits, and the parent's
// reclamation goes on running around it: the pinned lineage is spared whole,
// and every checkpoint of the parent's own that neither the new index nor a
// child reads is deleted. A sweep skipped because the checkpoint it replaced
// happened to be pinned would leave those behind for good, because a handle
// reclaims each checkpoint it published exactly once.
func TestReclamationRunsAroundAPinnedCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())
		write := func(offset uint64, value byte) {
			t.Helper()
			if err := vm.Volume("root").Write(t.Context(), offset,
				bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
				t.Fatal(err)
			}
		}
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		pinned := point.Parent()
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(t.Context())
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

		// The first checkpoint after the fork replaces the pinned one, which is
		// spared; the second replaces that one, whose own parts nothing reads
		// once both pages have been repacked over it.
		write(0, 3)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		replaced := vm.Status().Checkpoint
		write(0, 4)
		write(checkpoint.PageSize, 5)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, pinned); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the pinned checkpoint the child reads kept %v, want its index object and its part", got)
		}
		if got := objectsUnder(t, h, store, replaced); len(got) != 0 {
			t.Fatalf("the checkpoint published over the pinned one kept %v", got)
		}
	})
}

// A checkpoint's sweep runs behind its publication, so Wait returns before the
// deletes of what the replaced checkpoint no longer needs. Closing the handle
// then must finish that sweep rather than cancel it: a host that closes its VMs
// on the way out is exiting, not leaking.
func TestClosingAHandleFinishesTheSweepBehindItsLastCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, store := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if err != nil {
			t.Fatal(err)
		}
		root := vm.Volume("root")
		write := func(offset uint64, value byte) {
			t.Helper()
			if err := root.Write(t.Context(), offset, bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
				t.Fatal(err)
			}
		}
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		replaced := vm.Status().Checkpoint
		// Both pages repacked leaves nothing reading the first checkpoint, so
		// the publication behind this snapshot sweeps it.
		write(0, 3)
		write(checkpoint.PageSize, 4)
		ckpt, err := vm.Snapshot(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, replaced); len(got) != 0 {
			t.Fatalf("closing right after the checkpoint left the replaced one's %v behind", got)
		}
		if err := ckpt.Swept(t.Context()); err != nil {
			t.Fatalf("the sweep behind the checkpoint: %v", err)
		}
	})
}
