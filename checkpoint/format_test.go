package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// update rewrites the committed fixtures from what this build writes. A format
// bump keeps every fixture that is there and adds one for the new version: what
// the old ones are worth is that their bytes are the ones the old build wrote,
// so a store written before the bump either still opens or is refused with the
// version it was written under named.
var update = flag.Bool("update", false, "rewrite the index fixtures under testdata")

const (
	// currentCheckpoint holds the objects of a small published checkpoint as
	// this build writes them, named for the two formats they are written in: an
	// index object of format 8, and parts of layout 4.
	currentCheckpoint = "testdata/index-8-part-4"
	fixturePrefix     = "fixture/"
)

// supersededRootTables are the committed index tables of formats this build no
// longer reads, each with the version it carries. They are never rewritten:
// what they are worth is that their bytes are the ones that version was written
// with, so -update adds a fixture and rewrites none of these.
var supersededRootTables = []struct {
	path    string
	version uint32
}{
	{path: "testdata/index-4/index", version: 4},
}

// supersededCheckpoints are the committed object dumps of whole checkpoints
// this build no longer reads, each with the refusal opening one must carry. A
// store written before a bump is refused with the version it carries named,
// which is the whole contract while nothing is deployed. The first two wrote
// the root as the whole of an index object and are named by that object's
// version; the third wrote it as the last member of a part and is named by that
// part's layout.
var supersededCheckpoints = []struct {
	dir     string
	refusal string
}{
	{dir: "testdata/index-5-part-1", refusal: "checkpoint index format version 5"},
	{dir: "testdata/index-6-part-2", refusal: "checkpoint index format version 6"},
	{dir: "testdata/part-3", refusal: "checkpoint part format version 3"},
	// Version 7 is the layout immediately before this one: its roots state no
	// volume's page size, so its page numbers are 2 MiB pages and nothing else
	// may read them. Nothing is converted — it is refused by the version it
	// carries, before a segment is decoded or a page is served.
	{dir: "testdata/index-7-part-4", refusal: "checkpoint index format version 7"},
}

// fixtureVM is the checkpoint the fixture publishes: a first checkpoint of the
// root alone, and two over it carrying VMM state, a page of each volume, and a
// page the second publishes as zeroes, which leaves the segment naming it.
var (
	fixtureRoot   = control.Ref{VM: "alpha", Sequence: control.Sequence(1, 1)}
	fixtureFirst  = control.Ref{VM: "alpha", Sequence: control.Sequence(1, 2)}
	fixtureSecond = control.Ref{VM: "alpha", Sequence: control.Sequence(1, 3)}
	fixtureSizes  = map[string]uint64{"disk": PageSize2MiB + SectorSize, "ram": 2 * PageSize2MiB}
)

// fixtureSource fills every page with a byte derived from its name, so what the
// fixture must read back as is stated rather than remembered.
type fixtureSource struct{ zero map[string]uint64 }

func (s fixtureSource) fill(volume string, page uint64) byte {
	if number, held := s.zero[volume]; held && number == page {
		return 0
	}
	return byte(len(volume)*16) + byte(page) + 1
}

func (s fixtureSource) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	for at := range dst {
		dst[at] = s.fill(volume, page)
	}
	return nil
}

// A checkpoint this build published opens back out of committed bytes: its
// root parses out of its index object, every part it names is there with the
// part count and the member bytes the root recorded, and every byte of every
// volume reads back as what was published.
func TestTheCommittedCheckpointFixtureOpens(t *testing.T) {
	if *update {
		writeCheckpointFixtures(t)
	}
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		loadFixtureObjects(t, store, filepath.Join(currentCheckpoint, "objects"))
		index, err := store.Open(t.Context(), fixtureSecond)
		if err != nil {
			t.Fatalf("the committed checkpoint does not open: %v", err)
		}
		keys, violations := store.CheckIndex(t.Context(), index)
		if len(violations) != 0 {
			t.Fatalf("the committed checkpoint is not whole: %v", violations)
		}
		if len(keys) < 2 {
			t.Fatalf("the committed checkpoint reaches %d objects", len(keys))
		}
		state, err := store.ReadState(t.Context(), index)
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != "the fixture's VMM state" {
			t.Fatalf("the committed checkpoint's VMM state is %q", state)
		}
		source := fixtureSource{zero: map[string]uint64{"disk": 1}}
		for _, name := range slices.Sorted(maps.Keys(fixtureSizes)) {
			size := fixtureSizes[name]
			if index.Size(name) != size {
				t.Fatalf("%s is %d bytes, want %d", name, index.Size(name), size)
			}
			got := make([]byte, size)
			if err := store.Read(t.Context(), index, name, 0, got); err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			want := make([]byte, size)
			for page := range (size + PageSize2MiB - 1) / PageSize2MiB {
				start, span := at2MiB.PageSpan(size, page)
				for at := start; at < start+span; at++ {
					want[at] = source.fill(name, page)
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s reads back as %#x..., want %#x...", name, got[:8], want[:8])
			}
		}
	})
}

