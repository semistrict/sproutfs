package checkpoint_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// A publication may drop the state it would otherwise inherit, which is what a
// cold boot's does: the memory that state was captured over is being discarded
// in the same publication, and a checkpoint holding one without the other is an
// moment that never existed.
func TestDroppingTheInheritedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "cold")
		captured := store.Begin(root, control.Ref{VM: "cold", Sequence: 2})
		captured.SetState([]byte("registers and devices"))
		write(captured, m, "captured", 0)
		parent, err := captured.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}

		cold := store.Begin(parent, control.Ref{VM: "cold", Sequence: 3})
		cold.DropState()
		write(cold, m, "cold", 1)
		index, err := cold.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if index.HasState() {
			t.Fatal("a checkpoint that dropped its state still names one")
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadState(t.Context(), reopened); !errors.Is(err, checkpoint.ErrNoState) {
			t.Fatalf("reading the state of a cold boot's checkpoint = %v, want %v", err, checkpoint.ErrNoState)
		}
		requireSameIndex(t, reopened, index)
	})
}

// State of its own is what a publication ends up with whichever order the two
// are asked for in: a cold boot drops the parent's and a capture attaches one,
// and nothing does both at once.
func TestStateOfItsOwnOverridesADroppedOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "cold")
		publication := store.Begin(root, control.Ref{VM: "cold", Sequence: 2})
		publication.DropState()
		publication.SetState([]byte("its own"))
		write(publication, m, "own", 0)
		index, err := publication.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		state, err := store.ReadState(t.Context(), index)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "its own" {
			t.Fatalf("the checkpoint holds state %q, want its own", state)
		}
	})
}
