package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// update rewrites the committed fixture from what this build writes. A format
// bump keeps every fixture that is there and adds one for the new version.
var update = flag.Bool("update", false, "rewrite the journal fixture under testdata")

// currentDevice is a journal disk as this build writes it: a ring of 48 KiB
// whose entries have wrapped past its end.
const currentDevice = "testdata/journal-1/device"

const (
	fixtureRing       = 48 << 10
	fixtureGeneration = 0x5346_4a47_0000_0001
)

// fixtureEntropy draws the fixture's generation.
type fixtureEntropy struct{}

func (fixtureEntropy) Fill(b []byte) {
	for i := range b {
		b[i] = byte(i)
	}
}

func (fixtureEntropy) Uint64() uint64 { return fixtureGeneration }

// The fixture's live entries, as they read back.
var (
	fixtureA = at(entryOf("vm-a", 1, 0x40, 3), fixtureRing+32<<10)
	fixtureC = at(entryOf("vm-c", 3, 0x70, 9, 10), fixtureRing+48<<10)
)

// writeFixture writes the fixture: four entries of vm-a and one of vm-b, a
// checkpoint that covers all but vm-a's last, and an entry of vm-c that does
// not fit before the ring's end and goes after a pad, at its start.
func writeFixture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		disk, handle := device(t, ctx, runtime, fixtureRing, sim.DiskConfig{})
		j, err := Open(ctx, handle(), Config{Identity: diskOne, Lease: Lease{Assigned: 1, Member: memberA},
			Entropy: fixtureEntropy{}})
		if err != nil {
			t.Fatal(err)
		}
		var a []uint64
		for i := range 3 {
			a = append(a, mustCommit(t, ctx, j, entryOf("vm-a", 1, byte(0x10*(i+1)), uint64(i)))...)
			if i == 0 {
				mustCommit(t, ctx, j, entryOf("vm-b", 1, 0x20, 5))
			}
		}
		j.Trim("vm-a", map[uint64]uint64{1: a[2]})
		j.Trim("vm-b", nil)
		a4 := mustCommit(t, ctx, j, entryOf("vm-a", 1, 0x40, 3))
		c := mustCommit(t, ctx, j, entryOf("vm-c", 3, 0x70, 9, 10))
		if a4[0] != fixtureA.Position || c[0] != fixtureC.Position {
			t.Fatalf("the fixture's entries are at %d and %d, want %d and %d", a4[0], c[0], fixtureA.Position,
				fixtureC.Position)
		}
		if _, err := j.Close(ctx); err != nil {
			t.Fatal(err)
		}
		file, err := disk.Open(ctx, "device", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		image := make([]byte, ringOffset+fixtureRing)
		if err := readFull(ctx, file, image, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(currentDevice), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(currentDevice, image, 0o644); err != nil {
			t.Fatal(err)
		}
	})
}

// fixtureDevice is a simulated journal disk of that name holding image.
func fixtureDevice(t *testing.T, ctx context.Context, name string, image []byte) platform.File {
	t.Helper()
	_, handle := deviceNamed(t, ctx, sim.RuntimeFrom(ctx), name, int64(len(image))-ringOffset, sim.DiskConfig{})
	file := handle()
	if _, err := file.WriteAt(ctx, image, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	return file
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	image, err := os.ReadFile(currentDevice)
	if err != nil {
		t.Fatalf("reading the committed fixture (write it with -update): %v", err)
	}
	return image
}

// A journal disk this build wrote reads back, from committed bytes, under a
// newer lease: the entries the checkpoint did not cover, the one after the
// pad at the ring's end included, at their positions, with their blocks.
func TestTheCommittedJournalReadsBack(t *testing.T) {
	if *update {
		writeFixture(t)
	}
	image := readFixture(t)
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		j := open(t, ctx, fixtureDevice(t, ctx, "committed", image), Lease{Assigned: 2, Member: memberB})
		if j.Formatted() || j.Generation() != fixtureGeneration {
			t.Fatalf("the committed journal opened formatted %v under generation %#x, want %#x", j.Formatted(),
				j.Generation(), uint64(fixtureGeneration))
		}
		want := []Held{
			{VM: "vm-a", Epoch: 1, Entries: 1, Bytes: fixtureA.size(), First: fixtureA.Position,
				Last: fixtureA.Position},
			{VM: "vm-c", Epoch: 3, Entries: 1, Bytes: fixtureC.size(), First: fixtureC.Position,
				Last: fixtureC.Position},
		}
		if got := j.Held(); !slices.Equal(got, want) {
			t.Fatalf("the committed journal holds %+v, want %+v", got, want)
		}
		if diff := sameEntries(readAll(t, ctx, j, "vm-a", 1, 0, 2), []Entry{fixtureA}); diff != "" {
			t.Fatal(diff)
		}
		if diff := sameEntries(readAll(t, ctx, j, "vm-c", 3, 0, 4), []Entry{fixtureC}); diff != "" {
			t.Fatal(diff)
		}
	})
}

