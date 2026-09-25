// Package part lays out one checkpoint's parts, which are its data: the
// VMM state and the guest pages it published.
//
// A part is a concatenation of encoded members followed by a table naming them
// and a fixed trailer naming the table, so a part describes itself whether or
// not the root naming its members is at hand. Normal reads never touch the
// table: the root's segments carry the same offsets.
//
// The package is the layout alone. It does not know what a part is stored
// under, how large one is allowed to be, or which volumes exist: the store
// decides all three and reads the bytes this package writes.
package part

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform/sim"

	checkpointv1 "github.com/semistrict/sproutfs/checkpoint/internal/gen/sproutfs/checkpoint/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// ErrCorrupt reports a part that disagrees with itself: a tail that is not a
// trailer, a table that does not parse, or a member outside the bytes the part
// holds.
var ErrCorrupt = errors.New("checkpoint: corrupt part")

const (
	// TrailerSize is the fixed tail of every part: the table's offset and
	// length, the layout version, the checkpoint's part count, then the magic
	// that says the tail is a trailer at all.
	//
	// The version and the magic sit at fixed distances from the end of the part
	// — sixteen bytes and eight — and every layout has put them there, so a part
	// of a layout whose trailer was a different size is still refused by the
	// version it names rather than as a tail that is not a trailer.
	TrailerSize = 32
	// FormatVersion is the part layout this package writes and the only one it
	// reads. It is in the trailer as well as the table so that a part written
	// under another layout is refused before anything of it is parsed.
	FormatVersion = 4
	// trailerMagic is "SPROUTPA".
	trailerMagic = 0x5350524f55545041
)

// Trailer is what a part's fixed tail says about it: where its table is, and how
// many parts the checkpoint has. The count is set in the last part of a
// checkpoint alone; a zero count in a part a reader reached by number is what
// tells it the checkpoint goes on.
type Trailer struct {
	TableOffset uint64
	TableLength uint64
	Parts       uint32
}

// Member is one entry of a part's table: what the member holds, and the extent
// of its encoded bytes within the part. A part holds members of two kinds, a
// page of a volume and the VMM state; the state member sets State and leaves
// Volume empty.
//
// OriginVM and OriginSequence name the checkpoint the page was first published
// under, and are set only for a member compaction moved out of that
// checkpoint's parts. An empty OriginVM means the checkpoint holding the member
// is the one that published it, which is every member a publication writes
// itself.
type Member struct {
	Volume         string
	Page           uint64
	Offset         uint64
	Length         uint64
	State          bool
	OriginVM       string
	OriginSequence uint64
}

// Builder accumulates the members of one part. The body grows with what is
// written into it and is never larger than the part it is building, so a
// checkpoint of one page costs one page rather than a whole part.
type Builder struct {
	codecs  *blob.Codecs
	body    []byte
	members []Member
	// entries is what the members added so far cost in the encoded table. It is
	// carried along rather than measured at Seal because it is what says when
	// the part is full: a reader fetches a part's table as one bounded suffix,
	// so the table is a size the writer must respect.
	entries int
}

// NewBuilder returns a builder that encodes its members through codecs, which
// is the pool the host sized for publication. Nil, and the zero Builder, take
// the package-wide one.
func NewBuilder(codecs *blob.Codecs) *Builder { return &Builder{codecs: codecs} }

func (b *Builder) codec() *blob.Codecs {
	if b.codecs == nil {
		return blob.Default()
	}
	return b.codecs
}

// memberSlack is what one member may take beyond the bytes it holds: its
// envelope, and what the encoder writes before it falls back to raw. Room for
// it is made before the member is encoded, so the encoder appends into the body
// rather than growing it a block at a time.
const memberSlack = blob.HeaderSize + 64<<10

