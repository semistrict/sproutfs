package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// checkpointFixture is one VM of four pages over a store a test configures.
const checkpointPages = 4

func checkpointFixture(t *testing.T, config checkpoint.Config, vm string) (*checkpoint.Store, *checkpoint.Index, *model) {
	t.Helper()
	store := mustStore(t, config)
	sizes := map[string]uint64{"root": checkpointPages * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: vm, Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	return store, root, newModel(volumes2MiB(sizes))
}

// write marks one page changed with contents nothing else repeats, so a read
// that returns another page's bytes is not mistaken for a correct one.
func write(p *checkpoint.Publication, m *model, tag string, page uint64) {
	for storage := range uint32(sectorsPerPage) {
		m.dirty(p, "root", page, storage, sectorData(tag, page, storage))
	}
}

// objectsUnder counts the objects one checkpoint holds in the store — its index
// object and its parts — which is what says whether a sweep took it.
func objectsUnder(t *testing.T, objects platform.ObjectStore, vm string, sequence uint64) int {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(
		"deployment/vm/" + vm + "/ckpt/" + strconv.FormatUint(sequence, 10) + "/")
	if err != nil {
		t.Fatal(err)
	}
	count, token := 0, ""
	for {
		page, err := objects.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Objects)
		if page.NextContinuationToken == "" {
			return count
		}
		token = page.NextContinuationToken
	}
}

// openable reports whether one checkpoint's index object is still there, which
// is exactly whether the checkpoint is published.
func openable(t *testing.T, objects platform.ObjectStore, vm string, sequence uint64) bool {
	t.Helper()
	return present(t, objects, indexKey(t, vm, sequence))
}

// A checkpoint reopened out of its index object is the one that was published,
// whatever the part size: the root addresses the segments it wrote and the ones
// earlier checkpoints' index objects hold, and those locate members of the
// parts it wrote and of the ones it inherits.
func TestAReopenedCheckpointIsTheOnePublished(t *testing.T) {
	for _, partBytes := range []int{0, 1} {
		t.Run(fmt.Sprintf("partBytes=%d", partBytes), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				objects := sim.New(sim.Config{}).ObjectStore()
				store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects, PartBytes: partBytes}, "reopened")

				base := store.Begin(root, control.Ref{VM: "reopened", Sequence: 2})
				for page := range uint64(checkpointPages) {
					write(base, m, "base", page)
				}
				base.SetState([]byte("registers and devices"))
				parent, err := base.Commit(t.Context(), m)
				if err != nil {
					t.Fatal(err)
				}
				// The second checkpoint writes only what it changed, so reopening
				// it reads the rest through the checkpoint it inherits.
				child := store.Begin(parent, control.Ref{VM: "reopened", Sequence: 3})
				write(child, m, "child", 1)
				index, err := child.Commit(t.Context(), m)
				if err != nil {
					t.Fatal(err)
				}

				reopenedParent, err := store.Open(t.Context(), parent.Ref())
				if err != nil {
					t.Fatal(err)
				}
				requireSameIndex(t, reopenedParent, parent)
				reopened, err := store.Open(t.Context(), index.Ref())
				if err != nil {
					t.Fatal(err)
				}
				requireSameIndex(t, reopened, index)
				checkRead(t, store, reopened, m)
				state, err := store.ReadState(t.Context(), reopenedParent)
				if err != nil || string(state) != "registers and devices" {
					t.Fatalf("state through the reopened checkpoint: %q, %v", state, err)
				}
			})
		})
	}
}

