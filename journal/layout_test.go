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

// Every entry goes where the format puts it: a batch ends on a 4 KiB
// boundary, after a pad of at least 64 bytes where it does not end on one;
// an entry that would cross the ring's end, or leave less than 64 bytes
// before it, goes at the ring's start after a pad. Reading back passes every
// such pad and reaches the entry after it.
func TestABatchIsLaidOutOnTheRingAsTheFormatSays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		const ring = 64 << 10
		_, handle := device(t, ctx, runtime, ring, sim.DiskConfig{})
		file := handle()
		holder := uint64(1)
		j := open(t, ctx, file, Lease{Assigned: holder, Member: memberA})
		next := uint64(ring)
		lap := func(position uint64) uint64 { return position - position%ring }
		// place commits an entry, checks it lands at want and the next
		// batch at after, and hands the journal to a new holder. That holder
		// reads back from the last filling entry, across every pad before,
		// between and after them, and must reach both.
		step := 0
		place := func(why string, blocks int, size int64, want, after uint64) {
			t.Helper()
			step++
			e := sized(fmt.Sprintf("vm-%d", step), 1, byte(0x10*step), blocks, size)
			if got := mustCommit(t, ctx, j, e); got[0] != want {
				t.Fatalf("%s: it is at %d, want %d", why, got[0], want)
			}
			following := sized(fmt.Sprintf("vm-%d-next", step), 1, 0x01, 1, 8192)
			if got := mustCommit(t, ctx, j, following); got[0] != after {
				t.Fatalf("%s: the next batch is at %d, want %d", why, got[0], after)
			}
			if _, err := j.Close(ctx); err != nil {
				t.Fatal(err)
			}
			holder++
			j = open(t, ctx, file, Lease{Assigned: holder, Member: memberA})
			for _, read := range []Entry{at(e, want), at(following, after)} {
				if diff := sameEntries(readAll(t, ctx, j, read.VM, 1, 0, 2), []Entry{read}); diff != "" {
					t.Fatalf("%s: reading back: %s", why, diff)
				}
				j.Trim(read.VM, nil)
			}
			j.Trim("vm-fill", nil)
			next = after + 8192
		}
		// fill commits entries of 8 KiB until room is left before the ring's
		// end, and one of 12 KiB first where what is to fill is not a
		// multiple of 8 KiB. Only the last stays live.
		fill := func(room uint64) {
			t.Helper()
			var last uint64
			for ring-next%ring != room {
				size := int64(8192)
				if (ring-next%ring-room)%8192 != 0 {
					size = 12288
				}
				if got := mustCommit(t, ctx, j, sized("vm-fill", 1, 0x02, 1, size)); got[0] != next {
					t.Fatalf("a filling entry is at %d, want %d", got[0], next)
				}
				j.Trim("vm-fill", map[uint64]uint64{1: last})
				last = next
				next += uint64(size)
			}
		}

		fill(12288)
		place("an entry 8 bytes longer than the room before the ring's end goes at its start",
			2, 12296, lap(next)+ring, lap(next)+ring+16384)
		fill(12288)
		place("an entry 8 bytes shorter than that room goes at the ring's start too",
			2, 12280, lap(next)+ring, lap(next)+ring+16384)
		fill(12288)
		place("an entry as long as the room ends at the ring's end",
			2, 12288, next, lap(next)+ring)
		fill(12288)
		place("an entry 64 bytes shorter than the room ends before a pad of 64 at the ring's end",
			2, 12224, next, lap(next)+ring)
		place("an entry that ends on a 4 KiB boundary has no pad after it",
			1, 8192, next, next+8192)
		place("an entry that ends 64 bytes before a boundary has a pad of 64",
			1, 8128, next, next+8192)
		place("an entry that ends 56 bytes before a boundary has a pad of 56 and 4 KiB",
			1, 8136, next, next+8136+56+4096)
	})
}

