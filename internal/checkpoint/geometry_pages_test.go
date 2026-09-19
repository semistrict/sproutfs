package checkpoint

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
)

// The volumes of the mixed-geometry fixture. The memory is 4 KiB pages and
// spans its first segment boundary — 16,384 pages, 64 MiB — and the disk is
// 2 MiB pages and spans its own, at 256 pages, with a last page of three
// sectors so that a short page is published at the large geometry too. A 4 KiB
// page is one sector, so a 4 KiB-page volume is always a whole number of pages
// and cannot have a short one.
const (
	smallVolume = 64<<20 + 2*PageSize4KiB
	largeVolume = 512<<20 + 3*SectorSize
)

// pagePattern fills a page with bytes no other page of any volume repeats, so
// what a read must return is stated rather than remembered.
type pagePattern struct{ tag byte }

func (p pagePattern) fill(volume string, page uint64, dst []byte) {
	for at := range dst {
		dst[at] = p.tag ^ byte(len(volume)*37) ^ byte(page) ^ byte(page>>8) ^ byte(at)
	}
}

func (p pagePattern) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	p.fill(volume, page, dst)
	return nil
}

// wantPage is what one page of a volume holds under a pattern.
func (p pagePattern) wantPage(geometry Geometry, size uint64, volume string, page uint64) []byte {
	_, span := geometry.PageSpan(size, page)
	data := make([]byte, span)
	p.fill(volume, page, data)
	return data
}

// One VM's volumes may have different page sizes, and one checkpoint publishes
// both: each volume's pages are named, read and located in its own unit. The
// checkpoint is reopened from the store through a fresh reader, so what is
// asserted is what the objects say rather than what the publication remembered.
func TestOneCheckpointPublishesVolumesOfBothGeometries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		root, err := store.Root(t.Context(), control.Ref{VM: "mixed", Sequence: 1},
			map[string]VolumeSpec{
				"ram":  {Size: smallVolume, PageSize: PageSize4KiB},
				"disk": {Size: largeVolume, PageSize: PageSize2MiB},
			})
		if err != nil {
			t.Fatal(err)
		}
		// The pages on either side of each volume's first segment boundary, the
		// first page of each, and the short last page of the large one.
		small := []uint64{0, 1, 16383, 16384, 16385}
		large := []uint64{0, 255, 256}
		ref := control.Ref{VM: "mixed", Sequence: 2}
		publication := store.Begin(root, ref)
		for _, page := range small {
			publication.Dirty("ram", page)
		}
		for _, page := range large {
			publication.Dirty("disk", page)
		}
		pattern := pagePattern{tag: 0x9e}
		if _, err := publication.Commit(t.Context(), pattern); err != nil {
			t.Fatal(err)
		}
		index, err := store.Open(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if got := index.Geometry("ram"); got != at4KiB {
			t.Fatalf("the reopened checkpoint gives ram %+v, want %+v", got, at4KiB)
		}
		if got := index.Geometry("disk"); got != at2MiB {
			t.Fatalf("the reopened checkpoint gives disk %+v, want %+v", got, at2MiB)
		}
		// Every published page reads back byte for byte at its own page
		// boundary, including the short last page of the large volume.
		for _, item := range []struct {
			volume   string
			geometry Geometry
			size     uint64
			pages    []uint64
		}{
			{volume: "ram", geometry: at4KiB, size: smallVolume, pages: small},
			{volume: "disk", geometry: at2MiB, size: largeVolume, pages: large},
		} {
			for _, page := range item.pages {
				start, span := item.geometry.PageSpan(item.size, page)
				got := make([]byte, span)
				if err := store.Read(t.Context(), index, item.volume, start, got); err != nil {
					t.Fatalf("reading page %d of %s: %v", page, item.volume, err)
				}
				want := pattern.wantPage(item.geometry, item.size, item.volume, page)
				if !bytes.Equal(got, want) {
					t.Fatalf("page %d of %s reads back as %#x..., want %#x...",
						page, item.volume, got[:8], want[:8])
				}
			}
		}
		if got, want := uint64(largeVolume-256*PageSize2MiB), uint64(3*SectorSize); got != want {
			t.Fatalf("the large volume's last page is %d bytes, want %d", got, want)
		}
		// Mid-page, and across each volume's first segment boundary: a read that
		// spans two pages is served from both members.
		checkSpan(t, store, index, pattern, "ram", at4KiB, smallVolume, 16384*PageSize4KiB+1000, 2000)
		checkSpan(t, store, index, pattern, "ram", at4KiB, smallVolume, 16384*PageSize4KiB-100, 200)
		checkSpan(t, store, index, pattern, "disk", at2MiB, largeVolume, 255*PageSize2MiB+12345, 7777)
		checkSpan(t, store, index, pattern, "disk", at2MiB, largeVolume, 256*PageSize2MiB-100, 200)
		// Locate answers in each volume's own unit: one extent per page of that
		// volume, numbered in that volume's pages.
		checkExtents(t, index, "ram", at4KiB, 16383*PageSize4KiB, 3*PageSize4KiB,
			[]uint64{16383, 16384, 16385})
		checkExtents(t, index, "disk", at2MiB, 255*PageSize2MiB, PageSize2MiB+3*SectorSize,
			[]uint64{255, 256})
	})
}

