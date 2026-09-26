package checkpoint

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	checkpointv1 "github.com/semistrict/sproutfs/checkpoint/internal/gen/sproutfs/checkpoint/v1"
	"github.com/semistrict/sproutfs/control"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// location is one encoded member of a part: the checkpoint whose parts
// hold it, the part, and the member's extent within it. A zero length locates
// nothing. The reference names a VM as well as a sequence, because a fork's
// entries go on naming its parent's checkpoints.
//
// origin is the checkpoint the member was first published under, which is the
// page's identity. It equals ref until compaction rewrites the member into a
// later checkpoint's parts and carries the origin forward: where the bytes sit
// is storage bookkeeping, and which bytes they are is the identity.
type location struct {
	ref    control.Ref
	origin control.Ref
	part   uint32
	offset uint64
	length uint64
}

func (l location) isZero() bool { return l.length == 0 }

// segmentAddress is where one segment of a volume's page table is fetched from:
// the checkpoint whose index object holds it, and the extent inside that object.
// A segment's identity is the checkpoint that wrote it together with its volume
// and number, the way a page's is its origin; this is only where to read it, so
// a fetch stays one range get.
type segmentAddress struct {
	ref    control.Ref
	offset uint64
	length uint64
}

// checkpointCost is what one checkpoint's parts cost: how many parts it
// has and how many encoded member bytes they hold. Compaction measures a
// checkpoint's live bytes against that total. A checkpoint of no parts — a new
// VM's root, or one a root names only because a segment lives in its index
// object — states zero of both.
//
// emptied is set when a checkpoint's compaction leaves nothing in this root
// reading from those parts: the root goes on naming it for that one checkpoint,
// because a reader holding the view it replaced still reads from it.
type checkpointCost struct {
	parts   uint32
	bytes   uint64
	emptied uint64
}

// segment is one range of a volume's page table: the pages one segment of that
// volume's geometry covers that hold bytes, keyed by their number relative to
// the segment's first page. A page with no entry reads as zeroes, and a segment
// with no entry at all is an empty one.
type segment struct {
	pages map[uint32]location
}

func newSegment() *segment { return &segment{pages: make(map[uint32]location)} }

// checkpointUse is what one segment's page entries read from one checkpoint's
// parts: the checkpoint, and the encoded member bytes they name in it.
type checkpointUse struct {
	ref   control.Ref
	bytes uint64
}

// reads reports, in ascending order of checkpoint, what this segment's page
// entries read from each checkpoint they name. It is what the root records for
// the segment, so reclamation and compaction answer both what a checkpoint
// reads from and how much of each is still read for without opening a segment
// at all.
func (s *segment) reads() []checkpointUse {
	bytes := make(map[control.Ref]uint64, len(s.pages))
	for _, at := range s.pages {
		bytes[at.ref] += at.length
	}
	uses := make([]checkpointUse, 0, len(bytes))
	for _, ref := range sortedRefs(bytes) {
		uses = append(uses, checkpointUse{ref: ref, bytes: bytes[ref]})
	}
	return uses
}

// segmentEntry is where one segment of a volume's page table lives and what its
// pages read from. The address is inside the index object of the checkpoint that
// wrote the segment, which is this one for a segment this checkpoint changed and
// an earlier one for every other.
type segmentEntry struct {
	at    segmentAddress
	reads []checkpointUse
}

// segmentKey names one segment of one volume, which is what an index memoises
// its decoded segments by.
type segmentKey struct {
	volume string
	number uint64
}

// volumeTable is one volume's size, its geometry and the segments of its page
// table. The geometry is what every page number of this volume is divided by,
// here and in every reader: it is the volume's own and never a constant of this
// package.
//
// An ephemeral volume has no segments, ever: no checkpoint holds its pages.
type volumeTable struct {
	size      uint64
	geometry  Geometry
	ephemeral bool
	segments  map[uint64]segmentEntry
}

