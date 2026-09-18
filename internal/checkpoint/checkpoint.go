// Package checkpoint stores VM checkpoints in shared object storage as one
// index object of metadata and the parts holding the guest bytes.
//
// A checkpoint is named by a [control.Ref], the (VM, sequence) pair the caller
// allocated for it. Its parts hold the VMM state and the pages it changed. Its
// index object holds the segments of page table those pages changed and, last,
// the root: what the checkpoint says about itself, naming per volume where each
// segment is fetched from and carrying forward the entry of the checkpoint it
// inherits for every segment it did not change. A root is therefore complete on
// its own, a checkpoint writes as much table as it changed rather than as much
// as the volume holds, opening one is one get of the index object, and a cold
// page read is one range get for the segment and one for the page.
//
// The index object's create-if-absent PUT is the commit: a checkpoint is
// published exactly while its index object exists.
//
// Nothing is named by, addressed by, or compared by content: two VMs share a
// page because one inherited it from the other, never because their bytes are
// equal.
//
// The package has no volume dependency. It deletes an object only
// through Reclaim, which a VM calls for the checkpoint its newest one replaced.
package checkpoint

import (
	"errors"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
)

const (
	// PageSize is the unit of publication: a page is written whole or not at
	// all, and it is the unit a reader faults in.
	PageSize = 2 << 20
	// SectorSize is the granularity a volume's size is stated in.
	SectorSize = 4096
	// segmentPages is how many of a volume's pages one segment of its page
	// table covers: segment n holds pages [n*segmentPages, (n+1)*segmentPages),
	// which is 512 MiB of volume. It is a constant of the format rather than a
	// knob, because every reader of an index must divide page numbers by the
	// same thing the writer did.
	segmentPages = 256
)

const (
	maximumName = 255
	// maximumRootSize bounds one checkpoint's root, which ends its index object.
	// A root is fifteen bytes per 512 MiB of volume, so this is about 140,000
	// segments: 70 TiB of volume per VM, and the limit this design states rather
	// than a number nothing could reach. MaxIndexBytes bounds what one
	// publication may write, never what a reader accepts.
	maximumRootSize = 2 << 20
	// maximumSegmentSize bounds one segment, which holds at most segmentPages
	// entries of about a dozen bytes each.
	maximumSegmentSize = 1 << 20
	// maximumRootExtent and maximumSegmentExtent bound what a root addresses
	// inside an index object, which is an envelope rather than the bytes inside
	// it: an envelope that falls back to raw is its contents plus the header.
	maximumRootExtent    = maximumRootSize + blob.HeaderSize
	maximumSegmentExtent = maximumSegmentSize + blob.HeaderSize
	// maximumIndexSize bounds one checkpoint's whole index object: its header,
	// the segments it changed and its root. A fully dirty 4 TiB volume writes
	// 8192 segments of a few kilobytes each and does not reach this.
	maximumIndexSize = 64 << 20
	// maximumTableSize bounds one part's table, which names the part's members.
	// It bounds what a writer produces as well as what a reader accepts: a part
	// is sealed when the next member's entry would carry its table past this, so
	// a reader holds a part's whole table after one suffix read of this many
	// bytes plus the trailer. It admits a few thousand members of the longest
	// kind, which is far more than a part of partTargetBytes holds.
	maximumTableSize = 256 << 10
	// maximumStateSize bounds one VMM state member.
	maximumStateSize = 64 << 20
	// partTargetBytes is the encoded member size a part fills to before it is
	// sealed and uploaded, so one checkpoint costs a few PUTs and bounded memory.
	partTargetBytes = 64 << 20
	// maximumPartSize bounds one part: its members, its table and its trailer. A
	// part is sealed once it holds partTargetBytes of members and the member
	// that filled it is admitted whole, so the slack over the target is one
	// page's envelope, the table and the trailer.
	maximumPartSize = partTargetBytes + PageSize + blob.HeaderSize +
		maximumTableSize + part.TrailerSize
	// compactionBudget is the live bytes one checkpoint rewrites out of the
	// parts of checkpoints that have become mostly dead.
	compactionBudget = 64 << 20
)

var (
	// ErrInvalidConfig reports a configuration or argument that cannot name a
	// checkpoint: an empty VM identity, an unusable volume name, or a size that
	// is not a whole number of sectors.
	ErrInvalidConfig = errors.New("checkpoint: invalid configuration")
	// ErrInvalidRange reports a read or locate outside a volume's size.
	ErrInvalidRange = errors.New("checkpoint: invalid range")
	// ErrUnknownVolume reports a volume the index does not describe.
	ErrUnknownVolume = errors.New("checkpoint: unknown volume")
	// ErrCorrupt reports an object that disagrees with the index that named it,
	// or whose bounded compression envelope fails integrity verification.
	ErrCorrupt = errors.New("checkpoint: corrupt object")
	// ErrConflict reports an object already present under a key this
	// publication wanted to write, with different logical bytes. Retrying the
	// same Ref is idempotent; reusing a Ref for different contents is not.
	ErrConflict = errors.New("checkpoint: conflicting object under a reference")
	// ErrNoState reports ReadState on a checkpoint published without VMM state.
	ErrNoState = errors.New("checkpoint: no VMM state")
)

// validName reports whether a VM identity or volume name can be part of an
// object key. Names are opaque to this package but must not restructure a key.
func validName(name string) bool {
	if name == "" || len(name) > maximumName || name == "." || name == ".." {
		return false
	}
	for index := 0; index < len(name); index++ {
		if c := name[index]; c == '/' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// pageSpan reports the byte offset and length of a page within a volume of
// the given size. The length is zero for a page beyond the end.
func pageSpan(size, page uint64) (uint64, uint64) {
	start := page * PageSize
	if start >= size {
		return start, 0
	}
	return start, min(uint64(PageSize), size-start)
}

// ProbeCompactionRewrite marks a publication that rewrote the live pages out of
// the parts of checkpoints that had become mostly dead. It is the reclamation
// path a workload only reaches once enough of an old checkpoint has been
// overwritten.
const ProbeCompactionRewrite = "checkpoint/compaction-rewrite"