// Add encodes one member into the part's body and reports the extent it landed
// at. The caller describes what the member holds; its extent is this package's
// to fill in. The envelope is written straight into the body, so a member costs
// no allocation of its own and no copy out of one.
func (b *Builder) Add(ctx context.Context, member Member, data []byte) (offset, length uint64, err error) {
	start := uint64(len(b.body))
	body, err := b.codec().AppendEncode(ctx, slices.Grow(b.body, len(data)+memberSlack), data)
	if err != nil {
		return 0, 0, err
	}
	b.body = body
	if sim.Bug(ctx, "checkpoint-part-member-offset") {
		// Every member is located at the start of its part, so a read of one
		// returns whichever page the part begins with.
		start = 0
	}
	member.Offset, member.Length = start, uint64(len(b.body))-start
	b.hold(member)
	return member.Offset, member.Length, nil
}

// hold takes one member into the part and charges its entry against the table.
func (b *Builder) hold(member Member) {
	b.members = append(b.members, member)
	b.entries += EntryBytes(member)
}

// TableBytes is what this part's table would encode to if it were sealed now.
func (b *Builder) TableBytes() int { return emptyTableBytes + b.entries }

// Members reports how many members this part holds, which is what says whether
// there is anything to seal at all.
func (b *Builder) Members() int { return len(b.members) }

// Full reports whether this part must be sealed before next is added: its body
// has reached bodyTarget, or next's entry would carry the table past
// tableLimit, which is the tail a reader fetches a table in one read of. A
// checkpoint of many small members is bounded by the table rather than by the
// body, which is what keeps that tail one read whatever a publication writes.
func (b *Builder) Full(next Member, bodyTarget, tableLimit int) bool {
	// The member has not been written yet, so its extent is unknown here and is
	// charged at its widest: the part is sealed a few bytes early rather than
	// one entry late.
	next.Offset, next.Length = math.MaxUint64, math.MaxUint64
	return len(b.body) >= bodyTarget || b.TableBytes()+EntryBytes(next) > tableLimit
}

// emptyTableBytes is what a part's table costs before any member: the layout
// version every table carries.
var emptyTableBytes = proto.Size(checkpointv1.PartTable_builder{
	FormatVersion: proto.Uint32(FormatVersion)}.Build())

// EntryBytes is what one member costs in the encoded table: the repeated
// field's tag and length prefix, and the entry itself. Every field of an entry
// is written whether or not it is zero, so what an entry costs depends on how
// long its names are and how large its numbers. It is exported because the
// store sizes the bound it seals a part's table at from it: a part must fill to
// its target on the bytes it holds rather than stop short on the entries
// naming them.
func EntryBytes(member Member) int {
	size := proto.Size(tableEntry(member))
	return protowire.SizeTag(membersField) + protowire.SizeBytes(size)
}

// membersField is the field number the table lists its members under.
const membersField = 1

// tableEntry is one member as the table carries it.
func tableEntry(member Member) *checkpointv1.Member {
	return checkpointv1.Member_builder{
		Volume: proto.String(member.Volume), Page: proto.Uint64(member.Page),
		Offset: proto.Uint64(member.Offset), Length: proto.Uint64(member.Length),
		State: proto.Bool(member.State), OriginVm: proto.String(member.OriginVM),
		OriginSequence: proto.Uint64(member.OriginSequence),
	}.Build()
}

// Seal appends the table and the trailer, returning the finished part. The
// table lists members in the order they were written, which is the order the
// publication chose, so a retry produces identical bytes.
//
// parts is how many parts the whole checkpoint has, and is written only into
// its last part; an earlier part passes zero.
func (b *Builder) Seal(parts uint32) ([]byte, error) {
	entries := make([]*checkpointv1.Member, 0, len(b.members))
	for _, item := range b.members {
		entries = append(entries, tableEntry(item))
	}
	table, err := proto.MarshalOptions{Deterministic: true}.Marshal(checkpointv1.PartTable_builder{
		Members: entries, FormatVersion: proto.Uint32(FormatVersion)}.Build())
	if err != nil {
		return nil, err
	}
	offset := uint64(len(b.body))
	sealed := append(b.body, table...)
	trailer := make([]byte, TrailerSize)
	binary.LittleEndian.PutUint64(trailer[0:], offset)
	binary.LittleEndian.PutUint64(trailer[8:], uint64(len(table)))
	binary.LittleEndian.PutUint32(trailer[16:], FormatVersion)
	binary.LittleEndian.PutUint32(trailer[20:], parts)
	binary.LittleEndian.PutUint64(trailer[24:], trailerMagic)
	return append(sealed, trailer...), nil
}