// Nothing is deployed, so an index table written under a superseded version is
// refused rather than read, and the refusal names the version it was written
// under. A root carries no version of its own any more — the index object's
// header does — so what names these is the field they carry and this build's
// roots do not.
func TestARootOfASupersededVersionIsRefusedByVersion(t *testing.T) {
	for _, superseded := range supersededRootTables {
		data := readFixture(t, superseded.path)
		_, err := decodeRoot(nil, fixtureSecond, data)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("a version %d index table reported %v", superseded.version, err)
		}
		want := fmt.Sprintf("checkpoint index format version %d", superseded.version)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal reads %q, want it to name %q", err, want)
		}
	}
}

// A whole checkpoint written under a superseded set of formats is refused with
// the version that moved named: the index object's where the root was the whole
// of one, and the part layout's where the root was a member of a part.
func TestASupersededCheckpointFixtureIsRefusedByVersion(t *testing.T) {
	for _, superseded := range supersededCheckpoints {
		synctest.Test(t, func(t *testing.T) {
			store := fixtureStore(t)
			loadFixtureObjects(t, store, filepath.Join(superseded.dir, "objects"))
			_, err := store.Open(t.Context(), fixtureSecond)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("opening the %s checkpoint reported %v", superseded.dir, err)
			}
			if !strings.Contains(err.Error(), superseded.refusal) {
				t.Fatalf("the refusal reads %q, want it to name %q", err, superseded.refusal)
			}
		})
	}
}

// writeCheckpointFixtures publishes the fixture's checkpoints into a simulated
// store and writes every object out at the key it lives at. It writes only the
// current format: a superseded dump is worth keeping only as the bytes the
// build of that version wrote, so nothing here touches one.
func writeCheckpointFixtures(t *testing.T) {
	t.Helper()
	var objects []fixtureObject
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		root, err := store.Root(t.Context(), fixtureRoot, volumesAt(at2MiB, fixtureSizes))
		if err != nil {
			t.Fatal(err)
		}
		source := fixtureSource{}
		first := store.Begin(root, fixtureFirst)
		first.SetState([]byte("the fixture's VMM state"))
		for name, size := range fixtureSizes {
			for page := range (size + PageSize2MiB - 1) / PageSize2MiB {
				first.Dirty(name, page)
			}
		}
		firstIndex, err := first.Commit(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		// The second checkpoint republishes one page and erases another, so the
		// fixture holds a root naming a checkpoint that is not its own and a segment
		// the erased page has left.
		second := store.Begin(firstIndex, fixtureSecond)
		second.Dirty("disk", 0)
		second.Dirty("disk", 1)
		if _, err := second.Commit(t.Context(), fixtureSource{zero: map[string]uint64{"disk": 1}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Open(t.Context(), fixtureSecond); err != nil {
			t.Fatal(err)
		}
		objects = listFixtureObjects(t, store)
	})
	writeFixtureObjects(t, filepath.Join(currentCheckpoint, "objects"), objects)
}

// fixtureStore is the store every fixture is published through and read back
// out of: a simulated object store under one prefix.
func fixtureStore(t *testing.T) *Store {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(fixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Config{ObjectStore: sim.New(sim.Config{Seed: 103}).ObjectStore(), ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// fixtureObject is one object of a dump: the key it lives at and its bytes.
type fixtureObject struct {
	key  string
	data []byte
}

func listFixtureObjects(t *testing.T, store *Store) []fixtureObject {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(fixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	var found []fixtureObject
	token := ""
	for {
		page, err := store.objects.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			data, _, err := platform.ReadObject(t.Context(), store.objects, object.Key, 0, maximumPartSize, ErrCorrupt)
			if err != nil {
				t.Fatal(err)
			}
			found = append(found, fixtureObject{key: object.Key.String(), data: data})
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.SortFunc(found, func(a, b fixtureObject) int { return strings.Compare(a.key, b.key) })
	return found
}

func writeFixtureObjects(t *testing.T, dir string, objects []fixtureObject) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		writeFixture(t, filepath.Join(dir, filepath.FromSlash(object.key)), object.data)
	}
}

func loadFixtureObjects(t *testing.T, store *Store, dir string) {
	t.Helper()
	root := os.DirFS(dir)
	count := 0
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		key, err := platform.NewObjectKey(name)
		if err != nil {
			return err
		}
		_, err = store.objects.Put(t.Context(), platform.PutRequest{Key: key,
			Body: bytes.NewReader(data), Size: int64(len(data))})
		count++
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("the fixture at %s holds no objects; run go test ./checkpoint -update", dir)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./checkpoint -update to write the fixtures", err)
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