func requireSameIndex(t *testing.T, got, want *checkpoint.Index) {
	t.Helper()
	gotBytes, err := checkpoint.IndexBytes(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := checkpoint.IndexBytes(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("index %v differs from %v", got.Ref(), want.Ref())
	}
}

// A checkpoint bigger than one part fills parts in order and reads back from
// every one of them.
func TestMultiPartCheckpointReadsEveryMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects, PartBytes: 1}, "wide")
		p := store.Begin(root, control.Ref{VM: "wide", Sequence: 2})
		p.SetState([]byte("state"))
		for page := range uint64(checkpointPages) {
			write(p, m, "wide", page)
		}
		index, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		// One part per member: the state and each of the four pages. The segment
		// of page table that locates them is in the index object beside them.
		if got, want := objectsUnder(t, objects, "wide", 2), checkpointPages+2; got != want {
			t.Fatalf("the checkpoint wrote %d objects, want %d", got, want)
		}
		checkRead(t, store, index, m)
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, reopened, m)
		if state, err := store.ReadState(t.Context(), reopened); err != nil || string(state) != "state" {
			t.Fatalf("state of a multi-part checkpoint: %q, %v", state, err)
		}
	})
}

// A retry of one publication must write the same bytes, because every object is
// create-if-absent and the store compares a part it finds already there.
func TestRetriedPublicationIsByteIdentical(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "retry")
		p := store.Begin(root, control.Ref{VM: "retry", Sequence: 2})
		p.SetState([]byte("state"))
		for page := range uint64(checkpointPages) {
			write(p, m, "retry", page)
		}
		index, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		before := readWhole(t, objects, indexKey(t, "retry", 2))
		retried, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatalf("retry of an identical publication: %v", err)
		}
		requireSameIndex(t, retried, index)
		if after := readWhole(t, objects, indexKey(t, "retry", 2)); !bytes.Equal(before, after) {
			t.Fatal("the retry wrote a different index object")
		}
	})
}

func readWhole(t *testing.T, objects platform.ObjectStore, key platform.ObjectKey) []byte {
	t.Helper()
	data, _, err := platform.ReadObject(t.Context(), objects, key, 0, 1<<30, errors.New("unreadable object"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Reclamation deletes exactly the checkpoints the current index no longer reads
// from, whenever they became dead — including one the checkpoint being replaced
// was still reading, which the per-object rule could never reach.
func TestReclamationDeletesEveryCheckpointNothingReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "dead")
		publish := func(parent *checkpoint.Index, sequence uint64, pages ...uint64) *checkpoint.Index {
			t.Helper()
			p := store.Begin(parent, control.Ref{VM: "dead", Sequence: sequence})
			for _, page := range pages {
				write(p, m, "seq"+strconv.FormatUint(sequence, 10), page)
			}
			index, err := p.Commit(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			return index
		}
		second := publish(root, 2, 0, 1, 2, 3)
		third := publish(second, 3, 0)
		// Nothing is dead yet: the third still reads three pages of the second,
		// which is too many for compaction to rewrite them.
		if err := store.Reclaim(t.Context(), second, third, nil); err != nil {
			t.Fatal(err)
		}
		if !openable(t, objects, "dead", 2) {
			t.Fatal("a checkpoint the current index still reads was deleted")
		}
		// A checkpoint that writes nothing keeps reading both of them.
		fourth := publish(third, 4)
		if err := store.Reclaim(t.Context(), third, fourth, nil); err != nil {
			t.Fatal(err)
		}
		if !openable(t, objects, "dead", 2) || !openable(t, objects, "dead", 3) {
			t.Fatal("an empty checkpoint reclaimed the checkpoints it inherited")
		}
		// Repacking the rest leaves nothing reading the second checkpoint,
		// which is two selections behind by now.
		fifth := publish(fourth, 5, 1, 2, 3)
		if err := store.Reclaim(t.Context(), fourth, fifth, nil); err != nil {
			t.Fatal(err)
		}
		if got := objectsUnder(t, objects, "dead", 2); got != 0 {
			t.Fatalf("the checkpoint nothing reads kept %d objects", got)
		}
		if got := objectsUnder(t, objects, "dead", 4); got != 0 {
			t.Fatalf("a dead checkpoint kept %d objects", got)
		}
		if !openable(t, objects, "dead", 3) {
			t.Fatal("the checkpoint holding page 0 was deleted with the dead ones")
		}
		checkRead(t, store, fifth, m)
	})
}

