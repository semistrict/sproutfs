package volume_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
)

// An open keeps the journals the record names, and a close's final checkpoint
// holds every store, so its selection writes an empty list. The journal here
// is one a migration's open added, which is the only way a record names one
// before a host journals.
func TestAnOpenKeepsTheJournalsAndACloseWritesNone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		vm, _ := createVM(t, manager, "vm")
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		client := h.controlClient(t, h.objects)
		destination, err := client.OpenMigration(t.Context(), "vm",
			control.Journal{Disk: [16]byte{0x5a, 0x11}, Generation: 3, Covered: 4096})
		if err != nil {
			t.Fatal(err)
		}
		destination.Close()
		named := []control.Journal{{Disk: [16]byte{0x5a, 0x11}, Generation: 3, Epoch: destination.Epoch(), Covered: 4096}}

		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		opened, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if opened.Epoch != reopened.Epoch() || !slices.Equal(opened.Journals, named) {
			t.Fatalf("after the open the record is at epoch %d naming %v, want %d naming %v",
				opened.Epoch, opened.Journals, reopened.Epoch(), named)
		}
		if err := reopened.Volume("root").Write(t.Context(), 0, []byte("closing")); err != nil {
			t.Fatal(err)
		}
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		closed, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if closed.Selected != counted(reopened, 1) || len(closed.Journals) != 0 {
			t.Fatalf("after the close the record selects %d naming %v, want %d naming none",
				closed.Selected, closed.Journals, counted(reopened, 1))
		}
	})
}