// Index is the decoded root of one published checkpoint: where every volume's
// page table segments live, the checkpoints those segments and their pages read
// from, and where the VMM state is. An absent page reads as zeroes, and so does
// every page of an absent segment. The root is complete on its own — it names no
// parent — and the segments it addresses are fetched on demand.
//
// An Index is immutable once published and safe for concurrent use; the
// segments it has decoded are a memo in front of the store's page cache.
type Index struct {
	ref     control.Ref
	names   []string
	volumes map[string]*volumeTable
	// checkpoints names every checkpoint this index names, including this one,
	// so reclamation and compaction never open another index object.
	checkpoints map[control.Ref]checkpointCost
	state       location
	// vcpus is the processor count a boot of this checkpoint gives the guest,
	// zero where none is recorded.
	vcpus uint32
	// store is what segments are read through. A root built without one — which
	// nothing but a test does — can locate nothing it did not write itself.
	store *Store
	// mu guards loaded, the decoded segments this index has already fetched.
	mu     sync.Mutex
	loaded map[segmentKey]*segment
}

// newIndex is an empty root under ref, read through store.
func newIndex(store *Store, ref control.Ref) *Index {
	return &Index{ref: ref, store: store, volumes: make(map[string]*volumeTable),
		checkpoints: make(map[control.Ref]checkpointCost), loaded: make(map[segmentKey]*segment)}
}

// Ref reports the checkpoint this index publishes.
func (i *Index) Ref() control.Ref { return i.ref }

// Volumes reports the volume names in ascending order.
func (i *Index) Volumes() []string { return slices.Clone(i.names) }

// Size reports a volume's size in bytes, zero for a volume this index does not
// describe.
func (i *Index) Size(volume string) uint64 {
	table := i.volumes[volume]
	if table == nil {
		return 0
	}
	return table.size
}

// Geometry reports the page geometry a volume was created with, which this
// checkpoint records and every reader of it divides page numbers by. It is the
// zero Geometry for a volume this index does not describe.
func (i *Index) Geometry(volume string) Geometry {
	table := i.volumes[volume]
	if table == nil {
		return Geometry{}
	}
	return table.geometry
}

// Ephemeral reports a volume no checkpoint holds, whose pages this index
// never locates: a VM opened here gets it back zeroed at its recorded size.
func (i *Index) Ephemeral(volume string) bool {
	table := i.volumes[volume]
	return table != nil && table.ephemeral
}

// VCPUs is how many processors a boot of this checkpoint gives the guest, zero
// where the checkpoint records none and the host's default applies.
func (i *Index) VCPUs() int { return int(i.vcpus) }

// HasState reports whether VMM state was published with this checkpoint.
func (i *Index) HasState() bool { return !i.state.isZero() }

// Checkpoints reports every checkpoint this index reads from, including its
// own, in ascending order. A fork's list names its parent's checkpoints. A
// checkpoint this one's compaction emptied is not among them: nothing here
// reads its parts, and the root names it only so that reclamation spares it for
// the readers still holding the view this index replaces.
func (i *Index) Checkpoints() []control.Ref {
	read := make(map[control.Ref]bool, len(i.checkpoints))
	for ref, entry := range i.checkpoints {
		if entry.emptied == 0 {
			read[ref] = true
		}
	}
	return sortedRefs(read)
}

// named reports every checkpoint this index names, the ones it only spares
// included. Reclamation works from this rather than from Checkpoints: one the
// index no longer reads but still names must not be deleted under the readers
// that do.
func (i *Index) named() []control.Ref { return sortedRefs(i.checkpoints) }

// parts reports how many parts one checkpoint this index names has, which is
// what bounds a part number a page entry may carry.
func (i *Index) parts(ref control.Ref) uint32 { return i.checkpoints[ref].parts }

// readCheckpoints reports every checkpoint this index reads: its own, which
// holds the root itself, the one whose index object holds each segment, the
// ones that segment's pages name — which the root records, so no segment is
// opened for this — and the state's. It is what says which checkpoint entries a
// new index still needs and which a compaction has emptied.
func (i *Index) readCheckpoints() map[control.Ref]bool {
	read := make(map[control.Ref]bool, len(i.checkpoints))
	read[i.ref] = true
	for _, table := range i.volumes {
		for _, entry := range table.segments {
			read[entry.at.ref] = true
			for _, use := range entry.reads {
				read[use.ref] = true
			}
		}
	}
	if !i.state.isZero() {
		read[i.state.ref] = true
	}
	return read
}

