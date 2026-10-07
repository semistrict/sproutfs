package control_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// sourceDisk and destinationDisk are the identities of two hosts' journal
// disks.
var (
	sourceDisk      = [16]byte{0x5a, 0x11, 0x0e, 0x42, 0x9b, 0xc7, 0x03, 0x6d, 0xe8, 0x21, 0x77, 0x90, 0x4f, 0xa2, 0x18, 0xd6}
	destinationDisk = [16]byte{0x07, 0xf3, 0x6a, 0x2e, 0xd1, 0x58, 0xb4, 0x99, 0x3c, 0x40, 0xe5, 0x0b, 0x86, 0x1f, 0x72, 0xca}
)

// sourceJournal is the creating writer's journal, covering position covered.
func sourceJournal(covered uint64) control.Journal {
	return control.Journal{Disk: sourceDisk, Generation: 41, Epoch: control.MinimumEpoch, Covered: covered}
}

// readJournals reports the journals the durable record names.
func readJournals(t *testing.T, client *control.Client) []control.Journal {
	t.Helper()
	record, err := client.Read(t.Context(), "vm")
	if err != nil {
		t.Fatal(err)
	}
	return record.Journals
}

// A selection writes the list it is given in place of the one there: a
// checkpoint names the journals that may hold writes it does not, and the
// position it covers in each. A selection that holds every store, as a stop's
// and a close's do, names none and leaves the list empty.
func TestASelectionWritesTheJournalsItNames(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	for counter, journals := range [][]control.Journal{
		{sourceJournal(4096)},
		{sourceJournal(1 << 20)},
		nil,
	} {
		sequence := control.Sequence(control.MinimumEpoch, uint64(counter)+2)
		record, err := handle.Select(ctx, sequence, journals)
		if err != nil {
			t.Fatalf("selecting %d naming %v: %v", sequence, journals, err)
		}
		if record.Selected != sequence || !slices.Equal(record.Journals, journals) {
			t.Fatalf("the selection of %d reported %d naming %v, want %v",
				sequence, record.Selected, record.Journals, journals)
		}
		if durable := readJournals(t, client); !slices.Equal(durable, journals) {
			t.Fatalf("after selecting %d the record names %v, want %v", sequence, durable, journals)
		}
	}
}

// A selection that keeps its checkpoint writes the list in the same write.
func TestASelectionThatKeepsWritesTheJournalsItNames(t *testing.T) {
	client, _, _ := keptClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	journals := []control.Journal{sourceJournal(8192)}
	record, err := handle.SelectKept(ctx, second, true, journals)
	if err != nil {
		t.Fatal(err)
	}
	if !record.IsKept(second) || !slices.Equal(record.Journals, journals) {
		t.Fatalf("the kept selection reported %+v, want %d kept naming %v", record, second, journals)
	}
	durable, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(durable, record) {
		t.Fatalf("the durable record = %+v, want %+v", durable, record)
	}
}

// An open keeps the list as it is: a recovery replays the journals the record
// names before the VM runs, so the epoch it takes must not lose them. A pin and
// a release without the writer keep it too.
func TestAnOpenAPinAndAReleaseKeepTheJournals(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	third := control.Sequence(control.MinimumEpoch, 3)
	if _, err := handle.SelectKept(ctx, second, false, nil); err != nil {
		t.Fatal(err)
	}
	journals := []control.Journal{sourceJournal(4096)}
	if _, err := handle.Select(ctx, third, journals); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	if _, err := client.Pin(ctx, "vm", 0); err != nil {
		t.Fatal(err)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, journals) {
		t.Fatalf("after a pin the record names %v, want %v", durable, journals)
	}
	if _, err := client.Release(ctx, "vm", second); err != nil {
		t.Fatal(err)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, journals) {
		t.Fatalf("after a release the record names %v, want %v", durable, journals)
	}
	reopened, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Record().Journals; !slices.Equal(got, journals) {
		t.Fatalf("the reopened handle names %v, want %v", got, journals)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, journals) {
		t.Fatalf("after an open the record names %v, want %v", durable, journals)
	}
}

// A migration's open adds the destination's journal after the source's, in the
// write that claims the epoch, and stamps it with that epoch. The source's
// journal stays named until the destination selects without it.
func TestAMigrationOpenAddsItsJournalAfterTheSources(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	source, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Select(ctx, control.Sequence(control.MinimumEpoch, 2),
		[]control.Journal{sourceJournal(4096)}); err != nil {
		t.Fatal(err)
	}
	destination, err := client.OpenMigration(ctx, "vm",
		control.Journal{Disk: destinationDisk, Generation: 7, Covered: 65536})
	if err != nil {
		t.Fatal(err)
	}
	want := []control.Journal{sourceJournal(4096),
		{Disk: destinationDisk, Generation: 7, Epoch: control.MinimumEpoch + 1, Covered: 65536}}
	if destination.Epoch() != control.MinimumEpoch+1 || !slices.Equal(destination.Record().Journals, want) {
		t.Fatalf("the destination holds epoch %d naming %v, want %d naming %v",
			destination.Epoch(), destination.Record().Journals, control.MinimumEpoch+1, want)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, want) {
		t.Fatalf("after the migration's open the record names %v, want %v", durable, want)
	}
	if _, err := source.Select(ctx, control.Sequence(control.MinimumEpoch, 3), nil); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("the source selecting after the migration's open = %v, want ErrFenced", err)
	}
	own := want[1:]
	if _, err := destination.Select(ctx, control.Sequence(destination.Epoch(), 1), own); err != nil {
		t.Fatal(err)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, own) {
		t.Fatalf("after the destination's selection the record names %v, want %v", durable, own)
	}
}