// DecodeTrailer reads a part's trailer, which is its last TrailerSize bytes,
// and reports where its table is and how many parts the checkpoint has. The
// count is set in its last part alone, which is what tells a reader whether it
// has reached the end.
func DecodeTrailer(trailer []byte, size uint64) (Trailer, error) {
	if len(trailer) != TrailerSize || binary.LittleEndian.Uint64(trailer[24:]) != trailerMagic {
		return Trailer{}, ErrCorrupt
	}
	// A part written under another layout is refused before anything of it is
	// parsed, and the refusal names the layout it was written under. The version
	// is sixteen bytes from the end whatever the layout, so a part whose trailer
	// was longer than this one names itself here too.
	if version := binary.LittleEndian.Uint32(trailer[16:]); version != FormatVersion {
		return Trailer{}, fmt.Errorf("%w: part format version %d, want %d", ErrCorrupt, version, FormatVersion)
	}
	found := Trailer{
		TableOffset: binary.LittleEndian.Uint64(trailer[0:]),
		TableLength: binary.LittleEndian.Uint64(trailer[8:]),
		Parts:       binary.LittleEndian.Uint32(trailer[20:]),
	}
	if size < TrailerSize || found.TableOffset > size-TrailerSize ||
		found.TableLength > size-TrailerSize-found.TableOffset {
		return Trailer{}, ErrCorrupt
	}
	return found, nil
}

// TrailerVersion reports the layout version a part's trailer names, and whether
// the bytes are a trailer at all. It is what a reader that found a part where
// this build expects none uses to say which layout wrote the store, and it is
// the one reader that accepts a version this package does not implement.
func TrailerVersion(trailer []byte) (uint32, bool) {
	if len(trailer) < TrailerSize {
		return 0, false
	}
	trailer = trailer[len(trailer)-TrailerSize:]
	if binary.LittleEndian.Uint64(trailer[24:]) != trailerMagic {
		return 0, false
	}
	return binary.LittleEndian.Uint32(trailer[16:]), true
}

// DecodeTable parses a part's table and checks every member against the part's
// own bounds, so what a part says it holds is bytes it actually holds. The body
// is where the table starts, which is one past the last member byte. Whether a
// volume named here is one the checkpoint has is the store's to say.
func DecodeTable(data []byte, body uint64) ([]Member, error) {
	message := new(checkpointv1.PartTable)
	if err := proto.Unmarshal(data, message); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if message.GetFormatVersion() != FormatVersion {
		return nil, fmt.Errorf("%w: part table format version %d, want %d",
			ErrCorrupt, message.GetFormatVersion(), FormatVersion)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrCorrupt
	}
	members := make([]Member, 0, len(message.GetMembers()))
	for _, entry := range message.GetMembers() {
		item := Member{Volume: entry.GetVolume(), Page: entry.GetPage(),
			Offset: entry.GetOffset(), Length: entry.GetLength(), State: entry.GetState(),
			OriginVM: entry.GetOriginVm(), OriginSequence: entry.GetOriginSequence()}
		if item.Length == 0 || item.Offset > body || item.Length > body-item.Offset {
			return nil, ErrCorrupt
		}
		// The VMM state is the one member that names no volume, and a page is
		// one that does.
		if item.State != (item.Volume == "") {
			return nil, ErrCorrupt
		}
		if item.State && item.Page != 0 {
			return nil, ErrCorrupt
		}
		if (item.OriginVM == "") != (item.OriginSequence == 0) {
			return nil, ErrCorrupt
		}
		members = append(members, item)
	}
	return members, nil
}
