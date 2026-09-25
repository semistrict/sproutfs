// Package vmwire carries the host side of the managed-memory control protocol
// described in docs/vm-memory.md: fixed 56-byte little-endian frames over a
// Unix socket, SCM_RIGHTS descriptor passing, sealed memfd arenas, and the
// userfaultfd ioctls that resolve one fault. It is shared by the production
// host in vmmemory and by the Go pager fixture that qualifies the Rust client.
//
// This file is the half that is only arithmetic: the frames, the version, and
// the geometry a session states at setup. It builds everywhere, so the encoding
// and the geometry checks both ends make are tested on any machine; the
// syscalls are in vmwire_linux.go.
package vmwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"

	"github.com/semistrict/sproutfs/internal/checkpoint"
)

// Frame kinds. These control messages are separate from Linux's native
// 32-byte UFFD events.
const (
	Hello        = 1
	MemoryRegion = 2
	Attach       = 3
	MapRange     = 4
	Revoke       = 5
	Ack          = 6
	Stop         = 7
	// Seal asks the host to take the session's checkpoint: it write-protects the
	// dirty set and answers, without moving a byte.
	Seal = 8
	// Result answers a Seal or a Flush, echoing its request ID, with Flags zero
	// or a positive Linux errno.
	Result   = 9
	MapBatch = 10
	Ready    = 11
	MapZero  = 12
	// Flush asks the host to make durable a flush the guest made of the
	// session's memory region, under a request ID from the same sequence as Seal's and
	// with every other field zero. The host answers with Result once the flush
	// is durable, which may take a disk checkpoint first, and the device
	// completes the guest's flush then.
	Flush = 13
)

// KindName is the one word a frame's kind goes by in a log line or an error,
// so that a session ending on a command says what the client was asked to do
// rather than a number. An unrecognized kind reports itself.
func KindName(kind uint64) string {
	switch kind {
	case Hello:
		return "hello"
	case MemoryRegion:
		return "memory_region"
	case Attach:
		return "attach"
	case MapRange:
		return "map"
	case Revoke:
		return "revoke"
	case Ack:
		return "ack"
	case Stop:
		return "stop"
	case Seal:
		return "seal"
	case Result:
		return "result"
	case MapBatch:
		return "batch"
	case Ready:
		return "ready"
	case MapZero:
		return "zero"
	case Flush:
		return "flush"
	}
	return strconv.FormatUint(kind, 10)
}

const (
	// Version 9 added FLUSH, the request a client sends when its guest flushes
	// the memory region and whose RESULT completes that flush. A version 8 pager ends a
	// session on a control message it does not know, which would end the guest
	// at its first flush, so the two are told apart by the version before a
	// guest runs.
	//
	// Version 8 made ATTACH's length the arena's offset space. An arena's
	// offsets are not its pages: it is a sparse file, and a RAM pager that puts
	// a private page at the offset it has within its 2 MiB range owns 512
	// consecutive offsets per range whatever memory it holds there. The number
	// the client checks the descriptor against, and bounds a MAP's arena offset
	// by, is therefore the addresses and not the capacity. A version 7 peer
	// would read the same number as a promise of that much memory.
	//
	// Version 7 moved ATTACH in front of MEMORY_REGION and gave it the geometry: a
	// session states the page of the memory region it carries and the kind of memory
	// its arena is made of, the client builds its memory region at that page's
	// alignment and checks the arena against both, and the pager checks the
	// memory region it is then given against the page it runs. Version 6 is refused by
	// version too, because a 2 MiB page number read as a 4 KiB one names another
	// page.
	Version      = 9
	MaxBatchRuns = 1024
)

// The kinds of memory an arena is made of. A session states which its arena is,
// beside the page of its memory region, so that neither end has to infer one from the
// other: the pager sends what it made, and the client checks the descriptor it
// was given against what was claimed before it maps a byte of it.
const (
	// BackingHugeTLB is an explicit 2 MiB HugeTLB memfd out of the host's
	// provisioned pool, which is what a PMEM arena is.
	BackingHugeTLB = 1
	// BackingMemfd is an ordinary shared memfd over the pod's own memory, which
	// is what a RAM arena is: its pages are replaceable at 4 KiB.
	BackingMemfd = 2
)

