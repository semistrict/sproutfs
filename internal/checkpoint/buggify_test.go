package checkpoint_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// A retry of a publication finds the byte-identical parts its own earlier
// attempt wrote and carries on. Giving up on one instead is what a real
// publication does when the store stops answering partway through settling
// that the object it found is its own: the checkpoint fails, and what must
// survive that is the retry after it — the same reference, the same parts and
// the same index, with nothing half published in between.
//
// Seed 1 activates the give-up site and fires it on the first part the retry
// finds already there.
func TestAPublicationThatGivesUpOnItsOwnPartIsStillRetryable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1, Buggify: true})
		ctx := sim.WithRuntime(t.Context(), runtime)
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: runtime.ObjectStore()}, "give-up")
		p := store.Begin(root, control.Ref{VM: "give-up", Sequence: 2})
		p.SetState([]byte("registers and devices"))
		for page := range uint64(checkpointPages) {
			write(p, m, "base", page)
		}
		index, err := p.Commit(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		var retried *checkpoint.Index
		for range 20 {
			if retried, err = p.Commit(ctx, m); err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("twenty retries of one publication all gave up: %v", err)
		}
		if runtime.FiredSites()["checkpoint/give-up-on-existing-part"] == 0 {
			t.Fatal("the give-up site never fired: seed 1 is meant to activate it and fire it on the first part a retry finds")
		}
		// The publication that eventually landed is the one the first attempt
		// meant to publish, not a different checkpoint under the same reference.
		requireSameIndex(t, retried, index)
	})
}