// A batch never writes over the durable tail: a commit whose entry would
// need a pad at the ring's end as long as the entry less 8 bytes, and a pad
// of 56 bytes and 4 KiB after it, waits while the ring has 56 bytes less
// than that before the oldest live entry. A commit that needs exactly the
// room there is goes on.
func TestABatchNeverWritesOverTheDurableTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		const ring = 64 << 10
		disk, handle := device(t, ctx, runtime, ring, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		clock := runtime.NewClock("journal")
		j := openWith(t, ctx, file, Config{Lease: Lease{Assigned: 1, Member: memberA}, Clock: clock})
		// The tail hint reaches the header a second after the tail moves.
		moveTail := func() {
			t.Helper()
			synctest.Wait()
			clock.Advance(time.Second)
			if _, err := j.Commit(ctx, 0, func(context.Context) ([]Entry, error) { return nil, nil }); err != nil {
				t.Fatal(err)
			}
		}
		first := uint64(ring)
		f1, f2 := sized("vm-f", 1, 0x10, 2, 12232), sized("vm-g", 1, 0x20, 1, 4176)
		g := []Entry{f2, sized("vm-g", 1, 0x30, 2, 8280), sized("vm-g", 1, 0x40, 1, 4176),
			sized("vm-g", 1, 0x50, 1, 4176), sized("vm-g", 1, 0x60, 1, 4176)}
		positions := mustCommit(t, ctx, j, f1, f2)
		for _, e := range g[1:] {
			positions = append(positions, mustCommit(t, ctx, j, e)...)
		}
		if want := []uint64{first, first + 12232, first + 20<<10, first + 32<<10, first + 40<<10,
			first + 48<<10}; !slices.Equal(positions, want) {
			t.Fatalf("the entries are at %v, want %v", positions, want)
		}
		// The oldest live entry is f2, 12,232 bytes into the ring, and the
		// next batch goes 8 KiB before the ring's end: 20,424 bytes are free.
		j.Trim("vm-f", nil)
		moveTail()

		// x takes 8,136 bytes: a pad of 8,192 to the ring's end before it and
		// one of 4,152 after it make 20,480.
		x := sized("vm-x", 1, 0x70, 1, 8136)
		writes := file.written()
		answered := make(chan []uint64, 1)
		go func() {
			got, err := commitOf(ctx, j, x)
			if err != nil {
				t.Error(err)
			}
			answered <- got
		}()
		synctest.Wait()
		if got := file.written() - writes; got != 0 {
			t.Fatalf("a batch of 20,480 bytes was written, %d writes, with 20,424 free before the durable tail", got)
		}
		j.Trim("vm-g", map[uint64]uint64{1: positions[1]})
		moveTail()
		xAt := <-answered
		if xAt[0] != first+ring {
			t.Fatalf("x is at %d, want %d, at the ring's start", xAt[0], first+ring)
		}

		// Now 8,192 bytes are free before g's second entry, the oldest live
		// one: a commit of 1,984 bytes reserves exactly that.
		done := make(chan error, 1)
		go func() {
			_, err := j.Commit(ctx, 1984, func(context.Context) ([]Entry, error) { return nil, nil })
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("a commit that needs exactly the room the ring has waits")
		}

		if err := disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		var want []Entry
		for i, e := range g[1:] {
			want = append(want, at(e, positions[i+2]))
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-g", 1, 0, 2), want); diff != "" {
			t.Fatal(diff)
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-x", 1, 0, 2), []Entry{at(x, xAt[0])}); diff != "" {
			t.Fatal(diff)
		}
	})
}

