package checkpoint

import (
	"context"
	"slices"
	"unicode/utf8"
	"unsafe"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"google.golang.org/protobuf/encoding/protowire"
)

// A page table is one published segment of a volume's page table, decoded: what
// a reader looks a page up in. A segment is immutable under its identity — the
// checkpoint that wrote it, its volume and its number — so it is decoded once
// and shared by every index that addresses it, through the page cache's memory
// tier (Cache), which charges it to the host's budget and evicts it, least
// recently used, beside the pages. An index takes it from there on every
// lookup and memoises nothing.
//
// It is dense: one entry for every page up to the last one the segment
// locates, so a lookup is an index and no map. At 4 KiB per page a whole
// segment is 16,384 entries of twenty bytes, 320 KiB, which is about what its
// encoding is. On GCE on 2026-10-04 a fault's planning decoded the segment
// into a map of locations through 16,384 protobuf messages, about 6 ms of
// processor and 7.6 MB of allocation for each segment each index touched
// (docs/measurements/gce-fault-first-2026-10-04.md). It is parsed straight off
// the wire instead.
type pageTable struct {
	// refs are the checkpoints its pages read from and origins the checkpoints
	// pages compaction moved were first published under, as the segment lists
	// them.
	refs, origins []control.Ref
	// entries is indexed by a page's number relative to the segment's first
	// page.
	entries []tableEntry
	// parts holds, for each of refs, one more than the highest part a page
	// names in it, and zero for one no page names. It is what an index checks
	// the table against: every checkpoint a page reads from must be one the
	// root names, with that many parts.
	parts []uint32
	// pages is how many entries locate a page.
	pages int
}

// tableEntry is one page's location. A member lies within one part, which
// maximumPartSize bounds well inside 32 bits, so its offset and length are
// stored in 32. ref is one more than the position of the page's checkpoint in
// refs, zero for a page the table does not locate; origin is one more than
// the position of its origin in origins, zero where the origin is the
// checkpoint.
type tableEntry struct {
	offset, length uint32
	part           uint32
	ref, origin    uint32
}

// emptyTable is the table of a segment the root does not address: every page
// of it reads as zeroes, and it costs no I/O.
var emptyTable = &pageTable{}

// at reports where one page lives, by its number relative to the segment's
// first page.
func (t *pageTable) at(relative uint64) (location, bool) {
	if relative >= uint64(len(t.entries)) {
		return location{}, false
	}
	entry := t.entries[relative]
	if entry.ref == 0 {
		return location{}, false
	}
	ref := t.refs[entry.ref-1]
	origin := ref
	if entry.origin != 0 {
		origin = t.origins[entry.origin-1]
	}
	return location{ref: ref, origin: origin, part: entry.part, offset: uint64(entry.offset),
		length: uint64(entry.length)}, true
}

// locate appends to extents where the bytes span covers of the pages
// [first, stop) live, pages of the segment that begins at page base: one extent
// for each page the table locates, under its identity (identityOf), and one for
// each run of pages it does not, which read as zeroes and merge with a run
// of them extents already ends with. It reads the pages' entries off the table
// in order, which is what makes locating a fault's window one lookup of its
// table and a scan of it, not one lookup a page.
func (t *pageTable) locate(extents []control.Extent, volume string, base, first, stop uint64,
	span byteSpan) []control.Extent {
	from, to := first-base, stop-base
	located := t.entries[min(from, uint64(len(t.entries))):min(to, uint64(len(t.entries)))]
	extents = slices.Grow(extents, t.extentsIn(located, to-from))
	hole := func(first, stop uint64) {
		offset, _ := span.of(first)
		last, length := span.of(stop - 1)
		length += last - offset
		if n := len(extents); n > 0 && extents[n-1].Identity.Zero && extents[n-1].Offset+extents[n-1].Length == offset {
			extents[n-1].Length += length
			return
		}
		extents = append(extents, control.Extent{Offset: offset, Length: length, Identity: control.ZeroIdentity})
	}
	for at := 0; at < len(located); at++ {
		entry := located[at]
		page := first + uint64(at)
		if entry.ref == 0 {
			run := at + 1
			for run < len(located) && located[run].ref == 0 {
				run++
			}
			hole(page, first+uint64(run))
			at = run - 1
			continue
		}
		origin := t.refs[entry.ref-1]
		if entry.origin != 0 {
			origin = t.origins[entry.origin-1]
		}
		offset, length := span.of(page)
		extents = append(extents, control.Extent{Offset: offset, Length: length,
			Identity: control.Identity{Ref: origin, Volume: volume, Page: page}})
	}
	if beyond := first + uint64(len(located)); beyond < stop {
		// The table locates no page past its last entry.
		hole(beyond, stop)
	}
	return extents
}

