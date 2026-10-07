package host_test

import (
	"slices"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
)

// TestAStopWritesNoJournal: a stop's checkpoint holds every store the guest
// made, so its selection leaves the record naming no journal, even when the
// open before it kept one. The journal here is one a migration's open added,
// which is the only way a record names one before a host journals.
func TestAStopWritesNoJournal(t *testing.T) {
	h := newHostHarness(t)
	pagers := newPager(t, h.configs[0].Resources)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	created, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", coldVolumes)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	records := h.hosts[0].Control()
	destination, err := records.OpenMigration(t.Context(), "vm-1",
		control.Journal{Disk: [16]byte{0x07, 0xf3}, Generation: 5, Covered: 8192})
	if err != nil {
		t.Fatal(err)
	}
	destination.Close()
	named := []control.Journal{{Disk: [16]byte{0x07, 0xf3}, Generation: 5, Epoch: destination.Epoch(), Covered: 8192}}

	vm, err := h.hosts[0].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := records.Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opened.Journals, named) {
		t.Fatalf("after the open the record names %v, want %v", opened.Journals, named)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("root", 0, 7)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.hosts[0].Stop(t.Context(), "vm-1", hostapi.StopRequest{Suspend: true})
	if err != nil {
		t.Fatal(err)
	}
	record, err := records.Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Selected != stopped.Sequence || len(record.Journals) != 0 {
		t.Fatalf("after the stop the record selects %d naming %v, want %d naming none",
			record.Selected, record.Journals, stopped.Sequence)
	}
}