// A batch takes the commits waiting for it while their room adds up to 8 MiB
// at most, and leaves the rest to the next batch.
func TestABatchTakesCommitsUpToItsRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, 64<<20, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		t.Cleanup(file.release)
		start := func(room int64, e Entry) chan []uint64 {
			answered := make(chan []uint64, 1)
			go func() {
				got, err := j.Commit(ctx, room, func(context.Context) ([]Entry, error) { return []Entry{e}, nil })
				if err != nil {
					t.Error(err)
				}
				answered <- got
			}()
			synctest.Wait()
			return answered
		}
		reached := file.hold()
		a := start(4176, entryOf("vm-a", 1, 0x10, 0))
		<-reached
		b := start(MaxBatchBytes/2, entryOf("vm-b", 1, 0x20, 0))
		c := start(MaxBatchBytes/2, entryOf("vm-c", 1, 0x30, 0))
		d := start(4176, entryOf("vm-d", 1, 0x40, 0))
		file.release()
		var positions []uint64
		for _, answered := range []chan []uint64{a, b, c, d} {
			positions = append(positions, (<-answered)...)
		}
		first := uint64(64 << 20)
		if want := []uint64{first, first + 8<<10, first + 8<<10 + 4176, first + 20<<10}; !slices.Equal(positions, want) {
			t.Fatalf("the commits are at %v, want %v: b and c in one batch, d in the next", positions, want)
		}
		// A commit of more room than a batch has goes into a batch alone.
		e := entryOf("vm-e", 1, 0x50, 0)
		got, err := j.Commit(ctx, MaxBatchBytes+BlockBytes, func(context.Context) ([]Entry, error) {
			return []Entry{e}, nil
		})
		if err != nil || !slices.Equal(got, []uint64{first + 28<<10}) {
			t.Fatalf("a commit larger than a batch is at %v (%v), want %d", got, err, first+28<<10)
		}
	})
}

// Reading back reads the ring a window at a time, from the tail hint to the
// ring's end and then from its start, not an entry at a time: a journal of
// seven entries across the ring's end reads back in two reads.
func TestReadingBackReadsTheRingAWindowAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		const ring = 64 << 10
		_, handle := device(t, ctx, runtime, ring, sim.DiskConfig{})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		var positions []uint64
		for i := range 10 {
			positions = append(positions, mustCommit(t, ctx, j, entryOf("vm-a", 1, byte(i), uint64(i)))...)
			if i == 3 {
				j.Trim("vm-a", map[uint64]uint64{1: positions[2]})
			}
		}
		if want := uint64(ring + 9*8192); positions[9] != want {
			t.Fatalf("the last entry is at %d, want %d, past the ring's end", positions[9], want)
		}
		if _, err := j.Close(ctx); err != nil {
			t.Fatal(err)
		}
		file := &gatedFile{File: handle()}
		again := open(t, ctx, file, Lease{Assigned: 2, Member: memberA})
		if got := again.Held(); len(got) != 1 || got[0].Entries != 7 {
			t.Fatalf("the journal read back %+v, want seven entries of vm-a", got)
		}
		if got := file.read(); got != 3 {
			t.Fatalf("opening the journal took %d reads, want 3: the header, and the ring on each side of its end", got)
		}
	})
}

// A commit given up while it waits for its batch is never captured.
func TestACommitGivenUpWhileItWaitsIsNeverCaptured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		file := &gatedFile{File: handle()}
		j := open(t, ctx, file, Lease{Assigned: 1, Member: memberA})
		t.Cleanup(file.release)
		reached := file.hold()
		a := make(chan error, 1)
		go func() {
			_, err := commitOf(ctx, j, entryOf("vm-a", 1, 0x10, 0))
			a <- err
		}()
		<-reached
		waiting, giveUp := context.WithCancel(ctx)
		captured := false
		b := make(chan error, 1)
		go func() {
			_, err := j.Commit(waiting, 4176, func(context.Context) ([]Entry, error) {
				captured = true
				return []Entry{entryOf("vm-b", 1, 0x20, 0)}, nil
			})
			b <- err
		}()
		synctest.Wait()
		giveUp()
		if err := <-b; !errors.Is(err, context.Canceled) {
			t.Fatalf("a commit given up: %v, want context.Canceled", err)
		}
		file.release()
		if err := <-a; err != nil {
			t.Fatal(err)
		}
		mustCommit(t, ctx, j, entryOf("vm-c", 1, 0x30, 0))
		if captured {
			t.Fatal("a commit given up while it waited was captured")
		}
	})
}