func sortedRefs[V any](m map[control.Ref]V) []control.Ref {
	refs := slices.Collect(maps.Keys(m))
	slices.SortFunc(refs, compareRefs)
	return refs
}

func compareRefs(a, b control.Ref) int {
	if a.VM != b.VM {
		if a.VM < b.VM {
			return -1
		}
		return 1
	}
	switch {
	case a.Sequence < b.Sequence:
		return -1
	case a.Sequence > b.Sequence:
		return 1
	}
	return 0
}

// identity names the page an entry locates, which is what a pager keys a
// resident page by and what the page cache keys its bytes by. It is the
// member's origin rather than the checkpoint that currently holds it, so
// compaction moving those bytes leaves it alone.
func identityOf(volume string, number uint64, at location) control.Identity {
	return control.Identity{Ref: at.origin, Volume: volume, Page: number}
}

// segmentAt reports one segment of a volume's page table, fetching it through
// the store's cache the first time this index is asked for it. A segment the
// root does not address is an empty one and costs no I/O.
//
// The fetch runs outside the lock, so one slow segment does not hold up the
// others; two callers that raced share whichever copy landed first.
func (i *Index) segmentAt(ctx context.Context, volume string, number uint64) (*segment, error) {
	key := segmentKey{volume: volume, number: number}
	i.mu.Lock()
	held, found := i.loaded[key]
	i.mu.Unlock()
	if found {
		return held, nil
	}
	table := i.volumes[volume]
	if table == nil {
		return nil, ErrUnknownVolume
	}
	loaded := newSegment()
	if entry, addressed := table.segments[number]; addressed {
		if i.store == nil {
			return nil, ErrCorrupt
		}
		data, release, err := i.store.loadSegment(ctx, volume, number, entry.at)
		if err != nil {
			return nil, err
		}
		loaded, err = i.decodeSegment(volume, data)
		release()
		if err != nil {
			return nil, err
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.loaded == nil {
		i.loaded = make(map[segmentKey]*segment)
	}
	if held, found := i.loaded[key]; found {
		return held, nil
	}
	i.loaded[key] = loaded
	return loaded, nil
}

// pageAt reports where one page's current bytes live, fetching the segment that
// locates it if this index has not already.
func (i *Index) pageAt(ctx context.Context, volume string, number uint64) (location, bool, error) {
	table := i.volumes[volume]
	if table == nil {
		return location{}, false, ErrUnknownVolume
	}
	held, err := i.segmentAt(ctx, volume, table.geometry.SegmentOf(number))
	if err != nil {
		return location{}, false, err
	}
	at, found := held.pages[table.geometry.OffsetIn(number)]
	return at, found, nil
}

// Locate reports where the current bytes of a range live. The extents are
// sorted, adjacent and cover the range exactly, and each of them lies within
// one page. Consecutive pages are not merged: a page is the unit a member holds
// and the unit a pager keys a resident page by. A hole is one extent however large it
// is.
//
// It may fetch the segments the range falls in, which is why it takes a
// context; a segment already decoded costs nothing, and a range inside a
// segment the root does not address costs no I/O at all.
func (i *Index) Locate(ctx context.Context, volume string, offset, length uint64) ([]control.Extent, error) {
	table := i.volumes[volume]
	if table == nil {
		return nil, ErrUnknownVolume
	}
	if offset > table.size || length > table.size-offset {
		return nil, ErrInvalidRange
	}
	var extents []control.Extent
	add := func(offset, length uint64, identity control.Identity) {
		if n := len(extents); n > 0 && identity.Zero && extents[n-1].Identity == identity && extents[n-1].Offset+extents[n-1].Length == offset {
			extents[n-1].Length += length
			return
		}
		extents = append(extents, control.Extent{Offset: offset, Length: length, Identity: identity})
	}
	end := offset + length
	geometry := table.geometry
	var held *segment
	var current uint64
	for cursor := offset; cursor < end; {
		number := geometry.PageOf(cursor)
		start, span := geometry.PageSpan(table.size, number)
		limit := min(end, start+span)
		if held == nil || current != geometry.SegmentOf(number) {
			loaded, err := i.segmentAt(ctx, volume, geometry.SegmentOf(number))
			if err != nil {
				return nil, err
			}
			held, current = loaded, geometry.SegmentOf(number)
		}
		if at, found := held.pages[geometry.OffsetIn(number)]; found {
			add(cursor, limit-cursor, identityOf(volume, number, at))
		} else {
			add(cursor, limit-cursor, control.ZeroIdentity)
		}
		cursor = limit
	}
	return extents, nil
}

// encode marshals the root deterministically. Volumes, segments and checkpoints
// are emitted in ascending order so a retried publication produces identical
// bytes, which is what lets the index object's create-if-absent write settle a
// retry by digest.
//
// Every checkpoint the index names is emitted, the ones it only spares included:
// a sweep works from what the index names, and after a takeover that index is
// one read back from the store, so an emptied checkpoint left out here loses the
// checkpoint of grace it owes the view this index replaced.
func (i *Index) encode() ([]byte, error) {
	refs := i.named()
	position := make(map[control.Ref]uint32, len(refs))
	entries := make([]*checkpointv1.Checkpoint, 0, len(refs))
	for at, ref := range refs {
		position[ref] = uint32(at)
		entry := i.checkpoints[ref]
		entries = append(entries, checkpointv1.Checkpoint_builder{
			Vm: proto.String(ref.VM), Sequence: proto.Uint64(ref.Sequence),
			Parts: proto.Uint32(entry.parts), Bytes: proto.Uint64(entry.bytes),
			Emptied: proto.Uint64(entry.emptied),
		}.Build())
	}
	origins := i.sortedOrigins()
	slot := make(map[control.Ref]uint32, len(origins))
	for at, ref := range origins {
		slot[ref] = uint32(at) + 1
	}
	volumes := make([]*checkpointv1.Volume, 0, len(i.names))
	for _, name := range i.names {
		table := i.volumes[name]
		numbers := slices.Sorted(maps.Keys(table.segments))
		segments := make([]*checkpointv1.SegmentEntry, 0, len(numbers))
		for _, number := range numbers {
			entry := table.segments[number]
			reads := make([]*checkpointv1.CheckpointUse, 0, len(entry.reads))
			for _, use := range entry.reads {
				reads = append(reads, checkpointv1.CheckpointUse_builder{
					Checkpoint: proto.Uint32(position[use.ref]), Bytes: proto.Uint64(use.bytes),
				}.Build())
			}
			slices.SortFunc(reads, func(a, b *checkpointv1.CheckpointUse) int {
				return int(a.GetCheckpoint()) - int(b.GetCheckpoint())
			})
			segments = append(segments, checkpointv1.SegmentEntry_builder{
				Number: proto.Uint64(number), Checkpoint: proto.Uint32(position[entry.at.ref]),
				Offset: proto.Uint64(entry.at.offset), Length: proto.Uint64(entry.at.length),
				Reads: reads,
			}.Build())
		}
		volume := checkpointv1.Volume_builder{
			Name: proto.String(name), Size: proto.Uint64(table.size),
			PageSize: proto.Uint64(table.geometry.PageSize),
			// The pages one segment covers is recorded rather than derived: a
			// reader divides page numbers by what the root says, not by a
			// constant of the build that happens to be reading.
			SegmentPages: proto.Uint64(table.geometry.SegmentPages),
			Segments:     segments,
		}.Build()
		// A volume every checkpoint holds leaves the field out, so every root
		// written before the field existed decodes, and re-encodes, as it was.
		if table.ephemeral {
			volume.SetEphemeral(true)
		}
		volumes = append(volumes, volume)
	}
	originRefs := make([]*checkpointv1.Ref, 0, len(origins))
	for _, ref := range origins {
		originRefs = append(originRefs, refMessage(ref))
	}
	message := checkpointv1.Root_builder{
		Volumes:         volumes,
		Checkpoints:     entries,
		Origins:         originRefs,
		StateCheckpoint: proto.Uint32(position[i.state.ref]),
		StatePart:       proto.Uint32(i.state.part),
		StateOffset:     proto.Uint64(i.state.offset),
		StateLength:     proto.Uint64(i.state.length),
		StateOrigin:     proto.Uint32(slot[i.state.origin]),
	}.Build()
	// A root that records no count leaves the field out, so every root written
	// before the field existed decodes, and re-encodes, as it was.
	if i.vcpus != 0 {
		message.SetVcpus(i.vcpus)
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

func refMessage(ref control.Ref) *checkpointv1.Ref {
	return checkpointv1.Ref_builder{Vm: proto.String(ref.VM), Sequence: proto.Uint64(ref.Sequence)}.Build()
}

// sortedOrigins lists the origins the root itself carries, which is the state's
// alone when compaction has moved it out of the checkpoint that published it. A
// page's origin is carried by the segment that locates it.
func (i *Index) sortedOrigins() []control.Ref {
	if i.state.isZero() || i.state.origin == i.state.ref {
		return nil
	}
	return []control.Ref{i.state.origin}
}

// encodeSegment marshals one segment deterministically: its pages in ascending
// order, and the checkpoints and origins they name in ascending order too, so a
// segment whose table has not changed encodes to the same bytes wherever it is
// written.
func encodeSegment(held *segment) ([]byte, error) {
	uses := held.reads()
	position := make(map[control.Ref]uint32, len(uses))
	refs := make([]*checkpointv1.Ref, 0, len(uses))
	for at, use := range uses {
		position[use.ref] = uint32(at)
		refs = append(refs, refMessage(use.ref))
	}
	moved := make(map[control.Ref]bool)
	for _, at := range held.pages {
		if at.origin != at.ref {
			moved[at.origin] = true
		}
	}
	origins := sortedRefs(moved)
	slot := make(map[control.Ref]uint32, len(origins))
	originRefs := make([]*checkpointv1.Ref, 0, len(origins))
	for at, ref := range origins {
		slot[ref] = uint32(at) + 1
		originRefs = append(originRefs, refMessage(ref))
	}
	numbers := slices.Sorted(maps.Keys(held.pages))
	pages := make([]*checkpointv1.Page, 0, len(numbers))
	for _, number := range numbers {
		at := held.pages[number]
		pages = append(pages, checkpointv1.Page_builder{
			Number: proto.Uint32(number), Checkpoint: proto.Uint32(position[at.ref]),
			Part: proto.Uint32(at.part), Offset: proto.Uint64(at.offset),
			Length: proto.Uint64(at.length), Origin: proto.Uint32(slot[at.origin]),
		}.Build())
	}
	message := checkpointv1.Segment_builder{Pages: pages, Checkpoints: refs, Origins: originRefs}.Build()
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

// decodeSegment parses one segment against the root that addressed it: every
// page must lie within the segment's own range and locate a member of a
// checkpoint this root names.
//
// Whether a page lies inside the volume is not settled here. A publication that
// shrinks a volume reads the segment straddling the new end before it drops the
// pages the cut took, so a segment is briefly wider than the volume it belongs
// to; what a published root owes is checked by CheckIndex, and nothing reads
// past a volume's size in between.
func (i *Index) decodeSegment(volume string, data []byte) (*segment, error) {
	table := i.volumes[volume]
	if table == nil {
		return nil, ErrUnknownVolume
	}
	message := new(checkpointv1.Segment)
	if err := proto.Unmarshal(data, message); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrCorrupt
	}
	refs, err := decodeRefs(message.GetCheckpoints())
	if err != nil {
		return nil, err
	}
	origins, err := decodeRefs(message.GetOrigins())
	if err != nil {
		return nil, err
	}
	held := newSegment()
	for _, entry := range message.GetPages() {
		relative := entry.GetNumber()
		if uint64(relative) >= table.geometry.SegmentPages {
			return nil, ErrCorrupt
		}
		if _, held := held.pages[relative]; held {
			return nil, ErrCorrupt
		}
		if int(entry.GetCheckpoint()) >= len(refs) {
			return nil, ErrCorrupt
		}
		ref := refs[entry.GetCheckpoint()]
		at := location{ref: ref, origin: ref, part: entry.GetPart(),
			offset: entry.GetOffset(), length: entry.GetLength()}
		if slot := entry.GetOrigin(); slot != 0 {
			if int(slot-1) >= len(origins) {
				return nil, ErrCorrupt
			}
			at.origin = origins[slot-1]
		}
		if err := i.checkLocation(at); err != nil {
			return nil, err
		}
		held.pages[relative] = at
	}
	return held, nil
}

func decodeRefs(entries []*checkpointv1.Ref) ([]control.Ref, error) {
	refs := make([]control.Ref, 0, len(entries))
	for _, entry := range entries {
		at := control.Ref{VM: entry.GetVm(), Sequence: entry.GetSequence()}
		if !control.ValidID(at.VM) || at.Sequence == 0 {
			return nil, ErrCorrupt
		}
		refs = append(refs, at)
	}
	return refs, nil
}

const (
	// indexRecordSize is the fixed record an index object begins and ends with:
	// the magic that says the object is one, the format version, and the extent
	// of the root. The segments the checkpoint changed lie after the first
	// record, the root after them, and the same record again after the root, so
	// that a reader of the object's end alone has what locates the root — which
	// is how a part is read too — and a reader of its head alone can still name
	// the version it was written under.
	indexRecordSize = 32
	// indexMagic is "SPROUTIX".
	indexMagic = 0x5350524f55544958
	// indexFormatVersion is the index layout this build writes and the only one
	// it reads. Version 8 records every volume's page geometry in the root, and
	// ends the object with the record it begins with; a version 7 root states no
	// page size, and its page numbers are 2 MiB pages that must not be read as
	// anything else.
	indexFormatVersion = 8
)

// putIndexRecord writes the fixed record of an index object into dst, which is
// indexRecordSize bytes.
func putIndexRecord(dst []byte, rootOffset, rootLength uint64) {
	binary.LittleEndian.PutUint64(dst[0:], indexMagic)
	binary.LittleEndian.PutUint32(dst[8:], indexFormatVersion)
	binary.LittleEndian.PutUint32(dst[12:], 0)
	binary.LittleEndian.PutUint64(dst[16:], rootOffset)
	binary.LittleEndian.PutUint64(dst[24:], rootLength)
}

// isIndexRecord reports whether a record carries the magic this build stamps.
func isIndexRecord(record []byte) bool {
	return len(record) >= indexRecordSize && binary.LittleEndian.Uint64(record[0:]) == indexMagic
}

// decodeIndexRecord reads the record of an index object of size bytes and
// reports where the root is: after the first record and whatever segments
// follow it, and ending exactly where the last record begins. A record of
// another version refuses with the version it carries, because nothing this
// build reads looks like that.
func decodeIndexRecord(record []byte, size uint64) (offset, length uint64, err error) {
	if !isIndexRecord(record) {
		return 0, 0, ErrCorrupt
	}
	if version := binary.LittleEndian.Uint32(record[8:]); version != indexFormatVersion {
		return 0, 0, fmt.Errorf("%w: checkpoint index format version %d, want %d",
			ErrCorrupt, version, indexFormatVersion)
	}
	offset = binary.LittleEndian.Uint64(record[16:])
	length = binary.LittleEndian.Uint64(record[24:])
	if length == 0 || length > maximumRootExtent || offset < indexRecordSize ||
		size < 2*indexRecordSize || offset > size-indexRecordSize || length != size-indexRecordSize-offset {
		return 0, 0, ErrCorrupt
	}
	return offset, length, nil
}

// supersededFormat is the field a root carried its own format version in while
// it was the whole of an index object. The record carries it now, so an object
// carrying this field was written by a build this one does not read, and is
// refused with the version it names.
const supersededFormat = 1

// supersededRootVersion reports the checkpoint index format version a message
// carries in the field a root no longer has, and whether it carries one at all.
// It is what tells an index object written before this layout from one this
// build wrote.
func supersededRootVersion(data []byte) (uint32, bool) {
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 {
			return 0, false
		}
		data = data[size:]
		if number == supersededFormat && kind == protowire.VarintType {
			value, size := protowire.ConsumeVarint(data)
			if size < 0 {
				return 0, false
			}
			return uint32(value), true
		}
		if size = protowire.ConsumeFieldValue(number, kind, data); size < 0 {
			return 0, false
		}
		data = data[size:]
	}
	return 0, false
}

// decodeRoot parses the root of the checkpoint published under ref and rejects
// anything it cannot use. A root that survives this is self-consistent: every
// segment lies inside its volume, is addressed inside the index object of a
// checkpoint the root names, and reads only from checkpoints the root names.
func decodeRoot(store *Store, ref control.Ref, data []byte) (*Index, error) {
	// A superseded root is refused before anything else is read, and the refusal
	// names the version it was written under: a checkpoint written by a build
	// this one does not read must say so rather than report a root that
	// disagrees with itself.
	if version, superseded := supersededRootVersion(data); superseded {
		return nil, fmt.Errorf("%w: checkpoint index format version %d, which this build does not read: "+
			"a checkpoint is an index object and its parts", ErrCorrupt, version)
	}
	message := new(checkpointv1.Root)
	if err := proto.Unmarshal(data, message); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrCorrupt
	}
	index := &Index{ref: ref, store: store,
		volumes:     make(map[string]*volumeTable, len(message.GetVolumes())),
		checkpoints: make(map[control.Ref]checkpointCost, len(message.GetCheckpoints())),
		loaded:      make(map[segmentKey]*segment)}
	refs := make([]control.Ref, 0, len(message.GetCheckpoints()))
	for _, entry := range message.GetCheckpoints() {
		at := control.Ref{VM: entry.GetVm(), Sequence: entry.GetSequence()}
		if !control.ValidID(at.VM) || at.Sequence == 0 {
			return nil, ErrCorrupt
		}
		if _, duplicate := index.checkpoints[at]; duplicate {
			return nil, ErrCorrupt
		}
		index.checkpoints[at] = checkpointCost{parts: entry.GetParts(),
			bytes: entry.GetBytes(), emptied: entry.GetEmptied()}
		refs = append(refs, at)
	}
	origins, err := decodeRefs(message.GetOrigins())
	if err != nil {
		return nil, err
	}
	locate := func(slot, origin, part uint32, offset, length uint64) (location, error) {
		if int(slot) >= len(refs) {
			return location{}, ErrCorrupt
		}
		at := location{ref: refs[slot], origin: refs[slot], part: part, offset: offset, length: length}
		if origin != 0 {
			if int(origin-1) >= len(origins) {
				return location{}, ErrCorrupt
			}
			at.origin = origins[origin-1]
		}
		if err := index.checkLocation(at); err != nil {
			return location{}, err
		}
		return at, nil
	}
	for _, volume := range message.GetVolumes() {
		name := volume.GetName()
		if !validName(name) || index.volumes[name] != nil || volume.GetSize()%SectorSize != 0 {
			return nil, ErrCorrupt
		}
		// The geometry is read back as the pair the root recorded rather than
		// rebuilt from the page size, because it is what every page number of
		// this volume is divided by: a root carrying a pair this build does not
		// write is one it cannot divide by, and is refused whole.
		geometry := Geometry{PageSize: volume.GetPageSize(), SegmentPages: volume.GetSegmentPages()}
		if !geometry.supported() {
			return nil, fmt.Errorf("%w: %s has %d-byte pages, %d to a segment, which this build does not read",
				ErrCorrupt, name, geometry.PageSize, geometry.SegmentPages)
		}
		// No checkpoint holds an ephemeral volume, so a root that addresses a
		// segment of one disagrees with itself.
		if volume.GetEphemeral() && len(volume.GetSegments()) != 0 {
			return nil, fmt.Errorf("%w: the ephemeral volume %s has %d segments",
				ErrCorrupt, name, len(volume.GetSegments()))
		}
		table := &volumeTable{size: volume.GetSize(), geometry: geometry, ephemeral: volume.GetEphemeral(),
			segments: make(map[uint64]segmentEntry, len(volume.GetSegments()))}
		count := geometry.SegmentCount(table.size)
		for _, entry := range volume.GetSegments() {
			number := entry.GetNumber()
			if number >= count {
				return nil, ErrCorrupt
			}
			if _, duplicate := table.segments[number]; duplicate {
				return nil, ErrCorrupt
			}
			if int(entry.GetCheckpoint()) >= len(refs) {
				return nil, ErrCorrupt
			}
			at := segmentAddress{ref: refs[entry.GetCheckpoint()],
				offset: entry.GetOffset(), length: entry.GetLength()}
			if err := index.checkSegmentAddress(at); err != nil {
				return nil, err
			}
			// Reads is what the root answers "which checkpoints does this one
			// read from, and how much of each is still read for" out of, so it
			// is emitted sorted and read back strictly ascending: a duplicate,
			// a disorder or a checkpoint read for no bytes is a root that
			// disagrees with the one this build writes.
			reads := make([]checkpointUse, 0, len(entry.GetReads()))
			previous := -1
			for _, use := range entry.GetReads() {
				slot := use.GetCheckpoint()
				if int(slot) >= len(refs) || int(slot) <= previous || use.GetBytes() == 0 {
					return nil, ErrCorrupt
				}
				previous = int(slot)
				reads = append(reads, checkpointUse{ref: refs[slot], bytes: use.GetBytes()})
			}
			table.segments[number] = segmentEntry{at: at, reads: reads}
		}
		index.volumes[name] = table
		index.names = append(index.names, name)
	}
	slices.Sort(index.names)
	index.vcpus = message.GetVcpus()
	if index.vcpus > maximumVCPUs {
		return nil, ErrCorrupt
	}
	if message.GetStateLength() != 0 {
		state, err := locate(message.GetStateCheckpoint(), message.GetStateOrigin(),
			message.GetStatePart(), message.GetStateOffset(), message.GetStateLength())
		if err != nil {
			return nil, err
		}
		index.state = state
	}
	return index, nil
}

// checkLocation rejects a member the index cannot read: one with no
// bytes, one in a part the checkpoint it names does not have, or one past a
// part's bound.
func (i *Index) checkLocation(at location) error {
	entry, named := i.checkpoints[at.ref]
	if !named || at.length == 0 || at.part >= entry.parts ||
		at.offset > maximumPartSize || at.length > maximumPartSize-at.offset {
		return ErrCorrupt
	}
	if !control.ValidID(at.origin.VM) || at.origin.Sequence == 0 {
		return ErrCorrupt
	}
	return nil
}

// checkSegmentAddress rejects a segment the index cannot fetch: one with no
// bytes, one larger than a segment may be, or one outside the index object of a
// checkpoint this root names.
func (i *Index) checkSegmentAddress(at segmentAddress) error {
	if _, named := i.checkpoints[at.ref]; !named {
		return ErrCorrupt
	}
	if at.length == 0 || at.length > maximumSegmentExtent || at.offset < indexRecordSize ||
		at.offset > maximumIndexSize || at.length > maximumIndexSize-at.offset {
		return ErrCorrupt
	}
	return nil
}

// retainReferencedCheckpoints drops every checkpoint entry no segment, no page
// and no state member of this index still names, except its own — which holds
// the root itself — and the ones this checkpoint's own compaction emptied: those
// it keeps for this one checkpoint, so that reclamation leaves them for a reader
// still holding the view this index replaces. One an earlier checkpoint emptied
// has had its checkpoint of grace and goes.
func (i *Index) retainReferencedCheckpoints() {
	used := i.readCheckpoints()
	for ref, entry := range i.checkpoints {
		if !used[ref] && entry.emptied != i.ref.Sequence {
			delete(i.checkpoints, ref)
		}
	}
}
