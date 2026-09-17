package checkpoint

import (
	"context"
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

// objectPresent reports whether one key holds an object, which is what says an
// index object is still there or has been swept.
func objectPresent(t *testing.T, store *Store, key platform.ObjectKey) bool {
	t.Helper()
	_, err := store.objects.Head(t.Context(), key)
	if err == nil {
		return true
	}
	if !errors.Is(err, platform.ErrNotFound) {
		t.Fatal(err)
	}
	return false
}

// indexObjectKey names the metadata plane of one checkpoint.
func indexObjectKey(t *testing.T, store *Store, ref control.Ref) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey(store.checkpointPrefix(ref) + "index")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// memberCount is how many members one checkpoint's parts hold between them,
// which is what says whether anything but pages and the VMM state is in them.
func memberCount(t *testing.T, store *Store, index *Index, ref control.Ref) int {
	t.Helper()
	count := 0
	for number := uint32(0); number < index.parts(ref); number++ {
		table, err := store.readPartTable(t.Context(), ref, number)
		if err != nil {
			t.Fatal(err)
		}
		count += len(table.members)
	}
	return count
}

// A checkpoint is two planes: one index object holding the segments it changed
// and its root, and the parts holding the guest bytes. Nothing else is written
// under its prefix, and opening it is one GET of the index object.
func TestAPublishedCheckpointIsAnIndexObjectAndItsParts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, runtime := tailStore(t)
		ref := control.Ref{VM: "planes", Sequence: 2}
		root, err := store.Root(t.Context(), control.Ref{VM: "planes", Sequence: 1},
			map[string]uint64{"disk": 2 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, ref)
		publication.Dirty("disk", 0)
		publication.Dirty("disk", 1)
		if _, err := publication.Commit(t.Context(), fillSource{value: 9}); err != nil {
			t.Fatal(err)
		}
		if keys := checkpointObjects(t, store, ref); !slicesEqual(keys, []string{"index", "part/0"}) {
			t.Fatalf("a published checkpoint holds %v, want its index object and its parts", keys)
		}
		runtime.Trace().Reset()
		if _, err := store.Open(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if operations := objectRequests(runtime); len(operations) != 1 || operations[0] != string(sim.ObjectGet) {
			t.Fatalf("opening a checkpoint issued %v, want one get", operations)
		}
	})
}

// A checkpoint writes into its index object only the segments it changed, and
// its root addresses every other segment inside the index object of the
// checkpoint that wrote it. Its parts hold the guest bytes alone.
func TestACheckpointWritesOnlyTheSegmentsItChangedIntoItsIndexObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		pages := uint64(segmentVolumeSize / PageSize)
		root, err := store.Root(t.Context(), control.Ref{VM: "planes", Sequence: 1},
			map[string]uint64{"disk": segmentVolumeSize})
		if err != nil {
			t.Fatal(err)
		}
		firstRef := control.Ref{VM: "planes", Sequence: 2}
		first := store.Begin(root, firstRef)
		for page := range pages {
			first.Dirty("disk", page)
		}
		firstIndex, err := first.Commit(t.Context(), fillSource{value: 0x5a})
		if err != nil {
			t.Fatal(err)
		}
		secondRef := control.Ref{VM: "planes", Sequence: 3}
		second := store.Begin(firstIndex, secondRef)
		second.Dirty("disk", 1000)
		secondIndex, err := second.Commit(t.Context(), fillSource{value: 0xa5})
		if err != nil {
			t.Fatal(err)
		}
		if keys := checkpointObjects(t, store, secondRef); !slicesEqual(keys, []string{"index", "part/0"}) {
			t.Fatalf("the second checkpoint holds %v, want its index object and one part", keys)
		}
		table := secondIndex.volumes["disk"]
		if got, want := len(table.segments), int(pages/segmentPages); got != want {
			t.Fatalf("the root addresses %d segments, want %d", got, want)
		}
		own, inherited := 0, 0
		for _, entry := range table.segments {
			switch entry.at.ref {
			case secondRef:
				own++
			case firstRef:
				inherited++
			default:
				t.Fatalf("a segment is addressed in %v, which is neither checkpoint", entry.at.ref)
			}
		}
		if own != 1 || inherited != int(pages/segmentPages)-1 {
			t.Fatalf("the root addresses %d segments of its own and %d of its parent's, want 1 and %d",
				own, inherited, int(pages/segmentPages)-1)
		}
		// The data plane holds the dirty page and nothing else: no segment and
		// no root is a member of a part any more.
		if got := memberCount(t, store, secondIndex, secondRef); got != 1 {
			t.Fatalf("the second checkpoint's parts hold %d members, want the dirty page alone", got)
		}
	})
}

