package journal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

const testRing = 256 << 10

// Every entry a commit was answered for reads back after the power is lost,
// on a device that tears and garbles what was not synced, with the position
// the commit was given.
func TestEveryAnsweredEntryReadsBackAfterAPowerLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, testRing,
			sim.DiskConfig{PowerLossFaults: true, PowerLossKillMode: sim.FullCorruption})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		if !j.Formatted() {
			t.Fatal("a blank disk was not formatted")
		}
		a1, b1, a2 := entryOf("vm-a", 1, 0x10, 0, 7), entryOf("vm-b", 1, 0x20, 3), entryOf("vm-a", 1, 0x30, 7)
		atA1 := mustCommit(t, ctx, j, a1)
		atB1 := mustCommit(t, ctx, j, b1)
		atA2 := mustCommit(t, ctx, j, a2)
		if err := disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Close(ctx); !errors.Is(err, platform.ErrStaleHandle) {
			t.Fatalf("closing a journal whose device lost its power: %v, want ErrStaleHandle", err)
		}

		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberB})
		if again.Formatted() || again.Generation() != j.Generation() {
			t.Fatalf("the journal read back formatted %v under generation %d, want %d", again.Formatted(),
				again.Generation(), j.Generation())
		}
		want := []Held{
			{VM: "vm-a", Epoch: 1, Entries: 2, Bytes: a1.size() + a2.size(), First: atA1[0], Last: atA2[0]},
			{VM: "vm-b", Epoch: 1, Entries: 1, Bytes: b1.size(), First: atB1[0], Last: atB1[0]},
		}
		if got := again.Held(); !slices.Equal(got, want) {
			t.Fatalf("the journal holds %+v, want %+v", got, want)
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-a", 1, 0, 2), []Entry{at(a1, atA1[0]), at(a2, atA2[0])}); diff != "" {
			t.Fatal(diff)
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-b", 1, 0, 2), []Entry{at(b1, atB1[0])}); diff != "" {
			t.Fatal(diff)
		}
	})
}

// A batch whose write was torn ends reading back: the entry it tore is
// refused by its checksum, and the next holder writes from where the last
// whole entry ends.
func TestATornBatchEndsTheReadBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		e1, e2, e3 := entryOf("vm-a", 1, 0x10, 0), entryOf("vm-a", 1, 0x20, 1, 2), entryOf("vm-a", 1, 0x30, 3)
		at1 := mustCommit(t, ctx, j, e1)
		// The batch keeps its first entry's head and a few of its blocks'
		// bytes.
		disk.TearNextWrite(100)
		if _, err := commitOf(ctx, j, e2); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("a commit whose write was torn: %v, want ErrInjectedFault", err)
		}
		if _, err := j.Close(ctx); err != nil {
			t.Fatal(err)
		}

		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		if runtime.Probes()[ProbeTornEntry] != 1 {
			t.Fatalf("reading back reached the torn entry %d times, want once", runtime.Probes()[ProbeTornEntry])
		}
		if got, want := again.Held(), []Held{{VM: "vm-a", Epoch: 1, Entries: 1, Bytes: e1.size(), First: at1[0],
			Last: at1[0]}}; !slices.Equal(got, want) {
			t.Fatalf("the journal holds %+v, want %+v", got, want)
		}
		at3 := mustCommit(t, ctx, again, e3)
		if want := at1[0] + 2*BlockBytes; at3[0] != want {
			t.Fatalf("the next holder wrote at %d, want %d: where the last whole batch ends", at3[0], want)
		}
		if _, err := again.Close(ctx); err != nil {
			t.Fatal(err)
		}
		last := open(t, ctx, handle(), Lease{Assigned: 3, Member: memberA})
		if diff := sameEntries(readAll(t, ctx, last, "vm-a", 1, 0, 2), []Entry{at(e1, at1[0]), at(e3, at3[0])}); diff != "" {
			t.Fatal(diff)
		}
	})
}

