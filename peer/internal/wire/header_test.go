package wire_test

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"testing/iotest"

	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// pageLikeFrame is a frame with everything a page reply's header holds: ids, a
// message and a payload descriptor with a checksum.
func pageLikeFrame(t *testing.T, version uint32) (platform.Frame, []byte) {
	t.Helper()
	payload := bytes.Repeat([]byte("page"), 1024)
	checksum, err := wire.ComputeChecksum(t.Context(), bytes.NewReader(payload), int64(len(payload)), wire.ChecksumCRC32C)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := wire.Encode(wire.Outgoing{Version: version, RequestID: 7, InReplyTo: 7,
		Message: wrapperspb.Bytes([]byte{0b10110000, 0b00000001}),
		Payload: wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload)),
			Algorithm: wire.ChecksumCRC32C, Checksum: checksum}})
	if err != nil {
		t.Fatal(err)
	}
	return frame, payload
}

func decodeHeader(header, payload []byte) error {
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: header,
		Payload: io.NopCloser(bytes.NewReader(payload)), PayloadSize: int64(len(payload))})
	if err == nil {
		_ = incoming.Payload.Close()
	}
	return err
}

// Every bit of a version 2 header is covered: a frame with any one bit flipped
// in its header is refused as damaged, never read as one that was sent, and
// never as one the peer meant, which would make the peer unusable.
func TestEveryBitFlipOfAVersionTwoHeaderIsCaught(t *testing.T) {
	t.Parallel()
	frame, payload := pageLikeFrame(t, 2)
	if err := decodeHeader(frame.Header, payload); err != nil {
		t.Fatal(err)
	}
	for bit := range len(frame.Header) * 8 {
		flipped := bytes.Clone(frame.Header)
		flipped[bit/8] ^= 1 << (bit % 8)
		if err := decodeHeader(flipped, payload); !errors.Is(err, wire.ErrCorrupt) {
			t.Fatalf("bit %d of %d flipped: Decode = %v, want ErrCorrupt", bit, len(frame.Header)*8, err)
		}
	}
}

// A version 1 header carries the checksum too, and is held to it: every flip
// outside the checksum field's own tag is caught. A flip of that tag leaves a
// header whose other fields are as they were sent, which a release before this
// one wrote anyway: without a checksum.
func TestEveryBitFlipOfAVersionOneHeaderOutsideTheChecksumTagIsCaught(t *testing.T) {
	t.Parallel()
	frame, payload := pageLikeFrame(t, 1)
	tag := len(frame.Header) - 5
	for bit := range len(frame.Header) * 8 {
		if bit/8 == tag {
			continue
		}
		flipped := bytes.Clone(frame.Header)
		flipped[bit/8] ^= 1 << (bit % 8)
		if err := decodeHeader(flipped, payload); !errors.Is(err, wire.ErrCorrupt) {
			t.Fatalf("bit %d of %d flipped: Decode = %v, want ErrCorrupt", bit, len(frame.Header)*8, err)
		}
	}
}

// A version 1 header with no checksum, as the release before this one wrote
// every header, is read.
func TestAVersionOneHeaderWithoutAChecksumIsRead(t *testing.T) {
	t.Parallel()
	frame, payload := pageLikeFrame(t, 1)
	// The checksum is the last five bytes: its tag and its value.
	legacy := frame.Header[:len(frame.Header)-5]
	if err := decodeHeader(legacy, payload); err != nil {
		t.Fatalf("a header without a checksum at version 1: %v", err)
	}
}

// A version 2 header must carry its checksum: one without it is damaged.
func TestAVersionTwoHeaderWithoutAChecksumIsRefused(t *testing.T) {
	t.Parallel()
	frame, payload := pageLikeFrame(t, 2)
	if err := decodeHeader(frame.Header[:len(frame.Header)-5], payload); !errors.Is(err, wire.ErrCorrupt) {
		t.Fatalf("a version 2 header without a checksum: %v, want ErrCorrupt", err)
	}
}

// A payload checksum is optional: a frame whose payload checks itself carries
// none, and its header is still covered.
func TestAPayloadNeedNotCarryAChecksum(t *testing.T) {
	t.Parallel()
	payload := []byte("a stripe checks its own bytes")
	frame, err := wire.Encode(wire.Outgoing{Version: 2, RequestID: 1, Message: wrapperspb.String("stripes"),
		Payload: wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload))}})
	if err != nil {
		t.Fatal(err)
	}
	damaged := bytes.Clone(payload)
	damaged[0] ^= 1
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: frame.Header,
		Payload: io.NopCloser(bytes.NewReader(damaged)), PayloadSize: int64(len(damaged))})
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(incoming.Payload)
	if err != nil || !bytes.Equal(read, damaged) {
		t.Fatalf("an unchecked payload read %q, %v; want the bytes as they arrived", read, err)
	}
	if incoming.Version != 2 {
		t.Fatalf("the frame decoded at version %d, want 2", incoming.Version)
	}
}

// A version this codec does not read is refused as such, at encode and at
// decode.
func TestAVersionPastTheNewestIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := wire.Encode(wire.Outgoing{Version: wire.Newest + 1, Message: wrapperspb.String("x")}); !errors.Is(err, wire.ErrUnsupportedVersion) {
		t.Fatalf("encoding version %d: %v, want ErrUnsupportedVersion", wire.Newest+1, err)
	}
	// A frame of the newest version, renumbered past it with its checksum made
	// good again, is a peer of a later release.
	frame, payload := pageLikeFrame(t, wire.Newest)
	if frame.Header[0] != 1<<3 || frame.Header[1] != byte(wire.Newest) {
		t.Fatalf("the header starts % x, want the wire version first", frame.Header[:2])
	}
	later := bytes.Clone(frame.Header[:len(frame.Header)-5])
	later[1] = byte(wire.Newest + 1)
	later = appendChecksum(later)
	if err := decodeHeader(later, payload); !errors.Is(err, wire.ErrUnsupportedVersion) {
		t.Fatalf("a version %d header: %v, want ErrUnsupportedVersion", wire.Newest+1, err)
	}
}

// appendChecksum appends a header checksum the way the encoder does.
func appendChecksum(header []byte) []byte {
	sum := crc32.Checksum(header, crc32.MakeTable(crc32.Castagnoli))
	return append(append(header, 6<<3|5), byte(sum), byte(sum>>8), byte(sum>>16), byte(sum>>24))
}

// A reader that reads exactly a payload's length can be handed the last bytes
// and the end together, and learns of a mismatch only on the read after: that
// read reports the mismatch, never the end.
func TestAPayloadMismatchIsReportedOnEveryReadAfterIt(t *testing.T) {
	t.Parallel()
	frame, payload := pageLikeFrame(t, 2)
	damaged := bytes.Clone(payload)
	damaged[len(damaged)-1] ^= 1
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: frame.Header,
		Payload: io.NopCloser(iotest.DataErrReader(bytes.NewReader(damaged))), PayloadSize: int64(len(damaged))})
	if err != nil {
		t.Fatal(err)
	}
	read := make([]byte, len(damaged))
	if _, err := io.ReadFull(incoming.Payload, read); err != nil {
		t.Fatalf("reading the payload's length: %v", err)
	}
	for range 2 {
		if n, err := incoming.Payload.Read(make([]byte, 1)); n != 0 || !errors.Is(err, wire.ErrChecksumMismatch) {
			t.Fatalf("the read after the payload = %d, %v; want 0, ErrChecksumMismatch", n, err)
		}
	}
}