// A read returns the entries after the covered position it names, and only
// those. A reader must be at a newer epoch than the one it reads, and must
// name the journal's generation.
func TestAReadReturnsTheEntriesAfterTheCoveredPosition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		var entries []Entry
		for i := range 3 {
			e := entryOf("vm-a", 1, byte(0x10*(i+1)), uint64(i))
			entries = append(entries, at(e, mustCommit(t, ctx, j, e)[0]))
		}
		if diff := sameEntries(readAll(t, ctx, j, "vm-a", 1, entries[0].Position, 2), entries[1:]); diff != "" {
			t.Fatal(diff)
		}
		if got := readAll(t, ctx, j, "vm-a", 1, entries[2].Position, 2); len(got) != 0 {
			t.Fatalf("a read after the last entry returned %s", describe(got))
		}
		ignore := func(Entry) error { return nil }
		if err := j.Read(ctx, ReadRequest{VM: "vm-a", Epoch: 2, Generation: j.Generation(), Reader: 2},
			ignore); err == nil {
			t.Fatal("a reader read its own epoch")
		}
		if err := j.Read(ctx, ReadRequest{VM: "vm-a", Epoch: 1, Generation: j.Generation() + 1, Reader: 2},
			ignore); !errors.Is(err, ErrGeneration) {
			t.Fatalf("a read of another generation: %v, want ErrGeneration", err)
		}
	})
}

// A disk is formatted with a ring of whole blocks that fits it, under the
// generation the entropy draws. A disk too small for the smallest ring is
// refused.
func TestFormattingFitsTheRingToTheDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		_, uneven := deviceNamed(t, ctx, runtime, "uneven", minRingSize+BlockBytes+1000, sim.DiskConfig{})
		j := openWith(t, ctx, uneven(), Config{Lease: Lease{Assigned: 1, Member: memberA},
			Entropy: fixtureEntropy{}})
		if got := j.Usage(); got.Ring != minRingSize+BlockBytes || j.Generation() != fixtureGeneration {
			t.Fatalf("a disk of %d bytes was formatted with a ring of %d under %#x, want %d under %#x",
				ringOffset+minRingSize+BlockBytes+1000, got.Ring, j.Generation(), minRingSize+BlockBytes,
				uint64(fixtureGeneration))
		}
		_, smallest := deviceNamed(t, ctx, runtime, "smallest", minRingSize, sim.DiskConfig{})
		if got := open(t, ctx, smallest(), Lease{Assigned: 1, Member: memberA}).Usage().Ring; got != minRingSize {
			t.Fatalf("the smallest disk was formatted with a ring of %d, want %d", got, minRingSize)
		}
		_, small := deviceNamed(t, ctx, runtime, "small", minRingSize-8, sim.DiskConfig{})
		if _, err := Open(ctx, small(), Config{Identity: diskOne, Lease: Lease{Assigned: 1, Member: memberA}}); err == nil {
			t.Fatal("a disk too small for a ring was opened")
		}
	})
}

// A header is written into the slot that does not hold the current one, so a
// torn header write leaves the older header, and the journal reads back from
// it.
func TestATornHeaderWriteLeavesTheOlderHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, testRing, sim.DiskConfig{})
		j := open(t, ctx, handle(), Lease{Assigned: 1, Member: memberA})
		e := entryOf("vm-a", 1, 0x10, 0)
		positions := mustCommit(t, ctx, j, e)
		disk.TearNextWrite(40)
		if _, err := j.Close(ctx); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("closing a journal whose header write was torn: %v, want ErrInjectedFault", err)
		}
		again := open(t, ctx, handle(), Lease{Assigned: 2, Member: memberA})
		if again.Formatted() {
			t.Fatal("a torn header write left no header")
		}
		if diff := sameEntries(readAll(t, ctx, again, "vm-a", 1, 0, 2), []Entry{at(e, positions[0])}); diff != "" {
			t.Fatal(diff)
		}
	})
}