// A batch whose write or sync failed may be partly on the disk. The next
// batch pads over its range and goes after it, so reading back passes it and
// loses nothing written later, and no position is given out twice.
func TestAFailedBatchIsPaddedOverAndNothingAfterItIsLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{PowerLossFaults: true})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		first := uint64(testRing)
		e1, e2, e3 := entryOf("vm-a", 1, 0x10, 0), entryOf("vm-a", 1, 0x20, 1, 2), entryOf("vm-a", 1, 0x30, 3)
		e4, e5 := entryOf("vm-a", 1, 0x40, 4), entryOf("vm-a", 1, 0x50, 5)
		at1 := mustCommit(t, ctx, j, e1)
		disk.TearNextWrite(5000)
		if _, err := commitOf(ctx, j, e2); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("a commit whose write was torn: %v, want ErrInjectedFault", err)
		}
		// e1's batch is 8 KiB; e2's, which failed, 12 KiB after it.
		at3 := mustCommit(t, ctx, j, e3)
		disk.FailNext(sim.DiskSync, 1)
		if _, err := commitOf(ctx, j, e4); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("a commit whose sync failed: %v, want ErrInjectedFault", err)
		}
		at5 := mustCommit(t, ctx, j, e5)
		if got, want := []uint64{at1[0], at3[0], at5[0]}, []uint64{first, first + 20<<10, first + 36<<10}; !slices.Equal(got, want) {
			t.Fatalf("the answered entries are at %v, want %v", got, want)
		}
		if runtime.Probes()[ProbeFailedRangePadded] != 2 {
			t.Fatalf("%d batches padded over a failed one, want 2", runtime.Probes()[ProbeFailedRangePadded])
		}
		if err := disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		want := []Entry{at(e1, at1[0]), at(e3, at3[0]), at(e5, at5[0])}
		if diff := sameEntries(readAll(t, ctx, again, "vm-a", 1, 0, 2), want); diff != "" {
			t.Fatal(diff)
		}
		if got := mustCommit(t, ctx, again, entryOf("vm-a", 2, 0x60, 6)); got[0] != first+44<<10 {
			t.Fatalf("after the power loss the journal wrote at %d, want %d", got[0], first+44<<10)
		}
	})
}

// A lease of a newer assignment refuses the disk, as it opens and while it is
// open: a holder whose lease was taken refuses every commit and read from
// then on.
func TestANewerLeaseRefusesTheDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		old := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		mustCommit(t, ctx, old, entryOf("vm-a", 1, 0x10, 0))

		taken := open(t, ctx, handle(), Lease{Assigned: 4, Member: memberB})
		if len(taken.Held()) != 1 {
			t.Fatalf("the new holder holds %+v, want vm-a's entry", taken.Held())
		}
		if err := old.CheckLease(ctx); !errors.Is(err, ErrLeased) {
			t.Fatalf("the old holder's lease check: %v, want ErrLeased", err)
		}
		if _, err := commitOf(ctx, old, entryOf("vm-a", 1, 0x20, 1)); !errors.Is(err, ErrLeased) {
			t.Fatalf("a commit to a holder whose lease was taken: %v, want ErrLeased", err)
		}
		if err := old.Read(ctx, ReadRequest{VM: "vm-a", Epoch: 1, Generation: old.Generation(), Reader: 2},
			func(Entry) error { return nil }); !errors.Is(err, ErrLeased) {
			t.Fatalf("a read of a holder whose lease was taken: %v, want ErrLeased", err)
		}
		if _, err := old.Close(ctx); !errors.Is(err, ErrLeased) {
			t.Fatalf("closing a holder whose lease was taken: %v, want ErrLeased", err)
		}
		for _, lease := range []Lease{{Assigned: 1, Member: memberA}, {Assigned: 3, Member: memberB},
			{Assigned: 4, Member: memberA}} {
			_, err := Open(ctx, handle(), Config{Identity: diskOne, Lease: lease})
			if !errors.Is(err, ErrLeased) {
				t.Fatalf("opening the disk under %+v: %v, want ErrLeased", lease, err)
			}
		}
		if got := runtime.Probes(); got[ProbeLeaseRefused] != 3 || got[ProbeLeaseLost] != 1 {
			t.Fatalf("the lease was refused at open %d times and lost %d times, want 3 and 1",
				got[ProbeLeaseRefused], got[ProbeLeaseLost])
		}
		if _, err := Open(ctx, handle(), Config{Identity: memberA, Lease: Lease{Assigned: 9, Member: memberB}}); !errors.Is(err, ErrIdentity) {
			t.Fatalf("opening the disk as another: %v, want ErrIdentity", err)
		}
	})
}

