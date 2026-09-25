package checkpoint

import "fmt"

// The page sizes a volume may be created with. A page is the unit of
// publication — it is written whole or not at all, and it is the unit a reader
// faults in — so which of these a volume uses decides what one store costs it.
// The host chooses when it creates the volume, and the choice is durable.
const (
	PageSize4KiB = 4 << 10
	PageSize2MiB = 2 << 20
)

const (
	// segmentPages2MiB and segmentPages4KiB are how many of a volume's pages one
	// segment of its page table covers, per page size: segment n holds pages
	// [n*SegmentPages, (n+1)*SegmentPages). Each is chosen so that a segment
	// encodes to at most a few hundred kilobytes, well inside
	// maximumSegmentSize, while a root stays about fifteen bytes per segment:
	//
	//   - 256 pages of 2 MiB is 512 MiB of volume per segment, a segment of a few
	//     kilobytes, and two root entries per GiB;
	//   - 16,384 pages of 4 KiB is 64 MiB of volume per segment, a segment of
	//     about 330 KiB — 560 KiB with every entry at its widest — and sixteen
	//     root entries per GiB, about 240 bytes.
	//
	// The maximumRootSize of 2 MiB is about 140,000 entries either way, which is
	// 70 TiB of a 2 MiB-page volume and 8.5 TiB of a 4 KiB-page one.
	//
	// Neither is a constant a reader may divide by. The root records every
	// volume's geometry, and a reader divides page numbers by what it recorded.
	segmentPages2MiB = 256
	segmentPages4KiB = 16 << 10
)

// Geometry is one volume's page geometry: how large its pages are, and how many
// of them one segment of its page table covers. It is chosen when the volume is
// created, recorded in the root beside the volume's size, and immutable for the
// volume's life — a page number means nothing without it, so a checkpoint that
// changed it would rename every page of the volume.
type Geometry struct {
	PageSize     uint64
	SegmentPages uint64
}

// GeometryFor reports the geometry of a volume whose page is pageSize, and
// refuses every other page size: a geometry is durable and every reader divides
// by it, so one this build does not implement must never reach the store.
func GeometryFor(pageSize uint64) (Geometry, error) {
	switch pageSize {
	case PageSize4KiB:
		return Geometry{PageSize: PageSize4KiB, SegmentPages: segmentPages4KiB}, nil
	case PageSize2MiB:
		return Geometry{PageSize: PageSize2MiB, SegmentPages: segmentPages2MiB}, nil
	}
	return Geometry{}, fmt.Errorf("%w: a volume's page is %d or %d bytes, not %d",
		ErrInvalidConfig, PageSize4KiB, PageSize2MiB, pageSize)
}

// supported reports whether a geometry is one this build writes, which is what
// a root read back out of the store must carry: the pair is recorded rather
// than derived, so a root naming a page size with someone else's segment size
// is a root this build cannot divide by.
func (g Geometry) supported() bool {
	found, err := GeometryFor(g.PageSize)
	return err == nil && found == g
}

// PageOf reports the page one byte offset falls in.
func (g Geometry) PageOf(offset uint64) uint64 { return offset / g.PageSize }

// PageCount reports how many pages a volume of this size has. The last of them
// is short where the size is not a whole number of pages.
func (g Geometry) PageCount(size uint64) uint64 { return (size + g.PageSize - 1) / g.PageSize }

// PageSpan reports the byte offset and length of one page within a volume of
// the given size. The length is zero for a page beyond the end.
func (g Geometry) PageSpan(size, page uint64) (uint64, uint64) {
	start := page * g.PageSize
	if start >= size {
		return start, 0
	}
	return start, min(g.PageSize, size-start)
}

// SegmentOf reports the segment of the page table one page number is located by.
func (g Geometry) SegmentOf(page uint64) uint64 { return page / g.SegmentPages }

// OffsetIn reports a page's number relative to the first page of its segment,
// which is how a segment keys its entries.
func (g Geometry) OffsetIn(page uint64) uint32 { return uint32(page % g.SegmentPages) }

// SegmentBase reports the first page one segment covers.
func (g Geometry) SegmentBase(number uint64) uint64 { return number * g.SegmentPages }

// SegmentCount reports how many segments a volume of this size can have, which
// is what bounds a segment number the root names.
func (g Geometry) SegmentCount(size uint64) uint64 {
	return (g.PageCount(size) + g.SegmentPages - 1) / g.SegmentPages
}