// restamp sets the version byte of what lies at offset and rewrites its
// checksum, which covers checked bytes before it.
func restamp(image []byte, offset, checked int, version byte) {
	image[offset+4] = version
	sum := checksum(image[offset : offset+checked])
	copy(image[offset+checked:], sum[:])
}

// A journal disk whose header is of another version is refused, with the
// version named, rather than formatted over. An entry of another version
// ends reading back.
func TestAJournalOfAnotherVersionIsRefusedByName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		image := readFixture(t)
		restamp(image, 0, 88, 2)
		_, err := Open(ctx, fixtureDevice(t, ctx, "header-of-version-2", image), Config{Identity: diskOne,
			Lease: Lease{Assigned: 2, Member: memberB}})
		if !errors.Is(err, ErrVersion) || !strings.Contains(err.Error(), "version 2") {
			t.Fatalf("opening a journal whose header is of version 2: %v, want ErrVersion naming it", err)
		}

		image = readFixture(t)
		first := ringOffset + int(fixtureA.Position%fixtureRing)
		restamp(image, first, int(fixtureA.size())-checksumBytes, 2)
		j := open(t, ctx, fixtureDevice(t, ctx, "entry-of-version-2", image), Lease{Assigned: 2, Member: memberB})
		if got := j.Held(); len(got) != 0 {
			t.Fatalf("a journal whose first entry is of version 2 read back %+v, want nothing", got)
		}
	})
}

// Every entry decodes to what was encoded, and a change to any one of its
// bytes, or the loss of any of its end, makes it no entry.
func FuzzAnEntryReadsBackOnlyAsItWasWritten(f *testing.F) {
	f.Add("vm-a", "disk", uint64(1), uint8(1), uint64(fixtureRing), byte(0x5a), uint32(0), byte(1), uint32(0))
	f.Add("", "", uint64(0), uint8(0), uint64(0), byte(0), uint32(47), byte(0x80), uint32(63))
	f.Add("a VM with a long identity", "a volume", uint64(1<<40), uint8(3), uint64(1<<50), byte(0xff),
		uint32(4200), byte(0x01), uint32(4096))
	f.Fuzz(func(t *testing.T, vm, volume string, epoch uint64, blocks uint8, position uint64, fill byte,
		flip uint32, xor byte, short uint32) {
		vm, volume = vm[:min(len(vm), 300)], volume[:min(len(volume), 300)]
		e := entryOf(vm, epoch, fill)
		e.Volume = volume
		for i := range int(blocks % 4) {
			e.Blocks = append(e.Blocks, position+uint64(i))
			e.Data = append(e.Data, bytes.Repeat([]byte{fill + byte(i)}, BlockBytes)...)
		}
		encoded := appendEntry(nil, e, position, fixtureGeneration)
		if int64(len(encoded)) != e.size() {
			t.Fatalf("an entry of %d bytes encoded to %d", e.size(), len(encoded))
		}
		h, got, err := decodeEntry(encoded)
		if err != nil {
			t.Fatalf("decoding an entry as it was written: %v", err)
		}
		if h.generation != fixtureGeneration || got.Position != position || !sameContent(got, e) {
			t.Fatalf("an entry decoded to %s under generation %#x, want %s at %d", describe([]Entry{got}),
				h.generation, describe([]Entry{e}), position)
		}
		if xor == 0 {
			xor = 0x80
		}
		changed := slices.Clone(encoded)
		changed[int(flip)%len(changed)] ^= xor
		if _, _, err := decodeEntry(changed); err == nil {
			t.Fatalf("an entry with byte %d changed decoded", int(flip)%len(changed))
		}
		if _, _, err := decodeEntry(encoded[:int(short)%len(encoded)]); err == nil {
			t.Fatalf("the first %d bytes of an entry decoded", int(short)%len(encoded))
		}
	})
}

// A header slot decodes to what was encoded, and a change to any one of its
// bytes makes it no header.
func FuzzAHeaderSlotReadsBackOnlyAsItWasWritten(f *testing.F) {
	f.Add(true, uint64(1), uint64(fixtureGeneration), int64(fixtureRing), uint64(fixtureRing), uint64(1),
		uint32(0), byte(1))
	f.Add(false, uint64(1<<60), uint64(0), int64(1<<35), uint64(1<<40), uint64(0), uint32(103), byte(0x80))
	f.Fuzz(func(t *testing.T, empty bool, counter, generation uint64, length int64, tail, assigned uint64,
		flip uint32, xor byte) {
		s := slot{empty: empty, counter: counter, identity: diskOne, generation: generation, ringStart: ringOffset,
			ringLength: length, tail: tail, lease: Lease{Assigned: assigned, Member: memberB}}
		encoded := encodeSlot(s)
		got, err := decodeSlot(encoded)
		if err != nil || got != s {
			t.Fatalf("a slot decoded to %+v (%v), want %+v", got, err, s)
		}
		if xor == 0 {
			xor = 0x80
		}
		encoded[int(flip)%slotLength] ^= xor
		if _, err := decodeSlot(encoded); err == nil {
			t.Fatalf("a slot with byte %d changed decoded", int(flip)%slotLength)
		}
	})
}