// extentsIn is how many extents at most the pages entries and the pages after
// them, pages in all, locate to: one a located page and one a run of others.
func (t *pageTable) extentsIn(entries []tableEntry, pages uint64) int {
	count := 0
	for at, entry := range entries {
		if entry.ref != 0 || at == 0 || entries[at-1].ref != 0 {
			count++
		}
	}
	if uint64(len(entries)) < pages {
		count++
	}
	return count
}

// all yields every page the table locates, in ascending order.
func (t *pageTable) all(yield func(uint32, location) bool) {
	for relative := range t.entries {
		if at, found := t.at(uint64(relative)); found && !yield(uint32(relative), at) {
			return
		}
	}
}

// reads reports, in ascending order of checkpoint, what the table's pages read
// from each checkpoint they name: what the root records for the segment.
func (t *pageTable) reads() []checkpointUse {
	bytes := make([]uint64, len(t.refs))
	for _, entry := range t.entries {
		if entry.ref != 0 {
			bytes[entry.ref-1] += uint64(entry.length)
		}
	}
	uses := make([]checkpointUse, 0, len(t.refs))
	for at, ref := range t.refs {
		if t.parts[at] != 0 {
			uses = append(uses, checkpointUse{ref: ref, bytes: bytes[at]})
		}
	}
	slices.SortFunc(uses, func(a, b checkpointUse) int { return compareRefs(a.ref, b.ref) })
	return uses
}

// mutable is a segment a publication may change, holding what the table
// locates.
func (t *pageTable) mutable() *segment {
	held := &segment{pages: make(map[uint32]location, t.pages)}
	for relative, at := range t.all {
		held.pages[relative] = at
	}
	return held
}

// tableOverhead is what one table costs besides its entries and checkpoints:
// the slices' headers and the struct.
const tableOverhead = int64(unsafe.Sizeof(pageTable{}))

// charge is the memory the table holds, which is what the cache charges the
// host's budget for keeping it.
func (t *pageTable) charge() int64 {
	charge := tableOverhead + int64(cap(t.entries))*int64(unsafe.Sizeof(tableEntry{})) +
		int64(cap(t.parts))*int64(unsafe.Sizeof(uint32(0)))
	for _, refs := range [][]control.Ref{t.refs, t.origins} {
		charge += int64(cap(refs)) * int64(unsafe.Sizeof(control.Ref{}))
		for _, ref := range refs {
			charge += int64(len(ref.VM))
		}
	}
	return charge
}

// WorkPageTable is the work of decoding a segment into its page table, which
// a simulation prices by the segment's encoded bytes (sim.Config.Compute):
// what a test counts to see how often a segment was decoded.
const WorkPageTable = "checkpoint/page-table"

// decodeTable decodes one segment's bytes into its page table.
func decodeTable(ctx context.Context, data []byte) (*pageTable, error) {
	if err := sim.Work(ctx, WorkPageTable, len(data)); err != nil {
		return nil, err
	}
	return parsePageTable(data)
}

