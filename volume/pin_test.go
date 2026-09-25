package volume_test

import (
	"bytes"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/volume"
)

// forkedParent is a VM of two whole pages with one checkpoint published, ready
// to be forked, together with the writer a test dirties it through.
func forkedParent(t *testing.T, h *harness, manager *volume.Manager, id string) (*volume.VM, func(offset uint64, value byte)) {
	t.Helper()
	vm, err := manager.Create(t.Context(), id, reclaimSpecs)
	if err != nil {
		t.Fatal(err)
	}
	write := func(offset uint64, value byte) {
		t.Helper()
		if err := vm.Volume("root").Write(t.Context(), offset,
			bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
			t.Fatal(err)
		}
	}
	return vm, write
}

// pins reports what one VM's durable record pins, which is what reclamation
// spares.
func pins(t *testing.T, h *harness, vm string) control.Record {
	t.Helper()
	record, err := h.controlClient(t, h.objects).Read(t.Context(), vm)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A grandchild reads pages its grandparent published: its index names the
// grandparent's checkpoints directly, because the child it was forked from had not
// rewritten them. The grandchild pins its own parent's checkpoint and nothing
// above it, so what keeps the grandparent's objects whole is the child's pin — and
// that pin must stand even after the child has rewritten the last page it
// inherited, because the grandchild still reads through it.
func TestAGrandchildKeepsReadingThePageItsGrandparentPublished(t *testing.T) {
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
		grandparent := point.Parent()
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		rewrite := func(who *volume.VM, offset uint64, value byte) {
			t.Helper()
			if err := who.Volume("root").Write(t.Context(), offset,
				bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
				t.Fatal(err)
			}
			if err := who.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		// The child rewrites page zero, so page one still comes from the
		// grandparent's parts, and the grandchild forked here inherits both.
		rewrite(child, 0, 3)
		childPoint, err := child.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		grandchild, err := manager.Fork(t.Context(), "grandchild", childPoint)
		if err != nil {
			t.Fatal(err)
		}
		rewrite(grandchild, 0, 4)
		own, err := h.imageStore(t, h.objects).Open(t.Context(), grandchild.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(own.Checkpoints(), grandparent) {
			t.Fatalf("the grandchild's index names %v, want the grandparent's %v", own.Checkpoints(), grandparent)
		}

		// The child rewrites the last page it inherited, so its own index names
		// none of the grandparent's checkpoints — and the grandchild's still does.
		rewrite(child, checkpoint.PageSize2MiB, 5)
		if record := pins(t, h, "vm"); !record.IsPinned(grandparent.Sequence) {
			t.Fatalf("the grandparent stopped pinning %d, which its grandchild reads", grandparent.Sequence)
		}
		// Two checkpoints of the grandparent, so the checkpoint the child was
		// forked at is one a sweep would otherwise have reclaimed.
		for _, value := range []byte{6, 7} {
			write(0, value)
			write(checkpoint.PageSize2MiB, value)
			if err := vm.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for _, open := range []*volume.VM{grandchild, child, vm} {
			if err := open.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		readsPage(t, h, "grandchild", 0, 4)
		readsPage(t, h, "grandchild", checkpoint.PageSize2MiB, 2)
	})
}

// A pin is one link of a chain: a grandchild pins its own parent's checkpoint,
// and what the grandparent keeps is the child's pin. Deleting the grandchild
// takes neither away: the objects both pins protect are still there for it to
// have been read from, and only a collector can say otherwise.
func TestAPinChainsThroughAForkOfAFork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		vm, write := forkedParent(t, h, manager, "vm")
		defer vm.Close(t.Context())
		write(0, 1)
		write(checkpoint.PageSize2MiB, 2)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		grandparent := point.Parent()
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close(t.Context())
		// The child rewrites one of the two pages, so its own checkpoint holds a
		// checkpoint the grandchild reads and the other page still comes from the
		// grandparent's.
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
		parent := childPoint.Parent()
		grandchild, err := manager.Fork(t.Context(), "grandchild", childPoint)
		if err != nil {
			t.Fatal(err)
		}
		defer grandchild.Close(t.Context())
		if err := grandchild.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		root, err := h.imageStore(t, h.objects).Open(t.Context(), grandchild.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(root.Checkpoints(), parent) {
			t.Fatalf("the grandchild's root names %v, want its parent's %v", root.Checkpoints(), parent)
		}
		if !pins(t, h, "child").IsPinned(parent.Sequence) {
			t.Fatalf("the child does not pin %d for the grandchild that reads it", parent.Sequence)
		}
		if !pins(t, h, "vm").IsPinned(grandparent.Sequence) {
			t.Fatalf("the grandparent does not pin %d for the child that reads it", grandparent.Sequence)
		}

		// Deleting the grandchild leaves both links where they were.
		if err := manager.Delete(t.Context(), "grandchild"); err != nil {
			t.Fatal(err)
		}
		if !pins(t, h, "child").IsPinned(parent.Sequence) {
			t.Fatalf("deleting the grandchild released the pin on %d", parent.Sequence)
		}
		if !pins(t, h, "vm").IsPinned(grandparent.Sequence) {
			t.Fatalf("deleting the grandchild released the child's pin on %d", grandparent.Sequence)
		}
	})
}

// One point forked into several children is one pin, so the end of one child
// leaves what the others read exactly where it is.
func TestOneChildsDeletionLeavesWhatItsSiblingsRead(t *testing.T) {
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
		point, err := vm.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		pinned := point.Parent()
		// The fan-out holds the point until every child of it has been taken,
		// exactly as a host does: each child's own hold goes when it closes, and
		// a point whose last hold has gone has given the parent its pages back
		// and may not be forked from again.
		if err := point.Hold(); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			fork, err := manager.Fork(t.Context(), id, point)
			if err != nil {
				t.Fatal(err)
			}
			if err := fork.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := fork.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "first"); err != nil {
			t.Fatal(err)
		}
		if !pins(t, h, "vm").IsPinned(pinned.Sequence) {
			t.Fatalf("deleting one of two children released %d, which the other reads", pinned.Sequence)
		}
		if got := pins(t, h, "vm").Pinned; len(got) != 1 {
			t.Fatalf("the fan-out left the parent pinning %v, want one pin for the point", got)
		}

		// The parent rewrites every page it published, so only the surviving
		// child's pin keeps that checkpoint whole — and it must, because the
		// child's index names it.
		write(0, 3)
		write(checkpoint.PageSize2MiB, 4)
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, h, store, pinned); !slices.Equal(got, []string{"index", "part/0"}) {
			t.Fatalf("the checkpoint the surviving child reads kept %v", got)
		}
		elsewhere := h.manager(t, h.config())
		defer elsewhere.Close(t.Context())
		opened, err := elsewhere.Open(t.Context(), "second")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close(t.Context())
		got := make([]byte, checkpoint.SectorSize)
		if err := opened.Volume("root").Read(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{1}, checkpoint.SectorSize)) {
			t.Fatalf("the surviving child read %d..., want the checkpoint it inherited", got[0])
		}
	})
}