// A pinned checkpoint and every checkpoint its index reads are spared, because
// a fork reads through them.
func TestReclamationSparesWhatAPinProtects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "pinned")
		first := store.Begin(root, control.Ref{VM: "pinned", Sequence: 2})
		for page := range uint64(3) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		forked := m.clone()
		p := store.Begin(second, control.Ref{VM: "pinned", Sequence: 3})
		write(p, forked, "third", 0)
		third, err := p.Commit(t.Context(), forked)
		if err != nil {
			t.Fatal(err)
		}
		// A fork of the third checkpoint pins it, so both it and the second,
		// whose parts it reads pages 1 and 2 from, must survive. The fourth
		// rewrites every page the second published, so nothing it holds is read
		// through either of them any more.
		later := forked.clone()
		q := store.Begin(third, control.Ref{VM: "pinned", Sequence: 4})
		for page := range uint64(3) {
			write(q, later, "fourth", page)
		}
		fourth, err := q.Commit(t.Context(), later)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), third, fourth, []uint64{3}); err != nil {
			t.Fatal(err)
		}
		for _, sequence := range []uint64{2, 3} {
			if !openable(t, objects, "pinned", sequence) {
				t.Fatalf("what the pin protects lost checkpoint %d", sequence)
			}
		}
		// The fork reads its whole view through the spared checkpoints.
		pinnedIndex, err := store.Open(t.Context(), third.Ref())
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, store, pinnedIndex, forked)
		// Without the pin the same reclamation takes the second, which nothing
		// reads any more.
		if err := store.Reclaim(t.Context(), third, fourth, nil); err != nil {
			t.Fatal(err)
		}
		if objectsUnder(t, objects, "pinned", 2) != 0 || objectsUnder(t, objects, "pinned", 3) != 0 {
			t.Fatal("unpinned dead checkpoints survived")
		}
		checkRead(t, store, fourth, later)
	})
}

// A checkpoint most of whose members are dead has them rewritten into the
// checkpoint being published, so a cold page cannot keep an otherwise empty one
// alive.
func TestCompactionRewritesMostlyDeadPacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "cold")
		first := store.Begin(root, control.Ref{VM: "cold", Sequence: 2})
		for page := range uint64(checkpointPages) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}

		// One page of four rewritten leaves three quarters of the parts live,
		// which is not worth rewriting.
		p := store.Begin(second, control.Ref{VM: "cold", Sequence: 3})
		write(p, m, "third", 0)
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(third.Checkpoints(), second.Ref()) {
			t.Fatal("a checkpoint still mostly live was rewritten")
		}
		checkRead(t, store, third, m)

		// Three of four leaves a quarter live, and the cold page moves into the
		// new checkpoint's parts, so nothing reads the old ones.
		q := store.Begin(third, control.Ref{VM: "cold", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(fourth.Checkpoints(), second.Ref()) {
			t.Fatalf("a mostly dead checkpoint was kept: %v", fourth.Checkpoints())
		}
		checkRead(t, store, fourth, m)
		// Reclamation leaves it for one checkpoint, because a reader holding
		// the view the fourth replaced still reads through it.
		if err := store.Reclaim(t.Context(), third, fourth, nil); err != nil {
			t.Fatal(err)
		}
		if !openable(t, objects, "cold", 2) {
			t.Fatal("a checkpoint the replaced view still reads was deleted")
		}
		checkRead(t, store, fourth, m)

		// A pinned checkpoint is left alone however dead it is.
		pinned := store.Begin(fourth, control.Ref{VM: "cold", Sequence: 5})
		pinned.Protect([]uint64{fourth.Ref().Sequence})
		write(pinned, m, "fifth", 0)
		write(pinned, m, "fifth", 1)
		write(pinned, m, "fifth", 2)
		fifth, err := pinned.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fifth.Checkpoints(), fourth.Ref()) {
			t.Fatal("compaction rewrote a pinned checkpoint")
		}
		// The fifth no longer names the checkpoint the fourth emptied, so its
		// grace is over and this sweep takes it, which is what compaction exists
		// for.
		if err := store.Reclaim(t.Context(), fourth, fifth, nil); err != nil {
			t.Fatal(err)
		}
		if objectsUnder(t, objects, "cold", 2) != 0 {
			t.Fatal("the compacted checkpoint was not reclaimed")
		}
		checkRead(t, store, fifth, m)
	})
}

