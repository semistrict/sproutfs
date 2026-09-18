package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// checkpointObjects lists the keys one checkpoint holds, relative to its own
// prefix, so a test can say what a published checkpoint is made of.
func checkpointObjects(t *testing.T, store *Store, ref control.Ref) []string {
	t.Helper()
	prefix, err := store.ObjectPrefix(ref)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	token := ""
	for {
		page, err := store.objects.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			keys = append(keys, strings.TrimPrefix(object.Key.String(), prefix.String()))
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	return keys
}

// objectRequests reports the object-store operations the trace recorded, which
// is what says how many requests a read costs.
func objectRequests(runtime *sim.Runtime) []string {
	var operations []string
	for _, event := range runtime.Trace().Events() {
		if event.Kind == "object_store" {
			operations = append(operations, event.Operation)
		}
	}
	return operations
}

func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for at := range got {
		if got[at] != want[at] {
			return false
		}
	}
	return true
}

// A page a checkpoint zeroed is simply absent from the segment that checkpoint
// rewrote. Nothing in the parts says it left: the segment is the whole answer,
// so the parts hold no member naming the page and the index object holds the
// rewritten segment.
func TestAZeroedPageLeavesItsSegmentAndNothingElse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		// Three pages, of which one is zeroed: what is left of the first
		// checkpoint's parts is two thirds live, so nothing here is compacted
		// and the erasing checkpoint's parts hold only what it wrote itself.
		root, err := store.Root(t.Context(), control.Ref{VM: "erase", Sequence: 1},
			map[string]uint64{"disk": 3 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		filling := store.Begin(root, control.Ref{VM: "erase", Sequence: 2})
		for page := range uint64(3) {
			filling.Dirty("disk", page)
		}
		filled, err := filling.Commit(t.Context(), fillSource{value: 6})
		if err != nil {
			t.Fatal(err)
		}
		erasedRef := control.Ref{VM: "erase", Sequence: 3}
		erasing := store.Begin(filled, erasedRef)
		erasing.Dirty("disk", 1)
		erased, err := erasing.Commit(t.Context(), fillSource{value: 0})
		if err != nil {
			t.Fatal(err)
		}
		held, err := erased.segmentAt(t.Context(), "disk", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := held.pages[1]; found {
			t.Fatal("the segment the erasing checkpoint wrote still locates the page it zeroed")
		}
		if got := memberCount(t, store, erased, erasedRef); got != 0 {
			t.Fatalf("the erasing checkpoint's parts hold %d members, want the page absent from its segment and nothing more", got)
		}
		reopened, err := store.Open(t.Context(), erasedRef)
		if err != nil {
			t.Fatal(err)
		}
		page := make([]byte, PageSize)
		if err := store.Read(t.Context(), reopened, "disk", PageSize, page); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(page, make([]byte, PageSize)) {
			t.Fatalf("the zeroed page reads back as %#x...", page[:8])
		}
	})
}

// A deployment written when the root was the whole of an index object is
// refused by the version it was written under. Nothing is deployed, so the
// contract is that an operator is told which build wrote the store rather than
// being served a checkpoint this build cannot read.
func TestADeploymentOfSupersededIndexObjectsIsRefusedByVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		loadFixtureObjects(t, store, filepath.Join("testdata/index-6-part-2", "objects"))
		_, err := store.Open(t.Context(), fixtureSecond)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("opening a deployment of superseded index objects reported %v", err)
		}
		want := fmt.Sprintf("checkpoint index format version %d", 6)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal reads %q, want it to name %q", err, want)
		}
	})
}