// A pad decodes at every length the writer gives one, from a pad's head and
// checksum alone to the longest thing the ring holds. One shorter than that,
// of a length that is not a multiple of 8, or longer, is no entry.
func TestAPadDecodesAtEveryLengthTheWriterGivesIt(t *testing.T) {
	for _, length := range []int64{minEntryBytes, minEntryBytes + 8, BlockBytes + 56, maxStoredBytes} {
		h, err := checkEntry(appendPad(nil, length, fixtureRing, fixtureGeneration))
		if err != nil || h.kind != kindPad || h.length != length {
			t.Fatalf("a pad of %d bytes reads back as %+v (%v)", length, h, err)
		}
	}
	if _, err := checkEntry(appendPad(nil, maxStoredBytes+8, fixtureRing, fixtureGeneration)); err == nil {
		t.Fatalf("a pad of %d bytes decoded", maxStoredBytes+8)
	}
	for _, length := range []int{56, 68} {
		b := appendPad(nil, 72, fixtureRing, fixtureGeneration)[:length]
		binary.LittleEndian.PutUint32(b[32:], uint32(length))
		sum := checksum(b[:length-checksumBytes])
		copy(b[length-checksumBytes:], sum[:])
		if _, err := checkEntry(b); err == nil {
			t.Fatalf("a pad of %d bytes decoded", length)
		}
	}
}

// An entry decodes at the largest size a commit may give it. One 8 bytes
// larger is refused as it is captured and as it is read back.
func TestTheLargestEntryDecodes(t *testing.T) {
	e := sized("vm-a", 1, 0x10, 4086, MaxEntryBytes)
	if err := e.check(); err != nil {
		t.Fatal(err)
	}
	_, got, err := decodeEntry(appendEntry(nil, e, fixtureRing, fixtureGeneration))
	if err != nil || !sameContent(got, e) {
		t.Fatalf("the largest entry decoded to %s (%v)", describe([]Entry{got}), err)
	}
	larger := sized("vm-a", 1, 0x10, 4086, MaxEntryBytes+8)
	if err := larger.check(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("checking an entry of %d bytes: %v, want ErrTooLarge", larger.size(), err)
	}
	if _, _, err := decodeEntry(appendEntry(nil, larger, fixtureRing, fixtureGeneration)); err == nil {
		t.Fatalf("an entry of %d bytes decoded", larger.size())
	}
}

// An entry whose names are padded with other than zeros is refused, though
// its checksum holds.
func TestAnEntryWhoseNamesArePaddedWithOtherThanZerosIsRefused(t *testing.T) {
	e := entryOf("vm-a", 1, 0x10, 0)
	e.Volume = "disk-12345"
	b := appendEntry(nil, e, fixtureRing, fixtureGeneration)
	b[entryHeadBytes+len(e.VM)+len(e.Volume)] = 1
	sum := checksum(b[:len(b)-checksumBytes])
	copy(b[len(b)-checksumBytes:], sum[:])
	if _, _, err := decodeEntry(b); !errors.Is(err, errNoEntry) || errors.Is(err, errTorn) {
		t.Fatalf("an entry whose names are padded with a one: %v, want errNoEntry", err)
	}
}

// An entry the format cannot hold is refused as it is captured.
func TestAnEntryTheFormatCannotHoldIsRefused(t *testing.T) {
	long := entryOf(strings.Repeat("v", maxNameBytes+1), 1, 0x10, 0)
	if err := long.check(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("checking an entry whose VM identity is %d bytes: %v, want ErrTooLarge", len(long.VM), err)
	}
	short := entryOf("vm-a", 1, 0x10, 0, 1)
	short.Data = short.Data[:BlockBytes]
	if err := short.check(); err == nil || errors.Is(err, ErrTooLarge) {
		t.Fatalf("checking an entry of two blocks that holds one: %v, want an error", err)
	}
}

// A disk whose header names a ring that does not fit it is refused rather
// than formatted over.
func TestAHeaderWhoseRingDoesNotFitTheDiskIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		image := readFixture(t)[:ringOffset+fixtureRing/2]
		_, err := Open(ctx, fixtureDevice(t, ctx, "shorter", image), Config{Identity: diskOne,
			Lease: Lease{Assigned: 2, Member: memberB}})
		if err == nil || !strings.Contains(err.Error(), "does not fit") {
			t.Fatalf("opening a disk shorter than its header's ring: %v, want a refusal", err)
		}
	})
}