// refusingIndexObjects fails every delete of a checkpoint's index object, which
// is what a sweep meets when object storage is unwell part way through one.
type refusingIndexObjects struct {
	platform.ObjectStore
}

func (s refusingIndexObjects) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if strings.HasSuffix(request.Key.String(), "/index") {
		return errors.New("object storage refused the delete")
	}
	return s.ObjectStore.Delete(ctx, request)
}

// A checkpoint's index object goes first, because while it is there the
// checkpoint is still openable and nothing it names may go out from under a
// reader of it. A sweep that cannot delete that object therefore deletes
// nothing else: what it leaves is the whole checkpoint, which a repeat can
// sweep again.
func TestReclamationLeavesACheckpointWhoseIndexObjectWillNotDelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := refusingIndexObjects{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "stuck")
		first := store.Begin(root, control.Ref{VM: "stuck", Sequence: 2})
		for page := range uint64(checkpointPages) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		p := store.Begin(second, control.Ref{VM: "stuck", Sequence: 3})
		for page := range uint64(checkpointPages) {
			write(p, m, "third", page)
		}
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), second, third, nil); err == nil {
			t.Fatal("a sweep whose deletes all failed reported success")
		}
		if !openable(t, objects, "stuck", 2) {
			t.Fatal("the fixture expected the refused index object to still be there")
		}
		if _, err := store.Open(t.Context(), second.Ref()); err != nil {
			t.Fatalf("a checkpoint the sweep could not delete no longer opens: %v", err)
		}
	})
}

// A pinned index names the checkpoints its own compaction emptied as well as
// the ones it reads. A sweep must leave those alone too: what a pin promises is
// that what it protects is whole — the checkpoint, and every checkpoint its
// index names — because the only thing that can tell whether an object under it
// is still read is a collector that can see every fork, and the VM running the
// sweep is not one. An index that names a checkpoint nothing can fetch is a hole
// in what a fork inherits, which is what volume.CheckDeployment reports.
func TestReclamationSparesTheCheckpointsAPinnedIndexOnlyNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "named")
		first := store.Begin(root, control.Ref{VM: "named", Sequence: 2})
		for page := range uint64(checkpointPages) {
			write(first, m, "first", page)
		}
		second, err := first.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		p := store.Begin(second, control.Ref{VM: "named", Sequence: 3})
		write(p, m, "third", 0)
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		// The fourth's compaction takes the last live pages out of the second's
		// parts, so the fourth no longer reads it and still names it: that is the
		// grace the reader of the view it replaced is given.
		q := store.Begin(third, control.Ref{VM: "named", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(fourth.Checkpoints(), second.Ref()) {
			t.Fatalf("the fixture expected the second's parts to be emptied: %v", fourth.Checkpoints())
		}
		// A fork of the fourth pins it. The fifth then sweeps, and the grace the
		// fourth gave the second is over — but the pin is not: the fourth still
		// names that checkpoint, so it stays for as long as the pin does.
		r := store.Begin(fourth, control.Ref{VM: "named", Sequence: 5})
		write(r, m, "fifth", 0)
		fifth, err := r.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Reclaim(t.Context(), fourth, fifth, []uint64{4}); err != nil {
			t.Fatal(err)
		}
		if !openable(t, objects, "named", 2) {
			t.Fatal("the sweep took a checkpoint the pinned one names")
		}
		if !openable(t, objects, "named", 4) {
			t.Fatal("the sweep took the pinned checkpoint itself")
		}
		checkRead(t, store, fifth, m)
	})
}
