package rank

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"

	"github.com/semistrict/sproutfs/control"
)

// WindowBytes is the span of a volume one window covers.
const WindowBytes = 2 << 20

// Window is the unit of placement: the pages of one volume, in one aligned
// 2 MiB span, that one checkpoint published. At a 2 MiB page it is one
// envelope, and at a 4 KiB page up to 512. A segment of a volume's page table
// is a window of its own. Every envelope of a window is striped on the same
// caches, so a read-ahead run asks the same caches for all its pages.
type Window struct {
	// Ref is the checkpoint that published the window and Volume the volume.
	Ref    control.Ref
	Volume string
	// Segment marks a segment's window, and Number is then the segment's
	// number; otherwise Number is the span's, counted in WindowBytes from the
	// start of the volume.
	Segment bool
	Number  uint64
	// Pages is how many pages of its volume the window spans: one for a
	// segment and at a 2 MiB page, 512 at 4 KiB. It is what turns a page of
	// the window into a page of the volume. It is not part of what ranks the
	// window: a volume has one page size, so no two windows differ in it
	// alone.
	Pages uint32
}

// PageWindow is the window that holds page, a page of pageSize bytes.
func PageWindow(page control.Identity, pageSize uint64) Window {
	span := max(WindowBytes/max(pageSize, 1), 1)
	return Window{Ref: page.Ref, Volume: page.Volume, Number: page.Page / span, Pages: uint32(span)}
}

// SegmentWindow is the window of one segment of volume's page table, which
// the checkpoint ref wrote.
func SegmentWindow(ref control.Ref, volume string, number uint64) Window {
	return Window{Ref: ref, Volume: volume, Segment: true, Number: number, Pages: 1}
}

// Page is the page of the volume that is page at of the window.
func (w Window) Page(at uint32) uint64 { return w.Number*uint64(max(w.Pages, 1)) + uint64(at) }

// digest is the window's 64-bit hash, which every cache's score of it starts
// from. Each field is length-prefixed, so no two windows encode alike.
func (w Window) digest() uint64 {
	encoded := make([]byte, 0, 64+len(w.Ref.VM)+len(w.Volume))
	encoded = append(encoded, "sproutfs rank window\x00"...)
	if w.Segment {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	encoded = binary.AppendUvarint(encoded, uint64(len(w.Ref.VM)))
	encoded = append(encoded, w.Ref.VM...)
	encoded = binary.BigEndian.AppendUint64(encoded, w.Ref.Sequence)
	encoded = binary.AppendUvarint(encoded, uint64(len(w.Volume)))
	encoded = append(encoded, w.Volume...)
	encoded = binary.BigEndian.AppendUint64(encoded, w.Number)
	sum := sha256.Sum256(encoded)
	return binary.BigEndian.Uint64(sum[:8])
}

// shareSalt keeps a window's place in the rollout's share apart from its
// scores, so the windows a share turns on are not the ones some cache ranks
// highest.
const shareSalt = 0x5f3c_9e1d_a7b2_4c68

// InShare reports whether the window is among the percent of windows the
// cluster cache is turned on for: none at 0, every one at 100. The share is a
// hash of the window, so every host puts a window on the same side, and
// raising the share only adds windows to it.
func (w Window) InShare(percent int) bool {
	return mix(w.digest()^shareSalt)%100 < uint64(max(percent, 0))
}

// fractionBits is how many bits of a distance follow its binary point.
const fractionBits = 32

// distance is -log2(u) in fixed point with fractionBits after the point, for u
// the hash h mapped into (0, 1) as (h | 1) / 2^64. It is at least one unit
// and below 64 << fractionBits, so a weight times a distance fits in 128 bits.
//
// It is integer arithmetic, not math.Log, because the standard library's
// logarithm is assembly on some architectures and Go on others, and two hosts
// that rank a window by scores a rounding apart would disagree about whom to
// ask.
func distance(h uint64) uint64 {
	return 64<<fractionBits - log2(h|1)
}

// log2 is log2(m) for m at least 1, truncated to fractionBits after the
// point: the position of the top bit, then the fraction one bit at a time by
// squaring the mantissa, which doubles its logarithm.
func log2(m uint64) uint64 {
	top := uint64(bits.Len64(m) - 1)
	result := top << fractionBits
	// x is m over 2^top, in [1, 2), with 63 bits after the point.
	x := m << (63 - top)
	for bit := uint64(1) << (fractionBits - 1); bit != 0; bit >>= 1 {
		// x² is in [1, 4), with 126 bits after the point across high:low.
		high, low := bits.Mul64(x, x)
		if high >= 1<<63 {
			// x² is at least 2: this bit is one, and x² / 2 carries on.
			result |= bit
			x = high
		} else {
			x = high<<1 | low>>63
		}
	}
	return result
}
