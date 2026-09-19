package volume_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// sealedPages is a pager checkpoint of fixed content, which is what a running
// guest's regions give a checkpoint. It is the only thing that makes Sealed
// non-zero.
type sealedPages struct {
	size  int
	pages []uint64
	fill  byte
	age   time.Duration
}

func (s sealedPages) PageSize() int        { return s.size }
func (s sealedPages) DirtyPages() []uint64 { return s.pages }

func (s sealedPages) ReadDirty(_ context.Context, _ uint64, dst []byte) error {
	for index := range dst {
		dst[index] = s.fill
	}
	return nil
}

// age is how long these pages have been unpublished, which only a fork's
// handoff reads. A seal built by hand holds none of that history.
func (s sealedPages) UnpublishedAge() time.Duration { return s.age }

func (sealedPages) Hold() {}

func (sealedPages) Share(context.Context, control.Ref, string) error { return nil }

func (sealedPages) Retire(context.Context, bool) error { return nil }

// meteredManager builds a manager whose control records and checkpoint objects
// both go through one metered store, which is how a host is assembled.
func meteredManager(t *testing.T, h *harness) (*volume.Manager, *platform.MeteredObjectStore) {
	t.Helper()
	metered, err := platform.NewMeteredObjectStore(h.objects)
	if err != nil {
		t.Fatal(err)
	}
	return h.manager(t, volume.Config{Control: h.controlClient(t, metered),
		Store: h.imageStore(t, metered)}), metered
}

// One checkpoint's traffic is its own: the objects its publication wrote, and
// not those of a checkpoint of another VM running beside it.
func TestACheckpointCountsItsOwnPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, metered := meteredManager(t, h)
		defer manager.Close(t.Context())

		before := metered.Traffic()
		first, _ := createVM(t, manager, "first")
		defer first.Close(t.Context())
		second, _ := createVM(t, manager, "second")
		defer second.Close(t.Context())

		dirtyEveryPage(t, first, newModel(testSpecs), 0x11)
		dirtyEveryPage(t, second, newModel(testSpecs), 0x22)

		// Both publications are in flight together, which is exactly the case a
		// per-checkpoint tally has to survive.
		firstCheckpoint, err := first.Snapshot(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		secondCheckpoint, err := second.Snapshot(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := firstCheckpoint.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := secondCheckpoint.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}

		// One part holding the four dirty pages — three of root and one of
		// state — its index object, and the control record the selection
		// writes. The page count does not enter it: a checkpoint writes its
		// pages into one object however many of them there are.
		firstPuts := firstCheckpoint.Traffic().Put
		if firstPuts.Calls != 3 || firstPuts.Failures != 0 {
			t.Fatalf("the first checkpoint wrote %+v", firstPuts)
		}
		secondPuts := secondCheckpoint.Traffic().Put
		if secondPuts.Calls != 3 || secondPuts.Failures != 0 {
			t.Fatalf("the second checkpoint wrote %+v", secondPuts)
		}
		// The store's totals over the two checkpoints are the two checkpoints' own
		// traffic and the creations that preceded them, never one checkpoint's
		// counted against the other.
		total := metered.Traffic().Put.Calls - before.Put.Calls
		if total < firstPuts.Calls+secondPuts.Calls {
			t.Fatalf("the store saw %d puts, fewer than the %d the two checkpoints claim",
				total, firstPuts.Calls+secondPuts.Calls)
		}
	})
}

// Sealed is the dirty set the pause froze, which is what a checkpoint publishes
// and what the 2 MiB granularity is counted in.
func TestACheckpointReportsTheDirtySetItSealed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := meteredManager(t, h)
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		defer vm.Close(t.Context())

		sealed := sealedPages{size: checkpoint.PageSize2MiB, pages: []uint64{0, 2}, fill: 0x5a}
		ckpt, err := vm.Snapshot(t.Context(), volume.Prepared(nil,
			map[string]volume.DirtySource{"root": sealed}))
		if err != nil {
			t.Fatal(err)
		}
		pages, bytes := ckpt.Sealed()
		if pages != 2 || bytes != 2*checkpoint.PageSize2MiB {
			t.Fatalf("Sealed = %d pages, %d bytes", pages, bytes)
		}
		if err := ckpt.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
