package volume_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// update rewrites the committed fixture from what this build writes. A format
// bump keeps every fixture that is there and adds one named for the new set:
// what the old ones prove is that a store written before the bump still opens,
// or is refused with the version named, which is only worth anything while
// their bytes are the ones the old build actually wrote.
var update = flag.Bool("update", false, "rewrite the format fixtures under testdata")

// fixtureDeployment is the committed dump of a small deployment, named for the
// three formats its objects are written in: the control record, the checkpoint
// index object and the part layout. Each format bump adds a directory beside
// it, and supersededDeployments are the dumps committed before this one, each
// with the version it was written under — the bytes an older build actually
// wrote, which is the only thing that makes them worth keeping.
const fixtureDeployment = "testdata/deployment-record-5-index-8-part-4"

var supersededDeployments = []struct {
	dir string
	// sentinel is the package whose corruption the refusal is, and want the
	// text it must carry: whichever of the formats moved is what refuses the
	// dump, and it names the version the dump was written under.
	sentinel error
	want     string
}{
	// Opening a VM reads its control record before anything the record names,
	// so every dump written before record format 5 is refused by its record.
	{dir: "testdata/deployment-record-3-index-5-part-1", sentinel: control.ErrCorrupt,
		want: "record format version 3, want 5"},
	{dir: "testdata/deployment-record-4-index-5-part-1", sentinel: control.ErrCorrupt,
		want: "record format version 4, want 5"},
	{dir: "testdata/deployment-record-4-index-6-part-2", sentinel: control.ErrCorrupt,
		want: "record format version 4, want 5"},
	{dir: "testdata/deployment-record-4-part-3", sentinel: control.ErrCorrupt,
		want: "record format version 4, want 5"},
	{dir: "testdata/deployment-record-4-index-7-part-4", sentinel: control.ErrCorrupt,
		want: "record format version 4, want 5"},
	// The set before this one: its records keep no checkpoint.
	{dir: "testdata/deployment-record-4-index-8-part-4", sentinel: control.ErrCorrupt,
		want: "record format version 4, want 5"},
}

// objectsDir and manifestFile are the two halves of a fixture: the store's
// objects under the keys they live at, and what every volume of every VM must
// read back as.
const (
	objectsDir   = "objects"
	manifestFile = "contents"
)

// fixtureSpecs are deliberately small — one volume shorter than a page, one a
// page and a tail — so the fixture is a few kilobytes and every page of it can
// be read back and compared byte for byte.
var fixtureSpecs = []volume.VolumeSpec{
	{Name: "disk", Size: 3 * checkpoint.SectorSize, PageSize: checkpoint.PageSize2MiB},
	{Name: "ram", Size: checkpoint.SectorSize, PageSize: checkpoint.PageSize2MiB},
}

const (
	fixturePrefix = "sproutfs/"
	fixtureState  = "vmm state of the fixture"
)