// checkSpan reads a range that may cross a page or a segment boundary and
// compares it with the pages the pattern published.
func checkSpan(t *testing.T, store *Store, index *Index, pattern pagePattern,
	volume string, geometry Geometry, size, offset, length uint64) {
	t.Helper()
	got := make([]byte, length)
	if err := store.Read(t.Context(), index, volume, offset, got); err != nil {
		t.Fatalf("reading [%d,%d) of %s: %v", offset, offset+length, volume, err)
	}
	want := make([]byte, length)
	for cursor := offset; cursor < offset+length; {
		page := geometry.PageOf(cursor)
		start, span := geometry.PageSpan(size, page)
		stop := min(offset+length, start+span)
		whole := pattern.wantPage(geometry, size, volume, page)
		copy(want[cursor-offset:stop-offset], whole[cursor-start:])
		cursor = stop
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("[%d,%d) of %s reads back as %#x..., want %#x...",
			offset, offset+length, volume, got[:8], want[:8])
	}
}

// checkExtents requires that Locate report one extent per page of the volume's
// own geometry, in that volume's page numbers.
func checkExtents(t *testing.T, index *Index, volume string, geometry Geometry,
	offset, length uint64, pages []uint64) {
	t.Helper()
	extents, err := index.Locate(t.Context(), volume, offset, length)
	if err != nil {
		t.Fatal(err)
	}
	if len(extents) != len(pages) {
		t.Fatalf("locating [%d,%d) of %s reports %d extents, want %d",
			offset, offset+length, volume, len(extents), len(pages))
	}
	cursor := offset
	for at, page := range pages {
		start, span := geometry.PageSpan(index.Size(volume), page)
		want := control.Extent{Offset: cursor, Length: min(offset+length, start+span) - cursor,
			Identity: control.Identity{Ref: index.Ref(), Volume: volume, Page: page}}
		if extents[at] != want {
			t.Fatalf("extent %d of %s is %+v, want %+v", at, volume, extents[at], want)
		}
		cursor += want.Length
	}
}

// A store into one 4 KiB page renames that page and nothing else: the 511 pages
// that share its 2 MiB keep the identity the checkpoint before gave them, and
// the second checkpoint writes only the one segment whose table changed.
func TestARewrittenSmallPageLeavesItsNeighboursAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		root, err := store.Root(t.Context(), control.Ref{VM: "small", Sequence: 1},
			volumesAt(at4KiB, map[string]uint64{"ram": smallVolume}))
		if err != nil {
			t.Fatal(err)
		}
		// One 2 MiB run of 4 KiB pages, and one page in the volume's second
		// segment so that the second checkpoint has a segment to leave alone.
		const run = PageSize2MiB / PageSize4KiB
		const outside = 16384
		firstRef := control.Ref{VM: "small", Sequence: 2}
		first := store.Begin(root, firstRef)
		for page := range uint64(run) {
			first.Dirty("ram", page)
		}
		first.Dirty("ram", outside)
		firstIndex, err := first.Commit(t.Context(), pagePattern{tag: 0x11})
		if err != nil {
			t.Fatal(err)
		}
		secondRef := control.Ref{VM: "small", Sequence: 3}
		second := store.Begin(firstIndex, secondRef)
		second.Dirty("ram", 7)
		secondIndex, err := second.Commit(t.Context(), pagePattern{tag: 0x22})
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(t.Context(), secondRef)
		if err != nil {
			t.Fatal(err)
		}
		extents, err := reopened.Locate(t.Context(), "ram", 0, PageSize2MiB)
		if err != nil {
			t.Fatal(err)
		}
		if len(extents) != run {
			t.Fatalf("a 2 MiB run of 4 KiB pages locates as %d extents, want %d", len(extents), run)
		}
		for page, extent := range extents {
			want := firstRef
			if page == 7 {
				want = secondRef
			}
			if extent.Identity.Ref != want || extent.Identity.Page != uint64(page) ||
				extent.Length != PageSize4KiB {
				t.Fatalf("page %d of the run is %+v, want page %d of %v",
					page, extent.Identity, page, want)
			}
		}
		// One page changed, so one segment's table changed: the other segment
		// stays addressed in the index object of the checkpoint that wrote it.
		table := secondIndex.volumes["ram"]
		if at := table.segments[0].at.ref; at != secondRef {
			t.Fatalf("the segment holding the rewritten page is addressed in %v, want %v", at, secondRef)
		}
		if at := table.segments[1].at.ref; at != firstRef {
			t.Fatalf("the segment the checkpoint did not change is addressed in %v, want %v", at, firstRef)
		}
		if got := len(secondIndex.dirtySegments()); got != 1 {
			t.Fatalf("the second checkpoint wrote %d segments, want the one it changed", got)
		}
		// The bytes are the new page's, and its neighbours' are the old ones'.
		checkSpan(t, store, reopened, pagePattern{tag: 0x22}, "ram", at4KiB, smallVolume,
			7*PageSize4KiB, PageSize4KiB)
		checkSpan(t, store, reopened, pagePattern{tag: 0x11}, "ram", at4KiB, smallVolume,
			8*PageSize4KiB, PageSize4KiB)
	})
}

