package part_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
)

// update rewrites the committed fixtures from what this build writes. A layout
// bump keeps every fixture that is there and adds one for the new version: what
// the old ones are worth is that their bytes are the ones the old build wrote.
var update = flag.Bool("update", false, "rewrite the part fixtures under testdata")

// currentPart holds one sealed part as this build writes it.
const currentPart = "testdata/part-4"

// supersededParts are the committed parts of layouts this build no longer
// reads. They are never rewritten: what they are worth is that their bytes are
// the ones that layout was written with, so -update adds a fixture and rewrites
// none of these. A part whose own table is of another version than its trailer
// says — which is how the first of these was made, before there was a real
// older layout — carries that table in a file of its own; trailer is the size
// of the tail the layout wrote, which is what says where in the part the rest
// of it ends.
var supersededParts = []struct {
	dir     string
	version uint32
	trailer int
	table   string
}{
	{dir: "testdata/part-0", version: 0, table: "table"},
	{dir: "testdata/part-1", version: 1, trailer: 32},
	{dir: "testdata/part-2", version: 2, trailer: 32},
	{dir: "testdata/part-3", version: 3, trailer: 48},
}

// fixturePages are the pages the committed part holds, by volume and number.
var fixturePages = []struct {
	volume string
	page   uint64
	fill   byte
	length int
}{
	{volume: "disk", page: 0, fill: 0x5a, length: 4096},
	{volume: "disk", page: 2, fill: 0xa5, length: 4096},
	{volume: "ram", page: 1, fill: 0x3c, length: 8192},
}

// fixtureState is the VMM state member, which with the pages is the whole of
// what a part holds. What it holds is the store's business, not this package's,
// so the fixture's are opaque bytes.
var fixtureState = []byte("the fixture's VMM state")

// A part this build sealed describes itself back exactly: its trailer says
// where its table is and how many parts the checkpoint has, its table names
// every member at the extent it landed at, and every member decodes to the
// bytes it was written from — out of committed bytes rather than a round trip
// in memory.
func TestTheCommittedPartDescribesItself(t *testing.T) {
	if *update {
		writePartFixtures(t)
	}
	sealed := readFixture(t, filepath.Join(currentPart, "part"))
	trailer, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
	if err != nil {
		t.Fatalf("the committed part's trailer: %v", err)
	}
	if trailer.Parts != 1 {
		t.Fatalf("the committed part says the checkpoint has %d parts, want 1", trailer.Parts)
	}
	members, err := part.DecodeTable(sealed[trailer.TableOffset:trailer.TableOffset+trailer.TableLength],
		trailer.TableOffset)
	if err != nil {
		t.Fatalf("the committed part's table: %v", err)
	}
	want := []part.Member{{State: true}}
	for _, page := range fixturePages {
		want = append(want, part.Member{Volume: page.volume, Page: page.page})
	}
	if len(members) != len(want) {
		t.Fatalf("the committed part names %d members, want %d", len(members), len(want))
	}
	for at, member := range members {
		if member.Volume != want[at].Volume || member.Page != want[at].Page ||
			member.State != want[at].State {
			t.Fatalf("member %d is %+v, want %+v", at, member, want[at])
		}
	}
	decode := func(member part.Member) []byte {
		t.Helper()
		data, err := blob.Default().Decode(t.Context(), sealed[member.Offset:member.Offset+member.Length], 1<<20)
		if err != nil {
			t.Fatalf("decoding member %+v: %v", member, err)
		}
		return data
	}
	if got := decode(members[0]); !bytes.Equal(got, fixtureState) {
		t.Fatalf("the state member decodes to %q, want %q", got, fixtureState)
	}
	for at, page := range fixturePages {
		got := decode(members[at+1])
		if !bytes.Equal(got, bytes.Repeat([]byte{page.fill}, page.length)) {
			t.Fatalf("%s page %d decodes to %d bytes of %#x", page.volume, page.page, len(got), got[0])
		}
	}
}

// Nothing is deployed, so a part written under another layout is refused rather
// than read, and the refusal names the layout it was written under. The trailer
// is what refuses it first, before anything of the part is parsed; the table
// carries the same version so a part reached some other way is refused too.
func TestAPartOfASupersededLayoutIsRefusedByVersion(t *testing.T) {
	for _, superseded := range supersededParts {
		sealed := readFixture(t, filepath.Join(superseded.dir, "part"))
		_, err := part.DecodeTrailer(sealed[len(sealed)-part.TrailerSize:], uint64(len(sealed)))
		if !errors.Is(err, part.ErrCorrupt) {
			t.Fatalf("a version %d part reported %v", superseded.version, err)
		}
		want := fmt.Sprintf("part format version %d, want %d", superseded.version, part.FormatVersion)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the trailer's refusal reads %q, want it to name %q", err, want)
		}
		var table []byte
		if superseded.table != "" {
			table = readFixture(t, filepath.Join(superseded.dir, superseded.table))
		} else {
			table = tableOf(sealed, superseded.trailer)
		}
		_, err = part.DecodeTable(table, uint64(len(table)))
		if !errors.Is(err, part.ErrCorrupt) {
			t.Fatalf("a version %d table reported %v", superseded.version, err)
		}
		want = fmt.Sprintf("part table format version %d, want %d", superseded.version, part.FormatVersion)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the table's refusal reads %q, want it to name %q", err, want)
		}
	}
}

// tableOf slices a part's table out of it at the extent a trailer of the given
// size states, without the version check DecodeTrailer makes: a part of a
// superseded layout is refused by that check, and its own table is what the
// next refusal is about. Every layout has put the table's extent at the front of
// its trailer, so the size of that trailer is all this needs.
func tableOf(sealed []byte, size int) []byte {
	trailer := sealed[len(sealed)-size:]
	offset := binary.LittleEndian.Uint64(trailer[0:])
	length := binary.LittleEndian.Uint64(trailer[8:])
	return sealed[offset : offset+length]
}

// writePartFixtures seals one part holding both kinds of member a checkpoint
// writes — the VMM state and pages of two volumes — and writes it as this build
// seals it. It writes only the current layout: a superseded part is worth
// keeping only as the bytes the build of that version wrote.
func writePartFixtures(t *testing.T) {
	t.Helper()
	builder := part.NewBuilder(nil)
	if _, _, err := builder.Add(t.Context(), part.Member{State: true}, fixtureState); err != nil {
		t.Fatal(err)
	}
	for _, page := range fixturePages {
		member := part.Member{Volume: page.volume, Page: page.page}
		if _, _, err := builder.Add(t.Context(), member, bytes.Repeat([]byte{page.fill}, page.length)); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := builder.Seal(1)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(currentPart, "part"), sealed)
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/checkpoint/internal/part -update to write the fixtures", err)
	}
	return data
}

func writeFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