// A commit is answered only once the batch that holds it has synced. While a
// batch is in flight no other is written: the commits that arrive meanwhile
// wait, and then go together into the next batch, after it.
func TestACommitIsAnsweredOnlyAfterItsBatchSyncs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		// A test that fails with the sync held lets it go before the journal
		// closes.
		t.Cleanup(file.release)
		type answer struct {
			positions []uint64
			err       error
		}
		start := func(e Entry) chan answer {
			answered := make(chan answer, 1)
			go func() {
				positions, err := commitOf(ctx, j, e)
				answered <- answer{positions, err}
			}()
			synctest.Wait()
			return answered
		}
		reached := file.hold()
		before := file.written()
		a := start(entryOf("vm-a", 1, 0x10, 0))
		<-reached
		synctest.Wait()
		select {
		case got := <-a:
			t.Fatalf("a commit was answered %+v while its batch's sync had not returned", got)
		default:
		}
		b := start(entryOf("vm-b", 1, 0x20, 0))
		c := start(entryOf("vm-c", 1, 0x30, 0))
		if got := file.written() - before; got != 1 {
			t.Fatalf("%d writes while a batch was in flight, want its own one", got)
		}
		file.release()
		var positions []uint64
		for _, answered := range []chan answer{a, b, c} {
			got := <-answered
			if got.err != nil {
				t.Fatal(got.err)
			}
			positions = append(positions, got.positions...)
		}
		first := uint64(testRing)
		if want := []uint64{first, first + 8<<10, first + 8<<10 + 4176}; !slices.Equal(positions, want) {
			t.Fatalf("the commits are at %v, want %v", positions, want)
		}
		mustCommit(t, ctx, j, entryOf("vm-d", 1, 0x40, 0))
		if runtime.Probes()[ProbeGrouped] != 1 {
			t.Fatalf("%d batches held more than one commit, want 1", runtime.Probes()[ProbeGrouped])
		}
	})
}

// A commit whose sync a power loss interrupts is not answered, and nothing of
// it reads back.
func TestACommitWhoseSyncAPowerLossInterruptsIsNotAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		// A test that fails with the sync held lets it go before the journal
		// closes.
		t.Cleanup(file.release)
		reached := file.hold()
		answered := make(chan error, 1)
		go func() {
			_, err := commitOf(ctx, j, entryOf("vm-a", 1, 0x10, 0))
			answered <- err
		}()
		<-reached
		synctest.Wait()
		select {
		case err := <-answered:
			t.Fatalf("a commit was answered (%v) while its batch's sync had not returned", err)
		default:
		}
		if err := disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		file.release()
		if err := <-answered; !errors.Is(err, platform.ErrStaleHandle) {
			t.Fatalf("a commit whose sync a power loss interrupted: %v, want ErrStaleHandle", err)
		}
		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		if got := again.Held(); len(got) != 0 {
			t.Fatalf("the journal holds %+v, want nothing", got)
		}
	})
}

