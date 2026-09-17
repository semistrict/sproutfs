package part_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
)

// build writes the state member and two pages, which is the shape every
// checkpoint's first part has.
func build(t *testing.T) ([]byte, []part.Member) {
	t.Helper()
	var builder part.Builder
	want := []part.Member{
		{Volume: "", Page: 0, State: true},
		{Volume: "root", Page: 3},
		{Volume: "swap", Page: 9},
	}
	payloads := [][]byte{[]byte("registers and devices"), bytes.Repeat([]byte{7}, 4096), []byte("tail")}
	for index := range want {
		offset, length, err := builder.Add(t.Context(), want[index], payloads[index])
		if err != nil {
			t.Fatal(err)
		}
		want[index].Offset, want[index].Length = offset, length
	}
	sealed, err := builder.Seal(1)
	if err != nil {
		t.Fatal(err)
	}
	return sealed, want
}

// A sealed part describes itself: its trailer finds the table, the table names
// every member it holds, and each member's extent decodes to what was written.
func TestSealedPartNamesEveryMember(t *testing.T) {
	sealed, want := build(t)
	payloads := [][]byte{[]byte("registers and devices"), bytes.Repeat([]byte{7}, 4096), []byte("tail")}

	trailer, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	if trailer.Parts != 1 {
		t.Fatalf("the last part's trailer names %d parts, want 1", trailer.Parts)
	}
	offset, length := trailer.TableOffset, trailer.TableLength
	if offset+length != uint64(len(sealed)-part.TrailerSize) {
		t.Fatalf("the table at %d for %d bytes does not end where the trailer starts (%d)",
			offset, length, len(sealed)-part.TrailerSize)
	}
	got, err := part.DecodeTable(sealed[offset:offset+length], offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the table names %d members, want %d", len(got), len(want))
	}
	for index, member := range got {
		if member != want[index] {
			t.Fatalf("member %d is %+v, want %+v", index, member, want[index])
		}
		data, err := blob.Decode(t.Context(), sealed[member.Offset:member.Offset+member.Length], 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, payloads[index]) {
			t.Fatalf("member %d holds %d bytes, want %d", index, len(data), len(payloads[index]))
		}
	}
}

// The same members written twice produce the same bytes, which is what makes a
// retried publication idempotent against a create-if-absent store.
func TestSealingIsDeterministic(t *testing.T) {
	first, _ := build(t)
	second, _ := build(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two identical parts sealed to different bytes")
	}
}

// A part fills to the size it is sealed at and not before, in body bytes and in
// table bytes alike. A part of many small members is bounded by its table
// rather than by its body, which is what keeps the tail a reader fetches one
// read whatever a publication writes.
func TestFullReportsTheTargetSize(t *testing.T) {
	next := part.Member{Volume: "root"}
	var builder part.Builder
	if builder.Full(next, 1, 1<<20) {
		t.Fatal("an empty part reported itself full")
	}
	if _, _, err := builder.Add(t.Context(), next, []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	if !builder.Full(next, 1, 1<<20) {
		t.Fatal("a part holding a member reported itself unfilled")
	}

	var small part.Builder
	if _, _, err := small.Add(t.Context(), next, []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	if small.Full(next, 1<<20, 1<<20) {
		t.Fatal("a part holding one small member reported itself full")
	}
	if !small.Full(next, 1<<20, small.TableBytes()) {
		t.Fatal("a part whose table is already at its bound admitted another entry")
	}
}

// What a builder says its table costs is what the table it seals encodes to: it
// is the size a writer bounds a part by, so a part that measured itself wrong
// would write a tail its reader cannot fetch in one read.
func TestTableBytesIsWhatTheSealedTableCosts(t *testing.T) {
	var builder part.Builder
	for _, member := range []part.Member{
		{Volume: "", State: true},
		{Volume: "root", Page: 3},
		{Volume: "swap", Page: 9, OriginVM: "alpha", OriginSequence: 4098},
		{Volume: "root", Page: 1},
	} {
		if _, _, err := builder.Add(t.Context(), member, bytes.Repeat([]byte{3}, 128)); err != nil {
			t.Fatal(err)
		}
	}
	stated := builder.TableBytes()
	sealed, err := builder.Seal(1)
	if err != nil {
		t.Fatal(err)
	}
	trailer, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	if uint64(stated) != trailer.TableLength {
		t.Fatalf("the builder stated a table of %d bytes and sealed one of %d", stated, trailer.TableLength)
	}
}

func TestDecodeTrailerRejectsATailThatIsNotOne(t *testing.T) {
	sealed, _ := build(t)
	size := uint64(len(sealed))
	trailer := bytes.Clone(sealed[len(sealed)-part.TrailerSize:])
	cases := map[string]func(){
		"wrong magic":         func() { binary.LittleEndian.PutUint64(trailer[24:], 0) },
		"table past the end":  func() { binary.LittleEndian.PutUint64(trailer[0:], size) },
		"table too long":      func() { binary.LittleEndian.PutUint64(trailer[8:], size) },
		"another part layout": func() { binary.LittleEndian.PutUint32(trailer[16:], part.FormatVersion+1) },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			copy(trailer, sealed[len(sealed)-part.TrailerSize:])
			corrupt()
			if _, err := part.DecodeTrailer(trailer, size); !errors.Is(err, part.ErrCorrupt) {
				t.Fatalf("decoded a corrupt trailer: %v", err)
			}
		})
	}
	if _, err := part.DecodeTrailer(trailer[:part.TrailerSize-1], size); !errors.Is(err, part.ErrCorrupt) {
		t.Fatalf("decoded a short trailer: %v", err)
	}
}

func TestDecodeTableRejectsAMemberThePartDoesNotHold(t *testing.T) {
	sealed, _ := build(t)
	trailer, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	offset := trailer.TableOffset
	table := sealed[offset : offset+trailer.TableLength]
	// The body is what bounds a member, so a table read as if the part held one
	// byte fewer than the first member needs must reject it.
	if _, err := part.DecodeTable(table, 0); !errors.Is(err, part.ErrCorrupt) {
		t.Fatalf("decoded a table whose members lie outside the part: %v", err)
	}
	if _, err := part.DecodeTable([]byte{0xff, 0xff, 0xff}, offset); !errors.Is(err, part.ErrCorrupt) {
		t.Fatalf("decoded a table that is not a message: %v", err)
	}
}

// The VMM state and the root are the members that have no volume, so a table
// that says otherwise describes a part this store did not write.
func TestDecodeTableRejectsStateAndVolumeDisagreeing(t *testing.T) {
	var builder part.Builder
	if _, _, err := builder.Add(t.Context(), part.Member{Volume: "root", Page: 1, State: true}, []byte("state and a volume")); err != nil {
		t.Fatal(err)
	}
	sealed, err := builder.Seal(1)
	if err != nil {
		t.Fatal(err)
	}
	trailer, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	table := sealed[trailer.TableOffset : trailer.TableOffset+trailer.TableLength]
	if _, err := part.DecodeTable(table, trailer.TableOffset); !errors.Is(err, part.ErrCorrupt) {
		t.Fatalf("decoded a state member that names a volume: %v", err)
	}
}
