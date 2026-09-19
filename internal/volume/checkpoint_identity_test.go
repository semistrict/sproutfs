package volume_test

import (
	"bytes"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
)

func TestCheckpointPreservesPageIdentityBetweenDisjointWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "checkpoint-identity")
		defer vm.Close(t.Context())
		root := vm.Volume("root")
		inherited := bytes.Repeat([]byte{7}, checkpoint.SectorSize)
		if err := root.Write(t.Context(), 2*checkpoint.SectorSize, inherited); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		before, err := root.Locate(t.Context(), 2*checkpoint.SectorSize, checkpoint.SectorSize)
		if err != nil || len(before) != 1 || before[0].Identity.Zero {
			t.Fatalf("initial stored identity = %+v, error %v", before, err)
		}
		// A write in the next page leaves this one's object, and so its
		// identity, exactly where it was.
		if err := root.Write(t.Context(), checkpoint.PageSize2MiB, bytes.Repeat([]byte{9}, checkpoint.SectorSize)); err != nil {
			t.Fatal(err)
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := root.Locate(t.Context(), 2*checkpoint.SectorSize, checkpoint.SectorSize)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("checkpoint rewrote an untouched page's identity: before %+v, after %+v, error %v", before, after, err)
		}
		got := make([]byte, checkpoint.SectorSize)
		if err := root.Read(t.Context(), 2*checkpoint.SectorSize, got); err != nil || !bytes.Equal(got, inherited) {
			t.Fatalf("untouched page bytes changed: %x, error %v", got[:16], err)
		}
	})
}
