package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// segmentVolumeSize is the volume the segment tests publish: 4 GiB is 2048
// pages, which is eight segments of 256 pages each.
const segmentVolumeSize = 4 << 30

// fillSource fills every page with one repeating byte, so a volume of this size
// costs the encoder almost nothing and what the test measures is how much index
// a checkpoint writes rather than how much data it holds.
type fillSource struct{ value byte }

func (s fillSource) ReadPage(_ context.Context, _ string, _ uint64, dst []byte) error {
	for at := range dst {
		dst[at] = s.value
	}
	return nil
}

// A checkpoint writes the page table of the segments it changed and nothing
// else. The first checkpoint fills every page of a 4 GiB volume, so its index
// object holds eight segments and its parts hold 2048 pages; the second dirties
// one page, so its part holds that page, its index object the one segment
// locating it, and its root — which addresses the other seven in the first
// checkpoint's index object — is a few hundred bytes rather than one entry per
// page of the volume.
func TestACheckpointWritesOnlyTheSegmentsItChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		sizes := map[string]uint64{"disk": segmentVolumeSize}
		pages := uint64(segmentVolumeSize / PageSize)
		root, err := store.Root(t.Context(), control.Ref{VM: "seg", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		firstRef := control.Ref{VM: "seg", Sequence: 2}
		first := store.Begin(root, firstRef)
		for page := range pages {
			first.Dirty("disk", page)
		}
		firstIndex, err := first.Commit(t.Context(), fillSource{value: 0x5a})
		if err != nil {
			t.Fatal(err)
		}
		secondRef := control.Ref{VM: "seg", Sequence: 3}
		second := store.Begin(firstIndex, secondRef)
		second.Dirty("disk", 1000)
		secondIndex, err := second.Commit(t.Context(), fillSource{value: 0xa5})
		if err != nil {
			t.Fatal(err)
		}
		// Seven of the eight segments are still the first checkpoint's, so the
		// root addresses them where they already are rather than writing them
		// again.
		table := secondIndex.volumes["disk"]
		if got, want := len(table.segments), int(pages/segmentPages); got != want {
			t.Fatalf("the root addresses %d segments, want %d", got, want)
		}
		inOwnIndex := 0
		for _, entry := range table.segments {
			if entry.at.ref == secondRef {
				inOwnIndex++
			}
		}
		if inOwnIndex != 1 {
			t.Fatalf("the root addresses %d segments in the second checkpoint's own index object, want 1", inOwnIndex)
		}
		encoded, err := secondIndex.encode()
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) >= 4<<10 {
			t.Fatalf("the root of a checkpoint that dirtied one page of %d is %d bytes, want under 4 KiB",
				pages, len(encoded))
		}
		// The first checkpoint's parts hold every page; the second's hold the
		// one page it dirtied. The segments locating them are the metadata
		// plane's and are never members of a part.
		if got := partMembers(t, store, firstIndex, firstRef); got != int(pages) {
			t.Fatalf("the first checkpoint's parts hold %d members, want %d", got, pages)
		}
		if got := partMembers(t, store, secondIndex, secondRef); got != 1 {
			t.Fatalf("the second checkpoint's parts hold %d members, want the page it dirtied", got)
		}
	})
}

// partMembers counts every member one checkpoint's parts name, across all of
// them.
func partMembers(t *testing.T, store *Store, index *Index, ref control.Ref) int {
	t.Helper()
	count := 0
	parts := index.parts(ref)
	for number := uint32(0); number < parts; number++ {
		table, err := store.readPartTable(t.Context(), ref, number)
		if errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("part %d of %d is not there", number, parts)
		}
		if err != nil {
			t.Fatal(err)
		}
		count += len(table.members)
	}
	return count
}

// A checkpoint measures what its compaction should rewrite from its root alone.
// The root records, per segment, how many bytes that segment's pages read from
// each checkpoint, so a checkpoint's live bytes are a sum over entries the one
// being published already holds: the only segments it opens are the ones it
// changes.
//
// The fixture is a 4 GiB volume with one page written in each of its eight
// segments, then a checkpoint over the first three pages of the first segment
// and one over the first two of them, which leaves the middle one's parts a
// third live and that live page inside a segment the last checkpoint is
// rewriting anyway.
func TestCompactionMeasuresLivenessFromTheRootAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := fixtureStore(t)
		sizes := map[string]uint64{"disk": segmentVolumeSize}
		segments := uint64(segmentVolumeSize / PageSize / segmentPages)
		root, err := store.Root(t.Context(), control.Ref{VM: "live", Sequence: 1}, sizes)
		if err != nil {
			t.Fatal(err)
		}
		spread := store.Begin(root, control.Ref{VM: "live", Sequence: 2})
		for number := range segments {
			spread.Dirty("disk", segmentBase(number))
		}
		first, err := spread.Commit(t.Context(), fillSource{value: 0x11})
		if err != nil {
			t.Fatal(err)
		}
		middleRef := control.Ref{VM: "live", Sequence: 3}
		middle := store.Begin(first, middleRef)
		middle.Dirty("disk", 0)
		middle.Dirty("disk", 1)
		middle.Dirty("disk", 2)
		second, err := middle.Commit(t.Context(), fillSource{value: 0x22})
		if err != nil {
			t.Fatal(err)
		}
		last := store.Begin(second, control.Ref{VM: "live", Sequence: 4})
		last.Dirty("disk", 0)
		last.Dirty("disk", 1)
		third, err := last.Commit(t.Context(), fillSource{value: 0x33})
		if err != nil {
			t.Fatal(err)
		}
		// The middle checkpoint's parts are mostly dead — two of its three pages
		// are replaced here — so the last one compacts it, which is what makes
		// the measurement it took worth checking.
		if entry, named := third.checkpoints[middleRef]; !named || entry.emptied != third.ref.Sequence {
			t.Fatalf("the last checkpoint left %v unemptied (%+v), so it compacted nothing",
				middleRef, entry)
		}
		if slices.Contains(third.Checkpoints(), middleRef) {
			t.Fatalf("the last checkpoint still reads the parts it emptied: %v", third.Checkpoints())
		}
		// One segment changed, so one segment was opened.
		if got := len(third.loaded); got != 1 {
			t.Fatalf("a checkpoint that changed one of %d segments opened %d of them",
				segments, got)
		}
		// The page compaction rescued is still the one the middle checkpoint
		// published, read through the part the last one moved it into.
		page := make([]byte, PageSize)
		if err := store.Read(t.Context(), third, "disk", 2*PageSize, page); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(page, bytes.Repeat([]byte{0x22}, PageSize)) {
			t.Fatalf("the rescued page reads back as %#x...", page[:8])
		}
	})
}