// The fields of the protobuf messages a segment is encoded as
// (checkpoint/proto/sproutfs/checkpoint/v1/checkpoint.proto): Segment's pages,
// checkpoints and origins, Page's six and Ref's two.
const (
	segmentPagesField       = 1
	segmentCheckpointsField = 2
	segmentOriginsField     = 3

	pageNumberField     = 1
	pageCheckpointField = 2
	pagePartField       = 3
	pageOffsetField     = 4
	pageLengthField     = 5
	pageOriginField     = 6

	refVMField       = 1
	refSequenceField = 2
)

// maximumSegmentPages is the most pages any geometry this build reads puts in
// one segment, which bounds a page's relative number before the root says what
// the segment's geometry is.
const maximumSegmentPages = max(segmentPages2MiB, segmentPages4KiB)

// parsedPage is one Page message as it arrived.
type parsedPage struct {
	number, checkpoint, part, origin uint32
	offset, length                   uint64
}

// parsePageTable decodes one segment off the wire. It accepts exactly what
// unmarshalling the Segment message and checking each page used to: a message
// with a field Segment does not have is refused, while one inside a Page or a
// Ref is skipped, as the generated code would keep it unread; a scalar
// repeated is its last value; a uint32 takes the low 32 bits of its varint; a
// string must be UTF-8. What it checks of each page is what needs no root:
// that it lies within a segment of the largest geometry, is located once, names
// a checkpoint and an origin the segment lists, and has a member that fits in a
// part. What needs the root is Index.checkTable's.
func parsePageTable(data []byte) (*pageTable, error) {
	// A segment's pages are its relative numbers in ascending order, so the
	// table is as long as it has pages unless the segment has holes.
	table := &pageTable{entries: make([]tableEntry, 0, countPages(data))}
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 || !number.IsValid() {
			// The generated code refuses a field number past 2^29 - 1,
			// which a tag can carry.
			return nil, ErrCorrupt
		}
		data = data[size:]
		if kind != protowire.BytesType || number < segmentPagesField || number > segmentOriginsField {
			// A field Segment does not have, or one of its fields as another
			// wire type, is an unknown field of the segment itself.
			return nil, ErrCorrupt
		}
		value, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return nil, ErrCorrupt
		}
		data = data[size:]
		switch number {
		case segmentPagesField:
			page, err := parsePage(value)
			if err != nil {
				return nil, err
			}
			if err := table.add(page); err != nil {
				return nil, err
			}
		case segmentCheckpointsField, segmentOriginsField:
			ref, err := parseRef(value)
			if err != nil {
				return nil, err
			}
			if number == segmentCheckpointsField {
				table.refs = append(table.refs, ref)
			} else {
				table.origins = append(table.origins, ref)
			}
		}
	}
	// The pages come first on the wire, so what they name of the lists is
	// checked once the lists are in.
	table.parts = make([]uint32, len(table.refs))
	for _, entry := range table.entries {
		if entry.ref == 0 {
			continue
		}
		if int(entry.ref-1) >= len(table.refs) || entry.origin != 0 && int(entry.origin-1) >= len(table.origins) {
			return nil, ErrCorrupt
		}
		table.parts[entry.ref-1] = max(table.parts[entry.ref-1], entry.part+1)
	}
	if cap(table.entries) > len(table.entries)+len(table.entries)/8 {
		// What the table holds is what it is charged for.
		table.entries = slices.Clone(table.entries)
	}
	return table, nil
}

// countPages counts the Page messages of an encoded segment, or reports none
// where it does not scan as one; parsePageTable refuses those.
func countPages(data []byte) int {
	pages := 0
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 {
			return 0
		}
		data = data[size:]
		if size = protowire.ConsumeFieldValue(number, kind, data); size < 0 {
			return 0
		}
		data = data[size:]
		if number == segmentPagesField {
			pages++
		}
	}
	return min(pages, maximumSegmentPages)
}

