package control

import "strconv"

// Ref names one checkpoint of one VM: the (VM, sequence) pair a record
// allocated and selects. Its zero value names no checkpoint.
type Ref struct {
	// VM is the identity the orchestrator allocated; Sequence is the
	// checkpoint number within it.
	VM       string
	Sequence uint64
}

// IsZero reports whether the reference names no checkpoint.
func (r Ref) IsZero() bool { return r == Ref{} }

// String renders the reference as "<vm>/<sequence>".
func (r Ref) String() string { return r.VM + "/" + strconv.FormatUint(r.Sequence, 10) }

// Identity names the page whose bytes a range reads: the checkpoint whose parts
// holds them, the volume they belong to and the page's number within it. Two
// indexes report equal identities for a page they inherited from a common
// ancestor, so a host can share a resident page between VMs without comparing
// contents, and compaction moving those bytes into another checkpoint's parts does not change
// it. Zero means the range reads as zeroes and has no page.
type Identity struct {
	// Ref, Volume and Page name the page, which is the whole identity a pager
	// keys a resident page by and the whole key the page cache uses.
	Ref    Ref
	Volume string
	Page   uint64
	// Zero marks a range with no page at all, and is the only field set then.
	Zero bool
}

// ZeroIdentity is the identity every absent page reports, so a large hole is
// one extent rather than one per page.
var ZeroIdentity = Identity{Zero: true}

// Extent is a run of consecutive volume bytes whose current contents all live
// in one page. Offset is the volume offset of the run's first byte.
type Extent struct {
	// Offset is the volume offset of the run's first byte and Length its size.
	Offset, Length uint64
	// Identity names the object the whole run reads from.
	Identity Identity
}