// Compaction of a 4 KiB-page volume moves the bytes and nothing else: a rescued
// page keeps the identity of the checkpoint that published it and the volume
// keeps its geometry, and the byte sums the root records for each checkpoint
// are the member bytes its entries name — bytes, never pages.
func TestCompactionOfASmallPageVolumeKeepsIdentitiesAndGeometry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := tailStore(t)
		root, err := store.Root(t.Context(), control.Ref{VM: "compact", Sequence: 1},
			volumesAt(at4KiB, map[string]uint64{"ram": smallVolume}))
		if err != nil {
			t.Fatal(err)
		}
		publish := func(parent *Index, sequence uint64, tag byte, pages ...uint64) *Index {
			t.Helper()
			p := store.Begin(parent, control.Ref{VM: "compact", Sequence: sequence})
			for _, page := range pages {
				p.Dirty("ram", page)
			}
			index, err := p.Commit(t.Context(), pagePattern{tag: tag})
			if err != nil {
				t.Fatal(err)
			}
			return index
		}
		firstRef := control.Ref{VM: "compact", Sequence: 2}
		secondRef := control.Ref{VM: "compact", Sequence: 3}
		var written []uint64
		for page := range uint64(12) {
			written = append(written, page)
		}
		first := publish(root, 2, 0x11, written...)
		// Ten of the twelve are rewritten, which leaves the first checkpoint's
		// parts a sixth live, so the publication that rewrote them rescues the
		// other two into its own parts and empties the first.
		second := publish(first, 3, 0x22, written[:10]...)
		if entry := second.checkpoints[firstRef]; entry.emptied != secondRef.Sequence {
			t.Fatalf("the first checkpoint was left unemptied (%+v), so nothing was compacted", entry)
		}
		reopened, err := store.Open(t.Context(), secondRef)
		if err != nil {
			t.Fatal(err)
		}
		if got := reopened.Geometry("ram"); got != at4KiB {
			t.Fatalf("the compacting checkpoint gives ram %+v, want %+v", got, at4KiB)
		}
		// The rescued pages are in the third checkpoint's parts under the
		// identity the first gave them, and read back as the first's bytes.
		held, err := reopened.segmentAt(t.Context(), "ram", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range []uint64{10, 11} {
			at := held.pages[at4KiB.OffsetIn(page)]
			if at.ref != secondRef || at.origin != firstRef {
				t.Fatalf("rescued page %d sits in %v under the identity %v, want %v and %v",
					page, at.ref, at.origin, secondRef, firstRef)
			}
			checkSpan(t, store, reopened, pagePattern{tag: 0x11}, "ram", at4KiB, smallVolume,
				page*PageSize4KiB, PageSize4KiB)
		}
		extents, err := reopened.Locate(t.Context(), "ram", 10*PageSize4KiB, 2*PageSize4KiB)
		if err != nil {
			t.Fatal(err)
		}
		for at, extent := range extents {
			if extent.Identity.Ref != firstRef {
				t.Fatalf("rescued page %d reports the identity %v, want %v",
					10+at, extent.Identity.Ref, firstRef)
			}
		}
		checkLiveBytes(t, reopened)
	})
}

// checkLiveBytes requires that what the root records each segment's pages read
// from each checkpoint is the sum of the member lengths those entries name.
// The sums are what compaction measures liveness with, so they are bytes of
// encoded members and never a count of pages times a page size.
func checkLiveBytes(t *testing.T, index *Index) {
	t.Helper()
	for name, table := range index.volumes {
		for number, entry := range table.segments {
			held, err := index.segmentAt(t.Context(), name, number)
			if err != nil {
				t.Fatal(err)
			}
			want := make(map[control.Ref]uint64, len(held.pages))
			for _, at := range held.pages {
				want[at.ref] += at.length
			}
			if len(entry.reads) != len(want) {
				t.Fatalf("segment %d of %s records %d checkpoints, want %d",
					number, name, len(entry.reads), len(want))
			}
			for _, use := range entry.reads {
				if use.bytes != want[use.ref] {
					t.Fatalf("segment %d of %s records %d bytes read from %v, want %d",
						number, name, use.bytes, use.ref, want[use.ref])
				}
				if use.bytes == 0 || use.bytes > uint64(len(held.pages))*table.geometry.PageSize+uint64(len(held.pages))*64 {
					t.Fatalf("segment %d of %s records %d bytes from %v, which is not member bytes",
						number, name, use.bytes, use.ref)
				}
			}
		}
	}
}

// dirtySegments reports the segments this index's own checkpoint wrote into its
// index object, which is what a checkpoint costs in table however much of the
// volume it names.
func (i *Index) dirtySegments() []segmentKey {
	var own []segmentKey
	for name, table := range i.volumes {
		for number, entry := range table.segments {
			if entry.at.ref == i.ref {
				own = append(own, segmentKey{volume: name, number: number})
			}
		}
	}
	return own
}
