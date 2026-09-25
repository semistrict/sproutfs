package checkpoint_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A checkpoint records the processors a boot of it gives the guest, and every
// checkpoint after it keeps the count until one sets another. A root records
// none, which is the host's default.
func TestACheckpointRecordsItsProcessorsAndItsChildrenKeepThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects})
		sizes := map[string]uint64{"root": checkpoint.PageSize2MiB}
		root, err := store.Root(t.Context(), control.Ref{VM: "shaped", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		if root.VCPUs() != 0 {
			t.Fatalf("a root records %d processors, want none", root.VCPUs())
		}
		m := newModel(volumes2MiB(sizes))
		shaped := store.Begin(root, control.Ref{VM: "shaped", Sequence: 2})
		shaped.SetVCPUs(4)
		index, err := shaped.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(t.Context(), index.Ref())
		if err != nil {
			t.Fatal(err)
		}
		if reopened.VCPUs() != 4 {
			t.Fatalf("the reopened checkpoint records %d processors, want 4", reopened.VCPUs())
		}
		next := store.Begin(reopened, control.Ref{VM: "shaped", Sequence: 3})
		m.dirty(next, "root", 0, 0, sectorData("shaped", 0, 0))
		later, err := next.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if later.VCPUs() != 4 {
			t.Fatalf("the checkpoint after it records %d processors, want the 4 it inherits", later.VCPUs())
		}
		refused := store.Begin(later, control.Ref{VM: "shaped", Sequence: 4})
		refused.SetVCPUs(33)
		if _, err := refused.Commit(t.Context(), m); !errors.Is(err, checkpoint.ErrInvalidConfig) {
			t.Fatalf("a checkpoint of 33 processors committed with %v, want ErrInvalidConfig", err)
		}
	})
}
