package checkpoint

import (
	"errors"
	"testing"
)

// at2MiB and at4KiB are the two geometries a volume may be created with, which
// the tests in this package publish volumes at and divide page numbers by.
var (
	at2MiB = Geometry{PageSize: PageSize2MiB, SegmentPages: segmentPages2MiB}
	at4KiB = Geometry{PageSize: PageSize4KiB, SegmentPages: segmentPages4KiB}
)

// volumesAt is what Root takes: every named volume at its size and one page
// size.
func volumesAt(geometry Geometry, sizes map[string]uint64) map[string]VolumeSpec {
	specs := make(map[string]VolumeSpec, len(sizes))
	for name, size := range sizes {
		specs[name] = VolumeSpec{Size: size, PageSize: geometry.PageSize}
	}
	return specs
}

// The two supported page sizes each carry the segment geometry this build
// writes, and every other page size is refused: a geometry is durable, and a
// page size nothing divides by must never reach the store.
func TestGeometryForAcceptsTheTwoSupportedPageSizes(t *testing.T) {
	for _, want := range []Geometry{at4KiB, at2MiB} {
		got, err := GeometryFor(want.PageSize)
		if err != nil {
			t.Fatalf("GeometryFor(%d) = %v", want.PageSize, err)
		}
		if got != want {
			t.Fatalf("GeometryFor(%d) = %+v, want %+v", want.PageSize, got, want)
		}
	}
	for _, pageSize := range []uint64{0, 1, SectorSize / 2, 8 << 10, 64 << 10, 1 << 20, 4 << 20} {
		if _, err := GeometryFor(pageSize); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("GeometryFor(%d) = %v, want %v", pageSize, err, ErrInvalidConfig)
		}
	}
}

// A geometry read back out of a root is the pair that root recorded, so a page
// size paired with segment pages this build does not write is refused rather
// than divided by.
func TestOnlyTheRecordedPairsAreSupported(t *testing.T) {
	for _, geometry := range []Geometry{at4KiB, at2MiB} {
		if !geometry.supported() {
			t.Fatalf("%+v is not supported", geometry)
		}
	}
	for _, geometry := range []Geometry{
		{PageSize: PageSize4KiB, SegmentPages: segmentPages2MiB},
		{PageSize: PageSize2MiB, SegmentPages: segmentPages4KiB},
		{PageSize: PageSize2MiB, SegmentPages: 0},
		{},
	} {
		if geometry.supported() {
			t.Fatalf("%+v is supported", geometry)
		}
	}
}

// What each geometry covers is what the format states: a 2 MiB-page segment
// covers 512 MiB of volume and a 4 KiB-page one 64 MiB, so a 4 KiB-page volume
// costs sixteen root entries per GiB and the 2 MiB root bound admits about
// 8.5 TiB of it.
func TestASegmentCoversTheVolumeTheFormatStates(t *testing.T) {
	if got := at2MiB.PageSize * at2MiB.SegmentPages; got != 512<<20 {
		t.Fatalf("a 2 MiB-page segment covers %d bytes, want 512 MiB", got)
	}
	if got := at4KiB.PageSize * at4KiB.SegmentPages; got != 64<<20 {
		t.Fatalf("a 4 KiB-page segment covers %d bytes, want 64 MiB", got)
	}
	if got := at4KiB.SegmentCount(1 << 30); got != 16 {
		t.Fatalf("a GiB of a 4 KiB-page volume is %d segments, want 16", got)
	}
	if got := at2MiB.SegmentCount(1 << 30); got != 2 {
		t.Fatalf("a GiB of a 2 MiB-page volume is %d segments, want 2", got)
	}
	// Fifteen bytes an entry is what the root costs, so the 2 MiB bound is
	// about 140,000 segments whatever they cover.
	const rootEntryBytes = 15
	if got := maximumRootSize / rootEntryBytes * (at4KiB.PageSize * at4KiB.SegmentPages) >> 40; got != 8 {
		t.Fatalf("the root bound admits %d TiB of a 4 KiB-page volume, want 8", got)
	}
	if got := maximumRootSize / rootEntryBytes * (at2MiB.PageSize * at2MiB.SegmentPages) >> 40; got != 68 {
		t.Fatalf("the root bound admits %d TiB of a 2 MiB-page volume, want 68", got)
	}
}

// A page number is divided by the volume's own geometry: the segment it falls
// in, its number within that segment, and the byte span it covers, including
// the short last page of a volume that is not a whole number of pages.
func TestAGeometryDividesPageNumbersByItsOwnUnit(t *testing.T) {
	if got := at4KiB.SegmentOf(16383); got != 0 {
		t.Fatalf("page 16383 of a 4 KiB-page volume is in segment %d, want 0", got)
	}
	if got := at4KiB.SegmentOf(16384); got != 1 {
		t.Fatalf("page 16384 of a 4 KiB-page volume is in segment %d, want 1", got)
	}
	if got := at4KiB.OffsetIn(16384); got != 0 {
		t.Fatalf("page 16384 is entry %d of its segment, want 0", got)
	}
	if got := at4KiB.SegmentBase(1); got != 16384 {
		t.Fatalf("segment 1 of a 4 KiB-page volume starts at page %d, want 16384", got)
	}
	if got := at2MiB.SegmentOf(255); got != 0 {
		t.Fatalf("page 255 of a 2 MiB-page volume is in segment %d, want 0", got)
	}
	if got := at2MiB.SegmentOf(256); got != 1 {
		t.Fatalf("page 256 of a 2 MiB-page volume is in segment %d, want 1", got)
	}
	start, span := at4KiB.PageSpan(3*PageSize4KiB+SectorSize/2, 3)
	if start != 3*PageSize4KiB || span != SectorSize/2 {
		t.Fatalf("the last page of a short 4 KiB-page volume is [%d,%d), want [%d,%d)",
			start, start+span, 3*PageSize4KiB, 3*PageSize4KiB+SectorSize/2)
	}
	if _, span := at4KiB.PageSpan(3*PageSize4KiB, 3); span != 0 {
		t.Fatalf("a page past the end spans %d bytes, want none", span)
	}
}