// An index object lives for as long as any root addresses a segment in it. A
// sweep leaves one whose segments the current root still addresses, and takes
// it once a later checkpoint has rewritten the last of them.
func TestAnIndexObjectStaysUntilNoRootAddressesASegmentInIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		root, err := store.Root(t.Context(), control.Ref{VM: "held", Sequence: 1},
			map[string]uint64{"disk": segmentVolumeSize})
		if err != nil {
			t.Fatal(err)
		}
		publish := func(parent *Index, sequence uint64, value byte, pages ...uint64) *Index {
			t.Helper()
			p := store.Begin(parent, control.Ref{VM: "held", Sequence: sequence})
			for _, page := range pages {
				p.Dirty("disk", page)
			}
			index, err := p.Commit(t.Context(), fillSource{value: value})
			if err != nil {
				t.Fatal(err)
			}
			return index
		}
		// One page in each of two segments, then one checkpoint per segment
		// rewriting the page in it.
		first := publish(root, 2, 0x11, segmentBase(0), segmentBase(1))
		second := publish(first, 3, 0x22, segmentBase(0))
		firstKey := indexObjectKey(t, store, first.ref)
		if entry := second.volumes["disk"].segments[1]; entry.at.ref != first.ref {
			t.Fatalf("the second root addresses segment 1 in %v, want the first checkpoint", entry.at.ref)
		}
		if err := store.Reclaim(t.Context(), first, second, nil); err != nil {
			t.Fatal(err)
		}
		if !objectPresent(t, store, firstKey) {
			t.Fatal("a sweep deleted an index object the current root still addresses a segment in")
		}
		third := publish(second, 4, 0x33, segmentBase(1))
		if entry := third.volumes["disk"].segments[1]; entry.at.ref != third.ref {
			t.Fatalf("the third root addresses segment 1 in %v, want its own index object", entry.at.ref)
		}
		if err := store.Reclaim(t.Context(), second, third, nil); err != nil {
			t.Fatal(err)
		}
		if objectPresent(t, store, firstKey) {
			t.Fatal("a sweep kept an index object no root addresses a segment in")
		}
	})
}

// refusingIndexObject refuses the create-if-absent write of a checkpoint's
// index object, which is the publication's commit: every part is written and
// nothing of the checkpoint is readable.
type refusingIndexObject struct {
	platform.ObjectStore
	refuse bool
}

func (s *refusingIndexObject) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if s.refuse && strings.HasSuffix(request.Key.String(), "/index") {
		return platform.PutResult{}, errors.New("object storage refused the index object")
	}
	return s.ObjectStore.Put(ctx, request)
}

// The index object's PUT is the commit: a publication interrupted before it
// leaves parts nothing names, and the checkpoint is absent rather than half
// there. The same publication repeated under the same reference lands.
func TestAPublicationInterruptedBeforeTheIndexObjectLeavesNoCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := &refusingIndexObject{ObjectStore: sim.New(sim.Config{Seed: 106}).ObjectStore()}
		prefix, err := platform.NewObjectPrefix(fixturePrefix)
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(Config{ObjectStore: objects, ObjectPrefix: prefix, PartBytes: 1})
		if err != nil {
			t.Fatal(err)
		}
		ref := control.Ref{VM: "torn", Sequence: 2}
		root, err := store.Root(t.Context(), control.Ref{VM: "torn", Sequence: 1},
			map[string]uint64{"disk": 3 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, ref)
		for page := range uint64(3) {
			publication.Dirty("disk", page)
		}
		objects.refuse = true
		if _, err := publication.Commit(t.Context(), fillSource{value: 4}); err == nil {
			t.Fatal("a publication whose index object was refused reported success")
		}
		if _, err := store.Open(t.Context(), ref); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("opening a checkpoint whose index object never landed: %v", err)
		}
		objects.refuse = false
		index, err := publication.Commit(t.Context(), fillSource{value: 4})
		if err != nil {
			t.Fatalf("repeating the publication under the same reference: %v", err)
		}
		if _, err := store.Open(t.Context(), index.Ref()); err != nil {
			t.Fatal(err)
		}
	})
}