// BackingFor reports the arena an instance of the given page must be made of,
// and refuses a page this transport does not map. The two are not independent:
// a 2 MiB page is a HugeTLB page of the pool, and a 4 KiB page is ordinary
// memory, so a session that stated one and attached the other is refused before
// anything is mapped.
func BackingFor(pageSize uint64) (uint64, error) {
	switch pageSize {
	case checkpoint.PageSize2MiB:
		return BackingHugeTLB, nil
	case checkpoint.PageSize4KiB:
		return BackingMemfd, nil
	}
	return 0, fmt.Errorf("this transport maps %d-byte and %d-byte pages, not %d",
		checkpoint.PageSize4KiB, checkpoint.PageSize2MiB, pageSize)
}

// AttachFrame is the pager's half of the geometry: the page of the memory region this
// session carries, the offset space of the arena it is attaching — the
// addresses the file has, which is what the descriptor's size is and what
// bounds a MAP's arena offset, not the memory behind them — what that arena is
// made of, and the mapping-count budget the client admits its replacements
// against. It carries the arena descriptor.
func AttachFrame(pageSize, arenaOffsetBytes, backingKind, vmaBudget uint64) Frame {
	return Frame{Kind: Attach, ID: Version, Offset: pageSize, Length: arenaOffsetBytes,
		Backing: backingKind, Flags: vmaBudget}
}

// CheckAttach is the client's half: it reports why an ATTACH frame's geometry
// cannot be served, before the client has built a memory region or mapped the arena.
// The descriptor itself is checked by the client against the backing kind this
// returns, because only the client can stat it.
func CheckAttach(f Frame) error {
	if f.Kind != Attach || f.ID != Version || f.Generation != 0 {
		return fmt.Errorf("invalid managed-memory attachment: kind %d version %d", f.Kind, f.ID)
	}
	backing, err := BackingFor(f.Offset)
	if err != nil {
		return fmt.Errorf("invalid managed-memory attachment: %w", err)
	}
	if f.Backing != backing {
		return fmt.Errorf("invalid managed-memory attachment: a %d-byte page is arena kind %d, not %d",
			f.Offset, backing, f.Backing)
	}
	if f.Length == 0 || f.Length%f.Offset != 0 {
		return fmt.Errorf("invalid managed-memory attachment: an arena of %d bytes is not whole %d-byte pages",
			f.Length, f.Offset)
	}
	return nil
}

// FrameBytes is the encoded size of one control message.
const FrameBytes = 56

// Frame is one 56-byte control message. A session carries exactly one memory region,
// so no frame names one.
type Frame struct{ Kind, ID, Offset, Length, Backing, Generation, Flags uint64 }

// Bytes encodes the frame as seven little-endian words.
func (f Frame) Bytes() []byte {
	b := make([]byte, FrameBytes)
	for i, v := range []uint64{f.Kind, f.ID, f.Offset, f.Length, f.Backing, f.Generation, f.Flags} {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}

// Decode reads a frame out of exactly FrameBytes encoded bytes.
func Decode(b []byte) Frame {
	var v [7]uint64
	for i := range v {
		v[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return Frame{v[0], v[1], v[2], v[3], v[4], v[5], v[6]}
}

// Read consumes one whole frame, returning the zero frame on any error.
func Read(r io.Reader) (Frame, error) {
	var b [FrameBytes]byte
	_, err := io.ReadFull(r, b[:])
	if err != nil {
		return Frame{}, err
	}
	return Decode(b[:]), nil
}

// WriteBytes writes b in full, tolerating short writes.
func WriteBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// Write sends one encoded frame.
func Write(w io.Writer, f Frame) error { return WriteBytes(w, f.Bytes()) }