// A store written by this build must still open when this build is the one that
// reads it back: the fixture is the committed bytes of a small deployment, and
// the test opens every VM in it, reads every byte of every volume, reads the
// VMM state the checkpoint carries and runs the whole-deployment check over it.
//
// It is what a format bump is measured against. The fixture of every superseded
// formats stays committed and keeps its own test, so a store written before the
// bump either still opens or is refused with the version it was written under
// named — which is the contract, because nothing is deployed.
func TestTheCommittedDeploymentFixtureOpens(t *testing.T) {
	if *update {
		writeDeploymentFixture(t)
	}
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 101)
		loadObjects(t, h, filepath.Join(fixtureDeployment, objectsDir))
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatalf("the committed fixture is not a consistent deployment:\n%v", err)
		}
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		want := readManifest(t, filepath.Join(fixtureDeployment, manifestFile))
		var got []string
		for _, id := range []string{"alpha", "beta"} {
			vm, err := manager.Open(t.Context(), id)
			if err != nil {
				t.Fatalf("opening %s from the fixture: %v", id, err)
			}
			for _, v := range vm.Volumes() {
				data := make([]byte, v.Size())
				if err := v.Read(t.Context(), 0, data); err != nil {
					t.Fatalf("reading %s of %s: %v", v.Name(), id, err)
				}
				got = append(got, digestLine(id, v.Name(), data))
			}
			got = append(got, digestLine(id, "vmm-state", readState(t, h, vm)))
			if err := vm.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the fixture reads back as\n%s\nwant\n%s",
				strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// digestLine is one manifest entry: what a volume of a VM reads back as, by
// length and digest, so a mismatch names which volume moved.
func digestLine(vm, name string, data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s %s %d %s", vm, name, len(data), hex.EncodeToString(sum[:]))
}

// readState reads the VMM state the VM's selected checkpoint carries, which is
// the one member of a part that is not a page.
func readState(t *testing.T, h *harness, vm *volume.VM) []byte {
	t.Helper()
	store := h.imageStore(t, h.objects)
	index, err := store.Open(t.Context(), vm.Status().Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadState(t.Context(), index)
	if err != nil {
		t.Fatalf("reading the VMM state of %s: %v", vm.ID(), err)
	}
	return state
}

// writeDeploymentFixture builds the deployment the fixture holds and writes it
// out: a VM with a history of checkpoints and VMM state, whose record keeps its
// first capture and pins the point it was forked at, and a fork of it whose
// first checkpoint names that point's checkpoints. Between them they exercise every object the formats
// describe. It writes only the current set: a superseded dump is worth keeping
// only as the bytes the build of that version wrote.
func writeDeploymentFixture(t *testing.T) {
	t.Helper()
	var objects []fixtureObject
	var manifest []string
	synctest.Test(t, func(t *testing.T) {
		h := newSeededHarness(t, 101)
		manager := h.manager(t, h.config())
		alpha, err := manager.Create(t.Context(), "alpha", fixtureSpecs)
		if err != nil {
			t.Fatal(err)
		}
		want := newModel(fixtureSpecs)
		fill := func(vm *volume.VM, m model, name string, offset uint64, value byte) {
			t.Helper()
			data := bytes.Repeat([]byte{value}, checkpoint.SectorSize)
			if err := vm.Volume(name).Write(t.Context(), offset, data); err != nil {
				t.Fatal(err)
			}
			copy(m[name][offset:], data)
		}
		fill(alpha, want, "disk", 0, 0x11)
		fill(alpha, want, "ram", 0, 0x22)
		capture, err := alpha.Snapshot(t.Context(), volume.Prepared([]byte(fixtureState), nil), volume.Terms{Keep: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := capture.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		// A second checkpoint leaves the first one's parts still read, which is
		// what makes the root name a checkpoint that is not itself.
		fill(alpha, want, "disk", checkpoint.SectorSize, 0x33)
		if err := alpha.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := alpha.ForkPoint(t.Context(), volume.Prepared([]byte(fixtureState), nil))
		if err != nil {
			t.Fatal(err)
		}
		beta, err := manager.Fork(t.Context(), "beta", point)
		if err != nil {
			t.Fatal(err)
		}
		forked := clone(want)
		fill(beta, forked, "ram", 0, 0x44)
		if err := beta.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The parent moves on after the fork, so its later checkpoints and the
		// pin on the point the fork was taken at are both in the fixture.
		fill(alpha, want, "disk", 2*checkpoint.SectorSize, 0x55)
		if err := alpha.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, vm := range []*volume.VM{beta, alpha} {
			if err := vm.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatalf("the deployment the fixture would hold is inconsistent:\n%v", err)
		}
		for _, id := range []string{"alpha", "beta"} {
			m := want
			if id == "beta" {
				m = forked
			}
			for _, spec := range fixtureSpecs {
				manifest = append(manifest, digestLine(id, spec.Name, m[spec.Name]))
			}
			manifest = append(manifest, digestLine(id, "vmm-state", []byte(fixtureState)))
		}
		objects = listObjects(t, h)
	})
	writeObjects(t, filepath.Join(fixtureDeployment, objectsDir), objects)
	if err := os.WriteFile(filepath.Join(fixtureDeployment, manifestFile),
		[]byte(strings.Join(manifest, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureObject is one object of a dump: the key it lives at and its bytes.
type fixtureObject struct {
	key  string
	data []byte
}

// listObjects reads the whole store out, in ascending key order, which is what
// a dump is.
func listObjects(t *testing.T, h *harness) []fixtureObject {
	t.Helper()
	var found []fixtureObject
	token := ""
	for {
		page, err := h.objects.List(t.Context(), platform.ListRequest{Prefix: h.prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			data, _, err := platform.ReadObject(t.Context(), h.objects, object.Key, 0, 1<<30, platform.ErrNotFound)
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

// writeObjects lays the dump out as one file per object, at the key it lives
// at, so the fixture is reviewable as the namespace it is rather than as one
// opaque blob.
func writeObjects(t *testing.T, dir string, objects []fixtureObject) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		path := filepath.Join(dir, filepath.FromSlash(object.key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, object.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// loadObjects puts a committed dump back into a simulated store, at the keys
// its directory layout names.
func loadObjects(t *testing.T, h *harness, dir string) {
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
		_, err = h.objects.Put(t.Context(), platform.PutRequest{Key: key,
			Body: bytes.NewReader(data), Size: int64(len(data))})
		count++
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("the fixture at %s holds no objects; run go test -run %s -update",
			dir, t.Name())
	}
}

func readManifest(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// A deployment written under a superseded triple is refused with the version it
// was written under named, which is the whole contract while nothing is
// deployed: the store is not migrated, and an operator is told which build
// wrote it.
func TestASupersededDeploymentFixtureIsRefusedByVersion(t *testing.T) {
	for _, superseded := range supersededDeployments {
		synctest.Test(t, func(t *testing.T) {
			h := newSeededHarness(t, 103)
			loadObjects(t, h, filepath.Join(superseded.dir, objectsDir))
			manager := h.manager(t, h.config())
			defer manager.Close(t.Context())
			_, err := manager.Open(t.Context(), "alpha")
			if !errors.Is(err, superseded.sentinel) {
				t.Fatalf("opening %s = %v, want %v", superseded.dir, err, superseded.sentinel)
			}
			if got := err.Error(); !strings.Contains(got, superseded.want) {
				t.Fatalf("the refusal reads %q, want it to name %q", got, superseded.want)
			}
		})
	}
}

// The fixture's own keys must be the ones this build writes: a dump under a
// prefix or a layout that has moved would load into a store nothing could find
// anything in, and the test above would fail for a reason that has nothing to
// do with the formats.
func TestTheCommittedDeploymentFixtureUsesThisBuildsKeys(t *testing.T) {
	root := os.DirFS(filepath.Join(fixtureDeployment, objectsDir))
	var keys []string
	if err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			keys = append(keys, name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(keys)
	for _, key := range keys {
		rest, inside := strings.CutPrefix(key, fixturePrefix)
		if !inside {
			t.Fatalf("%s does not lie under the deployment prefix %q", key, fixturePrefix)
		}
		if strings.HasPrefix(rest, control.RecordPrefix) || strings.HasPrefix(rest, "vm/") {
			continue
		}
		t.Fatalf("%s names neither a control record nor a checkpoint object", key)
	}
	if len(keys) < 2 {
		t.Fatalf("the fixture holds %d objects", len(keys))
	}
}
