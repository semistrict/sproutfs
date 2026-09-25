package checkpoint_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// countingStore counts the bytes a store reads back out of object storage.
type countingStore struct {
	platform.ObjectStore
	gets  atomic.Int64
	bytes atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := s.ObjectStore.Get(ctx, request)
	if err == nil {
		s.gets.Add(1)
		s.bytes.Add(result.ContentLength)
	}
	return result, err
}

// Retrying a publication that already landed is idempotent, and settling that
// must not cost what the publication cost: the object's own metadata says what
// it holds, so a retry compares a digest rather than downloading every part it
// wrote.
func TestRetriedPublicationComparesWithoutDownloading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := &countingStore{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "idempotent")
		p := store.Begin(root, control.Ref{VM: "idempotent", Sequence: 2})
		p.SetState([]byte("registers and devices"))
		for page := range uint64(checkpointPages) {
			write(p, m, "base", page)
		}
		index, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}

		objects.gets.Store(0)
		objects.bytes.Store(0)
		retried, err := p.Commit(t.Context(), m)
		if err != nil {
			t.Fatalf("retry of an identical publication: %v", err)
		}
		requireSameIndex(t, retried, index)
		if got := objects.gets.Load(); got != 0 {
			t.Fatalf("the retry read %d objects back, %d bytes, to settle that it was idempotent",
				got, objects.bytes.Load())
		}
	})
}

// A reference reused for other contents is still a conflict, settled from the
// same metadata.
func TestRepublishingOtherContentsUnderOneRefConflicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store, root, m := checkpointFixture(t, checkpoint.Config{ObjectStore: objects}, "clash")
		first := store.Begin(root, control.Ref{VM: "clash", Sequence: 2})
		write(first, m, "first", 0)
		if _, err := first.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		other := store.Begin(root, control.Ref{VM: "clash", Sequence: 2})
		write(other, m, "other", 1)
		if _, err := other.Commit(t.Context(), m); err == nil {
			t.Fatal("a reference reused for other contents was accepted")
		} else if !errors.Is(err, checkpoint.ErrConflict) {
			t.Fatalf("reusing a reference: %v", err)
		}
	})
}