// Compaction works over the data plane alone: it rewrites live page members out
// of checkpoints whose parts have become mostly dead, and moves no segment. A
// segment whose entries it did not change stays where it was written, in the
// index object of the checkpoint being emptied, which therefore stays too.
//
// The fixture writes eight pages in segment 0 and four in segment 1, then a
// checkpoint that erases one page of segment 0 and rewrites the four of segment
// 1, and then one that rewrites three of those four. The middle checkpoint's
// parts are a quarter live, so its last page is rescued; its index object holds
// segment 0, which nothing since has touched. The first checkpoint stays more
// than half live throughout, so nothing rewrites it.
func TestCompactionMovesNoSegment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		root, err := store.Root(t.Context(), control.Ref{VM: "compacting", Sequence: 1},
			map[string]uint64{"disk": segmentVolumeSize})
		if err != nil {
			t.Fatal(err)
		}
		publish := func(parent *Index, sequence uint64, value byte, pages ...uint64) *Index {
			t.Helper()
			p := store.Begin(parent, control.Ref{VM: "compacting", Sequence: sequence})
			for _, page := range pages {
				p.Dirty("disk", page)
			}
			index, err := p.Commit(t.Context(), fillSource{value: value})
			if err != nil {
				t.Fatal(err)
			}
			return index
		}
		var cold, wide []uint64
		for page := range uint64(8) {
			cold = append(cold, segmentBase(0)+page)
		}
		for page := range uint64(4) {
			wide = append(wide, segmentBase(1)+page)
		}
		first := publish(root, 2, 0x11, append(append([]uint64{}, cold...), wide...)...)
		// The middle checkpoint erases one page of segment 0, so its index object
		// holds that segment while its parts hold only pages of segment 1.
		middle := store.Begin(first, control.Ref{VM: "compacting", Sequence: 3})
		middle.Dirty("disk", segmentBase(0))
		for _, page := range wide {
			middle.Dirty("disk", page)
		}
		second, err := middle.Commit(t.Context(), erasingSource{erased: segmentBase(0), value: 0x22})
		if err != nil {
			t.Fatal(err)
		}
		if second.volumes["disk"].segments[0].at.ref != second.ref {
			t.Fatal("the fixture expected the middle checkpoint to write segment 0")
		}
		lastRef := control.Ref{VM: "compacting", Sequence: 4}
		last := publish(second, 4, 0x33, wide[0], wide[1], wide[2])
		if entry := last.checkpoints[second.ref]; entry.emptied == 0 {
			t.Fatalf("the fixture expected the middle checkpoint to be compacted empty: %+v", entry)
		}
		// Segment 0 is untouched here, so it stays in the index object the
		// middle checkpoint wrote it into rather than being copied forward.
		if at := last.volumes["disk"].segments[0].at.ref; at != second.ref {
			t.Fatalf("compaction moved segment 0 to %v, want it left in %v", at, second.ref)
		}
		if !objectPresent(t, store, indexObjectKey(t, store, second.ref)) {
			t.Fatal("the index object holding an addressed segment is gone")
		}
		// Three pages written and one rescued: a segment is never a member of a
		// part, so nothing else is in the data plane.
		if got := memberCount(t, store, last, lastRef); got != 4 {
			t.Fatalf("the compacting checkpoint's parts hold %d members, want three written pages and one rescued", got)
		}
	})
}

// erasingSource fills every page with one byte except the one it erases, which
// reads as zeroes and so leaves the segment that named it.
type erasingSource struct {
	erased uint64
	value  byte
}

func (s erasingSource) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	fill := s.value
	if page == s.erased {
		fill = 0
	}
	for at := range dst {
		dst[at] = fill
	}
	return nil
}

// A deployment written when the root was the last member of a checkpoint's last
// part is refused by the layout version that part carries, rather than reported
// as a checkpoint that was never published.
func TestARootInAPartDeploymentIsRefusedByVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		loadFixtureObjects(t, store, filepath.Join("testdata/part-3", "objects"))
		_, err := store.Open(t.Context(), fixtureSecond)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("opening a deployment whose roots are part members reported %v", err)
		}
		want := fmt.Sprintf("checkpoint part format version %d", 3)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal reads %q, want it to name %q", err, want)
		}
	})
}
