package control

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// update rewrites the committed fixtures from what this build writes. A format
// bump keeps every fixture that is there and adds one for the new version: what
// the old ones are worth is that their bytes are the ones the old build wrote,
// so a store written before the bump either still parses or is refused with the
// version it was written under named.
var update = flag.Bool("update", false, "rewrite the record fixtures under testdata")

// fixtureRecords are the records the committed fixtures hold: one VM forked at
// two of its own checkpoints, in two different writer epochs, and one forked at
// the checkpoint it still selects.
var fixtureRecords = []Record{
	{
		VM:    "alpha",
		Epoch: 3,
		Nonce: []byte{0x9f, 0x2a, 0x00, 0x11, 0x7c, 0xd3, 0x45, 0x68,
			0xba, 0x01, 0xee, 0x5c, 0x30, 0x92, 0xaf, 0x77},
		Selected: Sequence(3, 2),
		Created:  true,
		Pinned:   []uint64{Sequence(1, 4), Sequence(3, 1)},
	},
	{
		VM:    "ghost",
		Epoch: MinimumEpoch,
		Nonce: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		Selected: Sequence(MinimumEpoch, 7),
		Created:  true,
		Pinned:   []uint64{Sequence(MinimumEpoch, 7)},
	},
}

// currentRecords is where this build's own records live. supersededRecords is
// every version committed before it, newest first, each holding the bytes the
// build of that version actually wrote: a bump adds a directory and rewrites
// none of them, because bytes this build produced and restamped would prove
// nothing about what an older build wrote.
const currentRecords = "testdata/record-4"

var supersededRecords = []struct {
	dir     string
	version uint32
}{
	{dir: "testdata/record-3", version: 3},
	{dir: "testdata/record-2", version: 2},
}

// A record this build wrote parses back into exactly the record it was written
// from, out of committed bytes rather than out of a round trip in memory.
func TestTheCommittedRecordFixtureParses(t *testing.T) {
	if *update {
		writeRecordFixtures(t)
	}
	for _, want := range fixtureRecords {
		data := readFixture(t, filepath.Join(currentRecords, want.VM))
		got, err := unmarshalRecord(want.VM, data)
		if err != nil {
			t.Fatalf("the committed record of %s does not parse: %v", want.VM, err)
		}
		if !got.equal(want) {
			t.Fatalf("the committed record of %s parsed as %+v, want %+v", want.VM, got, want)
		}
	}
}

// Nothing is deployed, so a store written under a superseded version is refused
// rather than read: the refusal names the version it was written under, which is
// what an operator needs to know which build wrote it.
func TestARecordOfASupersededVersionIsRefusedByVersion(t *testing.T) {
	if *update {
		writeRecordFixtures(t)
	}
	for _, superseded := range supersededRecords {
		for _, record := range fixtureRecords {
			data := readFixture(t, filepath.Join(superseded.dir, record.VM))
			_, err := unmarshalRecord(record.VM, data)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("a version %d record of %s reported %v", superseded.version, record.VM, err)
			}
			want := fmt.Sprintf("record format version %d, want %d", superseded.version, formatVersion)
			if got := err.Error(); !strings.Contains(got, want) {
				t.Fatalf("the refusal of a version %d record reads %q, want it to name %q",
					superseded.version, got, want)
			}
		}
	}
}

// writeRecordFixtures writes this build's own records. The superseded
// directories are never written: what they are worth is that their bytes are
// the ones the build of that version wrote.
func writeRecordFixtures(t *testing.T) {
	t.Helper()
	for _, record := range fixtureRecords {
		data, err := record.marshal()
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(currentRecords, record.VM), data)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/control -update to write the fixtures", err)
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

// The fixtures are the whole of what is committed: a directory holding a file
// this build no longer writes is a fixture nothing checks.
func TestTheRecordFixtureDirectoriesHoldOnlyTheirRecords(t *testing.T) {
	want := make([]string, 0, len(fixtureRecords))
	for _, record := range fixtureRecords {
		want = append(want, record.VM)
	}
	slices.Sort(want)
	dirs := []string{currentRecords}
	for _, superseded := range supersededRecords {
		dirs = append(dirs, superseded.dir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s holds %v, want %v", dir, got, want)
		}
	}
}
