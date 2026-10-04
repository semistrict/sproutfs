package checkpoint

import (
	"errors"
	"maps"
	"slices"
	"testing"

	checkpointv1 "github.com/semistrict/sproutfs/checkpoint/internal/gen/sproutfs/checkpoint/v1"
	"github.com/semistrict/sproutfs/control"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// tableOf is one segment's page table as index reads it, held only for the
// test.
func tableOf(t *testing.T, index *Index, volume string, number uint64) *pageTable {
	t.Helper()
	held, release, err := index.table(t.Context(), volume, number)
	if err != nil {
		t.Fatal(err)
	}
	release()
	return held
}

// protobufSegment decodes a segment as every reader did before the page
// table, through the generated Segment message: what parsePageTable must
// accept and refuse exactly as, and decode exactly to.
func protobufSegment(index *Index, volume string, data []byte) (*segment, error) {
	table := index.volumes[volume]
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
		if _, found := held.pages[relative]; found {
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
		if err := index.checkLocation(at); err != nil {
			return nil, err
		}
		held.pages[relative] = at
	}
	return held, nil
}

// tableIndex is a root naming two checkpoints of two and three parts and a
// 4 KiB-page volume, and the segment of it the fuzzing starts from: pages at
// both ends of the segment, one in each checkpoint, one compaction moved.
func tableIndex() (*Index, *segment) {
	first, second := control.Ref{VM: "table", Sequence: 2}, control.Ref{VM: "table", Sequence: 3}
	moved := control.Ref{VM: "parent", Sequence: 9}
	index := newIndex(nil, second)
	index.checkpoints[first] = checkpointCost{parts: 2}
	index.checkpoints[second] = checkpointCost{parts: 3}
	index.volumes["ram"] = &volumeTable{size: 4 * segmentPages4KiB * PageSize4KiB, geometry: at4KiB,
		segments: map[uint64]segmentEntry{}}
	held := newSegment()
	held.pages[0] = location{ref: first, origin: first, part: 1, offset: 4144, length: 4144}
	held.pages[7] = location{ref: second, origin: second, part: 2, offset: 0, length: 12}
	held.pages[9] = location{ref: second, origin: moved, part: 0, offset: 64 << 20, length: 4144}
	held.pages[segmentPages4KiB-1] = location{ref: first, origin: first, part: 0, offset: 8, length: 4096}
	return index, held
}

// sameAs reports whether a page table locates exactly the pages of a segment.
func sameAs(table *pageTable, held *segment) bool {
	got := make(map[uint32]location, table.pages)
	for relative, at := range table.all {
		got[relative] = at
	}
	return maps.Equal(got, held.pages) && table.pages == len(held.pages)
}

// A page table is what the segment it was decoded from encodes: every page,
// its checkpoint, part, extent and origin, and what its pages read from each
// checkpoint, which is what the root records.
func TestAPageTableIsTheSegmentItDecodes(t *testing.T) {
	index, held := tableIndex()
	data, err := encodeSegment(held)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parsePageTable(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.checkTable("ram", table); err != nil {
		t.Fatal(err)
	}
	if !sameAs(table, held) {
		t.Fatalf("the table locates %+v, want %+v", table.mutable().pages, held.pages)
	}
	if got, want := table.reads(), held.reads(); !slices.Equal(got, want) {
		t.Fatalf("the table reads %v, want %v", got, want)
	}
	if _, found := table.at(8); found {
		t.Fatal("the table locates page 8, which the segment does not")
	}
	if _, found := table.at(segmentPages4KiB); found {
		t.Fatal("the table locates a page past its segment")
	}
}

// A table is checked against each root that reads it: a 2 MiB-page volume's
// segment is 256 pages, so one locating page 300 is refused there; a root that
// does not name a checkpoint a page reads from, or names it with fewer parts
// than a page names, refuses the table too.
func TestAPageTableIsCheckedAgainstTheRootThatReadsIt(t *testing.T) {
	index, held := tableIndex()
	data, err := encodeSegment(held)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parsePageTable(data)
	if err != nil {
		t.Fatal(err)
	}
	narrow := newIndex(nil, index.ref)
	maps.Copy(narrow.checkpoints, index.checkpoints)
	narrow.volumes["disk"] = &volumeTable{size: 1 << 30, geometry: at2MiB, segments: map[uint64]segmentEntry{}}
	if err := narrow.checkTable("disk", table); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a 2 MiB-page root took a table of 16,384 pages: %v", err)
	}
	for ref, cost := range index.checkpoints {
		missing := newIndex(nil, index.ref)
		maps.Copy(missing.checkpoints, index.checkpoints)
		missing.volumes = index.volumes
		delete(missing.checkpoints, ref)
		if err := missing.checkTable("ram", table); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("a root not naming %v took a table reading from it: %v", ref, err)
		}
		missing.checkpoints[ref] = checkpointCost{parts: cost.parts - 1}
		if err := missing.checkTable("ram", table); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("a root naming %d parts of %v took a table reading part %d: %v",
				cost.parts-1, ref, cost.parts-1, err)
		}
	}
}

