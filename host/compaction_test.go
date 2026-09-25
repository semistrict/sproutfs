package host_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// Compaction moves a cold page's bytes out of a checkpoint that has become
// mostly dead and into the one being published. The guest running on that
// page never notices: the page keeps its identity, so the resident page
// the pager holds for it stays the page's, and reading it after the move
// costs no fault and no load. Here the checkpoint that first published four
// pages is left a quarter live by the next one, which compacts its one cold
// page away; the checkpoint after that reclaims the emptied one, and the
// guest's read of the cold page is served from the memory it already had.
func TestCompactionLeavesTheGuestsResidentPagesAlone(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = time.Hour
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")

	for page := range uint64(4) {
		guest.store("ram0", page, byte(page+1))
	}
	first := capture(t, vm, guest)
	// Three of the four pages rewritten leaves the first checkpoint a quarter
	// live, which is what makes compaction rewrite the cold page 3.
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+11))
	}
	capture(t, vm, guest)
	// The emptied checkpoint is spared for one more checkpoint, for a reader
	// still holding the view the compacting one replaced; this one reclaims it.
	guest.store("ram0", 0, 21)
	capture(t, vm, guest)
	if remaining := objectsUnder(t, h, first); len(remaining) != 0 {
		t.Fatalf("the compacted checkpoint %d still has objects %v; compaction did not empty it", first, remaining)
	}

	before, err := pagers.ram().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := guest.load("ram0", 3)[0]; got != 4 {
		t.Fatalf("page 3 reads %d after compaction moved it, want the 4 the guest stored", got)
	}
	after, err := pagers.ram().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Faults != before.Faults || after.Loads != before.Loads {
		t.Fatalf("reading the compacted page cost %d faults and %d loads; its resident page should have stayed the page's",
			after.Faults-before.Faults, after.Loads-before.Loads)
	}
	if after.ResidentPages != before.ResidentPages {
		t.Fatalf("resident pages went from %d to %d over a read of a page the guest already held",
			before.ResidentPages, after.ResidentPages)
	}
}

// capture takes a checkpoint of the VM through the guest and waits for its
// publication, where compaction runs, and for the sweep behind it, where
// reclamation runs, and reports the sequence it published.
func capture(t *testing.T, vm *volume.VM, guest *machine) uint64 {
	t.Helper()
	ckpt, err := host.Capture(t.Context(), vm, guest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Swept(t.Context()); err != nil {
		t.Fatal(err)
	}
	return ckpt.Ref().Sequence
}

// objectsUnder lists what the store still holds for one checkpoint of vm-1.
func objectsUnder(t *testing.T, h *hostHarness, sequence uint64) []string {
	t.Helper()
	marker := "/ckpt/" + strconv.FormatUint(sequence, 10) + "/"
	var keys []string
	for token := ""; ; {
		page, err := h.runtime.ObjectStore().List(t.Context(), platform.ListRequest{Prefix: h.prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			if key := object.Key.String(); strings.Contains(key, "/vm-1/") && strings.Contains(key, marker) {
				keys = append(keys, key)
			}
		}
		if token = page.NextContinuationToken; token == "" {
			return keys
		}
	}
}