// add puts one page into the table: once, within a segment of the largest
// geometry, with a member that fits in a part. The entries grow to the page,
// which the pages' ascending order on the wire makes an append.
func (t *pageTable) add(page parsedPage) error {
	if page.number >= maximumSegmentPages || page.checkpoint == ^uint32(0) ||
		page.length == 0 || page.offset > maximumPartSize || page.length > maximumPartSize-page.offset {
		return ErrCorrupt
	}
	if need := int(page.number) + 1; need > len(t.entries) {
		if need > cap(t.entries) {
			grown := make([]tableEntry, len(t.entries), min(max(need, 2*cap(t.entries)), maximumSegmentPages))
			copy(grown, t.entries)
			t.entries = grown
		}
		t.entries = t.entries[:need]
	}
	if t.entries[page.number].ref != 0 {
		return ErrCorrupt
	}
	t.entries[page.number] = tableEntry{offset: uint32(page.offset), length: uint32(page.length),
		part: page.part, ref: page.checkpoint + 1, origin: page.origin}
	t.pages++
	return nil
}

// parsePage decodes one Page message, skipping fields Page does not have.
func parsePage(data []byte) (parsedPage, error) {
	var page parsedPage
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 || !number.IsValid() {
			// The generated code refuses a field number past 2^29 - 1,
			// which a tag can carry.
			return page, ErrCorrupt
		}
		data = data[size:]
		if kind != protowire.VarintType || number < pageNumberField || number > pageOriginField {
			if size = protowire.ConsumeFieldValue(number, kind, data); size < 0 {
				return page, ErrCorrupt
			}
			data = data[size:]
			continue
		}
		value, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return page, ErrCorrupt
		}
		data = data[size:]
		switch number {
		case pageNumberField:
			page.number = uint32(value)
		case pageCheckpointField:
			page.checkpoint = uint32(value)
		case pagePartField:
			page.part = uint32(value)
		case pageOffsetField:
			page.offset = value
		case pageLengthField:
			page.length = value
		case pageOriginField:
			page.origin = uint32(value)
		}
	}
	if page.part == ^uint32(0) {
		// No root can state a count of parts past it, and one more than it,
		// which is what a table records, would wrap.
		return page, ErrCorrupt
	}
	return page, nil
}

// parseRef decodes one Ref message, skipping fields Ref does not have, and
// refuses one that names no checkpoint, as decodeRefs does.
func parseRef(data []byte) (control.Ref, error) {
	var ref control.Ref
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 || !number.IsValid() {
			// The generated code refuses a field number past 2^29 - 1,
			// which a tag can carry.
			return ref, ErrCorrupt
		}
		data = data[size:]
		switch {
		case number == refVMField && kind == protowire.BytesType:
			value, size := protowire.ConsumeBytes(data)
			if size < 0 || !utf8.Valid(value) {
				return ref, ErrCorrupt
			}
			ref.VM = string(value)
			data = data[size:]
		case number == refSequenceField && kind == protowire.VarintType:
			value, size := protowire.ConsumeVarint(data)
			if size < 0 {
				return ref, ErrCorrupt
			}
			ref.Sequence = value
			data = data[size:]
		default:
			if size = protowire.ConsumeFieldValue(number, kind, data); size < 0 {
				return ref, ErrCorrupt
			}
			data = data[size:]
		}
	}
	if !control.ValidID(ref.VM) || ref.Sequence == 0 {
		return ref, ErrCorrupt
	}
	return ref, nil
}

// checkTable refuses a table this index cannot read a volume's segment from:
// one wider than the volume's geometry puts in a segment, or one whose pages
// read from a checkpoint this root does not name, or from a part that
// checkpoint does not have. A table is shared by every root that addresses its
// segment, so each checks it against itself; it costs one lookup for each
// checkpoint the table names.
func (i *Index) checkTable(volume string, t *pageTable) error {
	table := i.volumes[volume]
	if table == nil {
		return ErrUnknownVolume
	}
	if uint64(len(t.entries)) > table.geometry.SegmentPages {
		return ErrCorrupt
	}
	for at, ref := range t.refs {
		if t.parts[at] == 0 {
			continue
		}
		entry, named := i.checkpoints[ref]
		if !named || t.parts[at] > entry.parts {
			return ErrCorrupt
		}
	}
	return nil
}