// parsePageTable takes the wire as the generated Segment message did: it
// refuses and accepts exactly the same bytes, and what it accepts it decodes to
// the same pages. The seeds are a segment as written, then with each kind of
// field the decoder must skip or refuse appended: unknown fields in a page and
// a ref, a field of the segment's own it does not have, a page past the
// segment, a duplicate page, and a varint's high bits.
func FuzzPageTableDecodesAsTheProtobufDid(f *testing.F) {
	index, held := tableIndex()
	data, err := encodeSegment(held)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte{})
	page := func(fields ...[]byte) []byte {
		var message []byte
		for _, field := range fields {
			message = append(message, field...)
		}
		return protowire.AppendBytes(protowire.AppendTag(nil, segmentPagesField, protowire.BytesType), message)
	}
	varint := func(number protowire.Number, value uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, number, protowire.VarintType), value)
	}
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7)
	f.Add(append(append([]byte(nil), data...), page(varint(pageNumberField, 3), varint(pageLengthField, 1), unknown)...))
	f.Add(append(append([]byte(nil), data...), unknown...))
	f.Add(append(append([]byte(nil), data...), page(varint(pageNumberField, 1<<32|4),
		varint(pageLengthField, 1))...))
	f.Add(append(append([]byte(nil), data...), page(varint(pageNumberField, 7), varint(pageLengthField, 1))...))
	f.Add(append(append([]byte(nil), data...), page(varint(pageNumberField, segmentPages4KiB),
		varint(pageLengthField, 1))...))
	f.Add(append(append([]byte(nil), data...), protowire.AppendBytes(protowire.AppendTag(nil, segmentCheckpointsField,
		protowire.BytesType), append(append(protowire.AppendString(protowire.AppendTag(nil, refVMField,
		protowire.BytesType), "table"), varint(refSequenceField, 3)...), unknown...))...))
	f.Fuzz(func(t *testing.T, data []byte) {
		want, wantErr := protobufSegment(index, "ram", data)
		got, gotErr := parsePageTable(data)
		if gotErr == nil {
			gotErr = index.checkTable("ram", got)
		}
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("the protobuf decoder says %v, the page table %v", wantErr, gotErr)
		}
		if wantErr == nil && !sameAs(got, want) {
			t.Fatalf("the page table locates %+v, the protobuf decoder %+v", got.mutable().pages, want.pages)
		}
	})
}

// fullSegment is a whole segment of a 4 KiB-page volume as one checkpoint
// publishes it: every one of its 16,384 pages a member of that checkpoint's
// parts, 64 MiB of members to a part, with the index that names the
// checkpoint and its parts.
func fullSegment(tb testing.TB) (*Index, []byte) {
	tb.Helper()
	ref := control.Ref{VM: "table", Sequence: 2}
	index := newIndex(nil, ref)
	index.checkpoints[ref] = checkpointCost{parts: 2}
	index.volumes["ram"] = &volumeTable{size: segmentPages4KiB * PageSize4KiB, geometry: at4KiB,
		segments: map[uint64]segmentEntry{}}
	held := newSegment()
	perPart := uint32(partTargetBytes / (PageSize4KiB + 48))
	for page := range uint32(segmentPages4KiB) {
		held.pages[page] = location{ref: ref, origin: ref, part: page / perPart,
			offset: uint64(page%perPart) * (PageSize4KiB + 48), length: PageSize4KiB + 48}
	}
	data, err := encodeSegment(held)
	if err != nil {
		tb.Fatal(err)
	}
	return index, data
}

// What decoding one whole segment of a 4 KiB-page volume costs a reader, as
// the generated message did and as the page table does.
func BenchmarkDecodeAWholeSegmentOf4KiBPages(b *testing.B) {
	index, data := fullSegment(b)
	b.Run("protobuf", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := protobufSegment(index, "ram", data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("table", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			table, err := parsePageTable(data)
			if err != nil {
				b.Fatal(err)
			}
			if err := index.checkTable("ram", table); err != nil {
				b.Fatal(err)
			}
		}
	})
}