// A read fences the VM at the reader's epoch and waits for the batch in
// flight, so it returns every entry a commit was answered for. From then on a
// commit of the VM at an older epoch is refused, and one at the reader's
// epoch is not.
func TestAReadFencesTheVMAndWaitsForTheBatchInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		// A test that fails with the sync held lets it go before the journal
		// closes.
		t.Cleanup(file.release)
		a1, a2 := entryOf("vm-a", 1, 0x10, 0), entryOf("vm-a", 1, 0x20, 1)
		at1 := mustCommit(t, ctx, j, a1)
		reached := file.hold()
		committed := make(chan []uint64, 1)
		go func() {
			positions, err := commitOf(ctx, j, a2)
			if err != nil {
				t.Error(err)
			}
			committed <- positions
		}()
		<-reached
		synctest.Wait()
		read := make(chan []Entry, 1)
		go func() {
			var entries []Entry
			err := j.Read(ctx, ReadRequest{VM: "vm-a", Epoch: 1, Generation: j.Generation(), Reader: 2},
				func(e Entry) error {
					entries = append(entries, e)
					return nil
				})
			if err != nil {
				t.Error(err)
			}
			read <- entries
		}()
		synctest.Wait()
		select {
		case got := <-read:
			t.Fatalf("a read returned %s while a batch placed before its fence was in flight", describe(got))
		default:
		}
		file.release()
		at2 := <-committed
		if diff := sameEntries(<-read, []Entry{at(a1, at1[0]), at(a2, at2[0])}); diff != "" {
			t.Fatal(diff)
		}
		if _, err := commitOf(ctx, j, entryOf("vm-a", 1, 0x30, 2)); !errors.Is(err, ErrFenced) {
			t.Fatalf("a commit at the fenced epoch: %v, want ErrFenced", err)
		}
		if runtime.Probes()[ProbeFenced] != 1 {
			t.Fatalf("the fence refused %d commits, want 1", runtime.Probes()[ProbeFenced])
		}
		mustCommit(t, ctx, j, entryOf("vm-a", 2, 0x40, 2))
		mustCommit(t, ctx, j, entryOf("vm-b", 1, 0x50, 2))
	})
}

// Trimming by covered positions frees the ring. A commit the ring has no room
// for waits, and goes on once a trim has moved the tail and the tail hint has
// reached the header. An entry that would cross the ring's end goes after a
// pad, at the ring's start, and reads back from there.
func TestTrimmingByCoveredPositionsFreesTheRing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		const ring = 64 << 10
		disk, handle := device(t, ctx, runtime, ring, sim.DiskConfig{PowerLossFaults: true})
		clock := runtime.NewClock("journal")
		j := openWith(t, ctx, handle(), Config{Lease: Lease{Assigned: 1, Member: memberA}, Clock: clock})
		var entries []Entry
		var positions []uint64
		for i := range 4 {
			e := entryOf("vm-a", 1, byte(0x10*(i+1)), uint64(2*i), uint64(2*i+1))
			entries = append(entries, e)
			positions = append(positions, mustCommit(t, ctx, j, e)...)
		}
		fifth := entryOf("vm-a", 1, 0x50, 8, 9)
		waiting := make(chan []uint64, 1)
		go func() {
			got, err := commitOf(ctx, j, fifth)
			if err != nil {
				t.Error(err)
			}
			waiting <- got
		}()
		synctest.Wait()
		select {
		case got := <-waiting:
			t.Fatalf("a commit the ring had no room for was answered at %v", got)
		default:
		}
		if got, want := j.Usage(), (Usage{Ring: ring, Used: 48 << 10}); got != want {
			t.Fatalf("the ring's usage is %+v, want %+v", got, want)
		}
		// The header was last written as the journal opened. Half a second
		// later a trim moves the tail, and the tail hint reaches the header
		// half a second after that, not before.
		clock.Advance(500 * time.Millisecond)
		j.Trim("vm-a", map[uint64]uint64{1: positions[2]})
		synctest.Wait()
		clock.Advance(499 * time.Millisecond)
		synctest.Wait()
		if clock.Pending() != 1 {
			t.Fatal("the tail hint was written less than a second after the header")
		}
		clock.Advance(time.Millisecond)
		if clock.Pending() != 0 {
			t.Fatal("the tail hint was not written a second after the header")
		}
		entries, positions = append(entries[3:], fifth), append(positions[3:], (<-waiting)...)
		if runtime.Probes()[ProbeFull] != 1 {
			t.Fatalf("%d commits waited for room, want 1", runtime.Probes()[ProbeFull])
		}
		sixth := entryOf("vm-a", 1, 0x60, 10, 11)
		entries, positions = append(entries, sixth), append(positions, mustCommit(t, ctx, j, sixth)...)
		first := uint64(ring)
		if want := []uint64{first + 36<<10, first + 48<<10, first + 64<<10}; !slices.Equal(positions, want) {
			t.Fatalf("the live entries are at %v, want %v", positions, want)
		}
		if runtime.Probes()[ProbeRingEndPadded] != 1 {
			t.Fatalf("%d pads filled the ring's end, want 1", runtime.Probes()[ProbeRingEndPadded])
		}
		if err := disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		var want []Entry
		for i, e := range entries {
			want = append(want, at(e, positions[i]))
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-a", 1, positions[0]-1, 2), want); diff != "" {
			t.Fatal(diff)
		}
		again.Trim("vm-a", map[uint64]uint64{1: positions[1]})
		if got := again.Held(); !slices.Equal(got, []Held{{VM: "vm-a", Epoch: 1, Entries: 1, Bytes: sixth.size(),
			First: positions[2], Last: positions[2]}}) {
			t.Fatalf("after a trim the journal holds %+v, want the last entry", got)
		}
		again.Trim("vm-a", map[uint64]uint64{2: 0})
		if got := again.Usage(); got.Used != 0 || len(again.Held()) != 0 {
			t.Fatalf("with the epoch no longer named the journal holds %+v and uses %+v, want nothing",
				again.Held(), got)
		}
	})
}

