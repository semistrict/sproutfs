package volume_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// fakeReplay is a deployment's journals as a manager's open sees them: whether
// they are served, and the entries each holds, which a replay hands over in
// order, failing with fail after the first where fail is set.
type fakeReplay struct {
	served  error
	entries []journal.Entry
	fail    error
	// asked is each replay's journals and reader epoch.
	asked   [][]control.Journal
	readers []uint64
	// lost is the journal disks formatted again.
	lost map[[16]byte]bool
}

func (f *fakeReplay) Served(context.Context, []control.Journal) error { return f.served }

func (f *fakeReplay) Replay(_ context.Context, vm string, journals []control.Journal, reader uint64,
	apply func(journal.Entry) error) error {
	f.asked, f.readers = append(f.asked, journals), append(f.readers, reader)
	for _, named := range journals {
		if f.lost[named.Disk] {
			return journal.ErrGeneration
		}
	}
	for index, entry := range f.entries {
		if err := apply(entry); err != nil {
			return err
		}
		if index == 0 && f.fail != nil {
			return f.fail
		}
	}
	return nil
}

// blocksOf is one entry of root's blocks, each filled with its own byte.
func blocksOf(fill byte, blocks ...uint64) journal.Entry {
	entry := journal.Entry{VM: "vm", Volume: "root", Blocks: blocks}
	for index := range blocks {
		entry.Data = append(entry.Data, bytes.Repeat([]byte{fill + byte(index)}, journal.BlockBytes)...)
	}
	return entry
}

// namedJournal is a record of vm naming one journal, as a migration's open
// leaves it, and that journal.
func namedJournal(t *testing.T, h *harness, manager *volume.Manager) (*control.Client, []control.Journal) {
	t.Helper()
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
	return client, []control.Journal{{Disk: [16]byte{0x5a, 0x11}, Generation: 3, Epoch: destination.Epoch(),
		Covered: 4096}}
}

// An open keeps the journals the record names, and a close's final checkpoint
// holds every store, so its selection writes an empty list. The journal here
// is one a migration's open added, and holds nothing of the VM.
func TestAnOpenKeepsTheJournalsAndACloseWritesNone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		config := h.config()
		config.Replay = &fakeReplay{}
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, named := namedJournal(t, h, manager)

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

// An open writes what the journals its record names hold into the VM before
// anything runs it, in order, having had the holder fence the VM at the epoch
// the open took. The VM is then marked replayed, and its next checkpoint
// publishes the blocks and names no journal.
func TestAnOpenReplaysTheJournalsItsRecordNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		replay := &fakeReplay{entries: []journal.Entry{blocksOf(1, 0, 2), blocksOf(9, 2)}}
		config := h.config()
		config.Replay = replay
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, named := namedJournal(t, h, manager)

		vm, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if !vm.Replayed() {
			t.Fatal("a VM whose journals held entries is not marked replayed")
		}
		if len(replay.asked) != 1 || !slices.Equal(replay.asked[0], named) || replay.readers[0] != vm.Epoch() {
			t.Fatalf("the replay was asked %v at %v, want %v at the open's epoch %d", replay.asked, replay.readers,
				named, vm.Epoch())
		}
		got := make([]byte, 3*journal.BlockBytes)
		if err := vm.Volume("root").Read(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat(bytes.Repeat([]byte{1}, journal.BlockBytes), make([]byte, journal.BlockBytes),
			bytes.Repeat([]byte{9}, journal.BlockBytes))
		if !bytes.Equal(got, want) {
			t.Fatal("the replayed blocks do not read back as the later entry over the earlier")
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		closed, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if closed.Selected != counted(vm, 1) || len(closed.Journals) != 0 {
			t.Fatalf("after the close the record selects %d naming %v, want %d naming none", closed.Selected,
				closed.Journals, counted(vm, 1))
		}
	})
}

// A journal no member serves yet refuses the open before it takes the epoch:
// the open is asked again once the disk has moved.
func TestAnOpenOfAJournalNotServedTakesNoEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		config := h.config()
		config.Replay = &fakeReplay{served: volume.ErrJournalPending}
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, _ := namedJournal(t, h, manager)
		before, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Open(t.Context(), "vm"); !errors.Is(err, volume.ErrJournalPending) {
			t.Fatalf("an open of a VM whose journal is not served = %v, want ErrJournalPending", err)
		}
		after, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if after.Epoch != before.Epoch {
			t.Fatalf("the refused open moved the epoch from %d to %d", before.Epoch, after.Epoch)
		}
	})
}

// A replay that fails part way publishes nothing: the record keeps its
// checkpoint and the journals that hold the rest, so the next open replays
// them all. A journal formatted again is reported lost.
func TestAFailedReplayPublishesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		replay := &fakeReplay{entries: []journal.Entry{blocksOf(1, 0), blocksOf(9, 2)}, fail: journal.ErrGeneration}
		config := h.config()
		config.Replay = replay
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, named := namedJournal(t, h, manager)
		before, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		ctx := sim.WithRuntime(t.Context(), h.runtime)
		if _, err := manager.Open(ctx, "vm"); !errors.Is(err, volume.ErrJournalLost) {
			t.Fatalf("an open whose journal was formatted again = %v, want ErrJournalLost", err)
		}
		after, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if after.Selected != before.Selected || !slices.Equal(after.Journals, named) {
			t.Fatalf("the failed replay left the record selecting %d naming %v, want %d naming %v",
				after.Selected, after.Journals, before.Selected, named)
		}
	})
}

// A migration's open reads no journal back, since the source hands over every
// page, and names the destination's journal after the source's.
func TestAMigrationsOpenNamesItsJournalAndReplaysNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		replay := &fakeReplay{entries: []journal.Entry{blocksOf(1, 0)}}
		config := h.config()
		config.Replay = replay
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, named := namedJournal(t, h, manager)
		joining := control.Journal{Disk: [16]byte{0x77}, Generation: 5}
		vm, err := manager.OpenMigration(t.Context(), "vm", &joining)
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close(t.Context())
		if vm.Replayed() || len(replay.asked) != 0 {
			t.Fatalf("a migration's open replayed %v", replay.asked)
		}
		record, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		joining.Epoch = vm.Epoch()
		if want := append(slices.Clone(named), joining); !slices.Equal(record.Journals, want) {
			t.Fatalf("the migration's open left the record naming %v, want %v", record.Journals, want)
		}
	})
}

// An operator's discard opens a VM whose journal was formatted again without
// the flushes it held, and its next checkpoint names no journal.
func TestADiscardOpensAVMWhoseJournalIsLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		replay := &fakeReplay{entries: []journal.Entry{blocksOf(1, 0)}, lost: map[[16]byte]bool{{0x5a, 0x11}: true}}
		config := h.config()
		config.Replay = replay
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		client, _ := namedJournal(t, h, manager)
		if _, err := manager.Open(t.Context(), "vm"); !errors.Is(err, volume.ErrJournalLost) {
			t.Fatalf("an open whose journal is lost = %v, want ErrJournalLost", err)
		}
		vm, err := manager.OpenWith(t.Context(), "vm", volume.OpenOptions{DiscardJournals: true})
		if err != nil {
			t.Fatalf("an open discarding the lost journal: %v", err)
		}
		if vm.Replayed() {
			t.Fatal("an open that discarded its only journal replayed something")
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		closed, err := client.Read(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if len(closed.Journals) != 0 {
			t.Fatalf("after the discard and a close the record names %v, want none", closed.Journals)
		}
	})
}
