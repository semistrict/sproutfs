package checkpoint_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// An ephemeral volume is a name, a size and a page size in every root, and
// nothing else: a page of it is refused, the checkpoints after the root keep the
// marker, and a reopened checkpoint reports it with no page at all.
func TestAnEphemeralVolumeIsRecordedButNeverPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		specs := map[string]checkpoint.VolumeSpec{
			"root":    {Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
			"scratch": {Size: 3 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB, Ephemeral: true},
		}
		root, err := store.Root(t.Context(), control.Ref{VM: "boxed", Sequence: 1}, specs)
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(specs)

		refused := store.Begin(root, control.Ref{VM: "boxed", Sequence: 2})
		m.dirty(refused, "scratch", 1, 0, sectorData("boxed", 1, 0))
		if _, err := refused.Commit(t.Context(), m); !errors.Is(err, checkpoint.ErrEphemeral) {
			t.Fatalf("a page of the ephemeral volume committed with %v, want ErrEphemeral", err)
		}

		next := store.Begin(root, control.Ref{VM: "boxed", Sequence: 3})
		m.dirty(next, "root", 0, 0, sectorData("boxed", 0, 0))
		if _, err := next.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(t.Context(), control.Ref{VM: "boxed", Sequence: 3})
		if err != nil {
			t.Fatal(err)
		}
		if !reopened.Ephemeral("scratch") || reopened.Ephemeral("root") {
			t.Fatalf("the reopened checkpoint marks scratch %v and root %v, want only scratch",
				reopened.Ephemeral("scratch"), reopened.Ephemeral("root"))
		}
		if got := reopened.Size("scratch"); got != 3*checkpoint.PageSize2MiB {
			t.Fatalf("the reopened ephemeral volume is %d bytes, want %d", got, 3*checkpoint.PageSize2MiB)
		}
		extents, err := reopened.Locate(t.Context(), "scratch", 0, 3*checkpoint.PageSize2MiB)
		if err != nil {
			t.Fatal(err)
		}
		for _, extent := range extents {
			if !extent.Identity.Zero {
				t.Fatalf("the ephemeral volume locates %+v, want only zeroes", extent)
			}
		}
		if _, violations := store.CheckIndex(t.Context(), reopened); len(violations) != 0 {
			t.Fatalf("the checkpoint holding an ephemeral volume has violations: %v", violations)
		}
	})
}

// A cold boot adds a disk to a VM that has none: the volume it adds reads as
// zeroes, keeps the marker it was added with, and can be resized both ways,
// because it holds nothing. A name the parent already has is refused.
func TestAPublicationAddsAnEphemeralVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "grown", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		scratch := checkpoint.VolumeSpec{Size: 4 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB,
			Ephemeral: true}

		clash := store.Begin(root, control.Ref{VM: "grown", Sequence: 2})
		clash.Add("root", scratch)
		if _, err := clash.Commit(t.Context(), newModel(volumes2MiB(sizes))); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("adding a volume the parent has committed with %v, want ErrInvalidConfig", err)
		}

		added := store.Begin(root, control.Ref{VM: "grown", Sequence: 3})
		added.Add("scratch", scratch)
		index, err := added.Commit(t.Context(), newModel(volumes2MiB(sizes)))
		if err != nil {
			t.Fatal(err)
		}
		if !index.Ephemeral("scratch") || index.Size("scratch") != scratch.Size {
			t.Fatalf("the added volume is ephemeral %v at %d bytes, want ephemeral at %d",
				index.Ephemeral("scratch"), index.Size("scratch"), scratch.Size)
		}

		shrunk := store.Begin(index, control.Ref{VM: "grown", Sequence: 4})
		shrunk.SetSize("scratch", checkpoint.PageSize2MiB)
		index, err = shrunk.Commit(t.Context(), newModel(volumes2MiB(sizes)))
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if !reopened.Ephemeral("scratch") || reopened.Size("scratch") != checkpoint.PageSize2MiB {
			t.Fatalf("the resized volume is ephemeral %v at %d bytes, want ephemeral at %d",
				reopened.Ephemeral("scratch"), reopened.Size("scratch"), checkpoint.PageSize2MiB)
		}
	})
}