// Closing a journal with no live entry writes empty into its header; closing
// one with a live entry does not.
func TestClosingWritesEmptyOnlyWhenNoEntryIsLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		file := handle()
		emptyAfter := func(j *Journal) (bool, bool) {
			t.Helper()
			empty, err := j.Close(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found, _, err := readHeader(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			return empty, found.empty
		}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		positions := mustCommit(t, ctx, j, entryOf("vm-a", 1, 0x10, 0))
		if reported, written := emptyAfter(j); reported || written {
			t.Fatalf("a journal with a live entry closed empty: reported %v, written %v", reported, written)
		}
		j = open(t, ctx, file, Lease{Assigned: 2, Member: memberA})
		j.Trim("vm-a", map[uint64]uint64{1: positions[0]})
		if reported, written := emptyAfter(j); !reported || !written {
			t.Fatalf("a journal with no live entry closed empty: reported %v, written %v", reported, written)
		}
		if _, err := j.Close(ctx); !errors.Is(err, ErrClosed) {
			t.Fatalf("closing a journal twice: %v, want ErrClosed", err)
		}
		if _, err := commitOf(ctx, j, entryOf("vm-a", 1, 0x10, 0)); !errors.Is(err, ErrClosed) {
			t.Fatalf("a commit to a closed journal: %v, want ErrClosed", err)
		}
	})
}

// A commit that cannot fit the ring, or whose capture makes more than it
// asked room for, is refused; one whose capture fails fails alone.
func TestACommitTooLargeOrWhoseCaptureFailsIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		e := entryOf("vm-a", 1, 0x10, 0)
		if _, err := j.Commit(ctx, testRing, func(context.Context) ([]Entry, error) { return nil, nil }); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a commit as large as the ring: %v, want ErrTooLarge", err)
		}
		if _, err := j.Commit(ctx, e.size()-1, func(context.Context) ([]Entry, error) { return []Entry{e}, nil }); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a capture larger than its room: %v, want ErrTooLarge", err)
		}
		failure := fmt.Errorf("the capture failed")
		if _, err := j.Commit(ctx, e.size(), func(context.Context) ([]Entry, error) { return nil, failure }); !errors.Is(err, failure) {
			t.Fatalf("a commit whose capture failed: %v, want its error", err)
		}
		if got, err := j.Commit(ctx, 0, func(context.Context) ([]Entry, error) { return nil, nil }); err != nil || len(got) != 0 {
			t.Fatalf("a commit with nothing to capture: %v, %v, want no positions", got, err)
		}
		// The largest commit reserves the whole ring.
		largest := int64(testRing-BlockBytes-2*minEntryBytes) / 2
		if _, err := j.Commit(ctx, largest, func(context.Context) ([]Entry, error) { return nil, nil }); err != nil {
			t.Fatalf("a commit that reserves the whole ring: %v", err)
		}
		if got := mustCommit(t, ctx, j, e); got[0] != testRing {
			t.Fatalf("the first entry after the refused commits is at %d, want %d", got[0], testRing)
		}
	})
}