// A migration's open whose claim lands and whose reply is lost owns the epoch
// and the journal it added, as any open does.
func TestALostMigrationOpenReplyKeepsItsJournal(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "vm", root(), true); err != nil {
		t.Fatal(err)
	}
	store.FailNextAfterApply(sim.ObjectPut, 1)
	destination, err := client.OpenMigration(ctx, "vm", control.Journal{Disk: destinationDisk, Generation: 7})
	if err != nil {
		t.Fatalf("a migration's open whose reply was lost reported %v", err)
	}
	want := []control.Journal{{Disk: destinationDisk, Generation: 7, Epoch: control.MinimumEpoch + 1}}
	if destination.Epoch() != control.MinimumEpoch+1 || !slices.Equal(destination.Record().Journals, want) {
		t.Fatalf("the reconciled open holds epoch %d naming %v, want %d naming %v",
			destination.Epoch(), destination.Record().Journals, control.MinimumEpoch+1, want)
	}
	if durable := readJournals(t, client); !slices.Equal(durable, want) {
		t.Fatalf("the record names %v, want %v", durable, want)
	}
}

// A record names at most two journals: a source's and its destination's. A
// migration's open of a record that already names two is refused, and the
// epoch is left alone.
func TestAMigrationOpenOfARecordNamingTwoJournalsIsRefused(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	source, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Select(ctx, control.Sequence(control.MinimumEpoch, 2),
		[]control.Journal{sourceJournal(4096)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenMigration(ctx, "vm", control.Journal{Disk: destinationDisk, Generation: 7}); err != nil {
		t.Fatal(err)
	}
	before, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenMigration(ctx, "vm", control.Journal{Disk: sourceDisk, Generation: 41}); !errors.Is(err, control.ErrTooManyJournals) {
		t.Fatalf("a migration's open of a record naming two journals = %v, want ErrTooManyJournals", err)
	}
	after, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(after, before) {
		t.Fatalf("the refused open left %+v, want %+v", after, before)
	}
}

// A list the record could not hold is refused before anything is written: a
// disk with no identity, a journal of an epoch past the writer's, two of one
// epoch, epochs out of order, or more than two. A migration's open refuses a
// journal with no identity, or one that names an epoch of its own.
func TestJournalsTheRecordCannotHoldAreRefused(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "vm", root(), true); err != nil {
		t.Fatal(err)
	}
	// Two opens, so the writer's epoch has two before it.
	if _, err := client.Open(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	handle, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	epoch := handle.Epoch()
	oldest := control.Journal{Disk: destinationDisk, Generation: 1, Epoch: epoch - 2}
	older := control.Journal{Disk: sourceDisk, Generation: 41, Epoch: epoch - 1}
	own := control.Journal{Disk: destinationDisk, Generation: 7, Epoch: epoch}
	for _, test := range []struct {
		name     string
		journals []control.Journal
	}{
		{"no identity", []control.Journal{{Generation: 7, Epoch: epoch}}},
		{"epoch zero", []control.Journal{{Disk: destinationDisk, Generation: 7}}},
		{"an epoch past the writer's", []control.Journal{{Disk: destinationDisk, Generation: 7, Epoch: epoch + 1}}},
		{"two of one epoch", []control.Journal{own, own}},
		{"out of order", []control.Journal{own, older}},
		{"three", []control.Journal{oldest, older, own}},
	} {
		if _, err := handle.Select(ctx, control.Sequence(epoch, 1), test.journals); !errors.Is(err, control.ErrInvalidConfig) {
			t.Fatalf("selecting %s = %v, want ErrInvalidConfig", test.name, err)
		}
	}
	durable, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecords(durable, handle.Record()) || durable.Selected != root() {
		t.Fatalf("the refused selections left %+v, want %+v", durable, handle.Record())
	}
	if _, err := handle.Select(ctx, control.Sequence(epoch, 1), []control.Journal{older, own}); err != nil {
		t.Fatalf("selecting an earlier writer's journal and this one's: %v", err)
	}
	for _, journal := range []control.Journal{{Generation: 7}, {Disk: destinationDisk, Generation: 7, Epoch: epoch}} {
		if _, err := client.OpenMigration(ctx, "vm", journal); !errors.Is(err, control.ErrInvalidConfig) {
			t.Fatalf("a migration's open adding %+v = %v, want ErrInvalidConfig", journal, err)
		}
	}
}
