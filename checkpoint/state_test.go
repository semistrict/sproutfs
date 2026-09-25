package checkpoint_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A checkpoint of a running VM carries no VMM state: only a capture pauses the
// guest for one. The VM is still restorable from the state its last capture
// published, so a checkpoint with none inherits the one it replaces rather than
// clearing it.
func TestAStatelessCheckpointInheritsTheParentsState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "inherited")
		captured := store.Begin(root, control.Ref{VM: "inherited", Sequence: 2})
		captured.SetState([]byte("registers and devices"))
		write(captured, m, "captured", 0)
		parent, err := captured.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}

		plain := store.Begin(parent, control.Ref{VM: "inherited", Sequence: 3})
		write(plain, m, "plain", 1)
		index, err := plain.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if !index.HasState() {
			t.Fatal("a checkpoint with no state of its own reports none at all")
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		state, err := store.ReadState(t.Context(), reopened)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "registers and devices" {
			t.Fatalf("inherited state %q", state)
		}
		// The checkpoint the state lives in is the parent, so the root must go
		// on naming it however little else it reads from it.
		if !slices.Contains(index.Checkpoints(), parent.Ref()) {
			t.Fatalf("the root does not name the checkpoint its state lives in: %v", index.Checkpoints())
		}
		requireSameIndex(t, reopened, index)
	})
}

// An inherited state member lives in a checkpoint whose parts can become mostly
// dead like any other, and compaction moves it with the pages.
func TestCompactionMovesAnInheritedStateMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "movedstate")
		captured := store.Begin(root, control.Ref{VM: "movedstate", Sequence: 2})
		captured.SetState([]byte("registers and devices"))
		for page := range uint64(checkpointPages) {
			write(captured, m, "captured", page)
		}
		second, err := captured.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		p := store.Begin(second, control.Ref{VM: "movedstate", Sequence: 3})
		write(p, m, "third", 0)
		third, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		q := store.Begin(third, control.Ref{VM: "movedstate", Sequence: 4})
		write(q, m, "fourth", 1)
		write(q, m, "fourth", 2)
		fourth, err := q.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(fourth.Checkpoints(), second.Ref()) {
			t.Fatalf("compaction left the state behind in a mostly dead checkpoint: %v", fourth.Checkpoints())
		}
		state, err := store.ReadState(t.Context(), fourth)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "registers and devices" {
			t.Fatalf("state after compaction moved it: %q", state)
		}
		checkRead(t, store, fourth, m)
	})
}
