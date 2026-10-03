// Package wire encodes typed protobuf control messages and keeps optional bulk
// data in a separate raw payload frame.
//
// Every header this package encodes ends in a CRC32C of the bytes in front of
// it, so a header damaged on the way is never read as the one that was sent. A
// payload's checksum is optional: a payload whose own format is checked, as a
// cache's stripes are, goes without one.
package wire

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	wirev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/wire/v1"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// The wire versions this codec reads. Version 1 is what the release before
// this one wrote, which may carry a header checksum and need not. Version 2
// must carry one.
const (
	Oldest uint32 = 1
	Newest uint32 = 2
)

var (
	ErrChecksumMismatch   = errors.New("payload checksum mismatch")
	ErrMalformedFrame     = errors.New("malformed frame")
	ErrUnsupportedVersion = errors.New("unsupported wire version")
	// ErrCorrupt reports a header that does not parse or whose checksum does
	// not match its bytes. It is not a malformed frame: what the peer sent was
	// damaged on the way, so the connection is dropped and what it carried is
	// asked for again, as on a connection that broke. Nothing in a damaged
	// header can be trusted, not even which request it answers.
	ErrCorrupt = errors.New("frame header damaged")
)

// headerTrailer is the encoded tag of Envelope.header_checksum — field 6, wire
// type fixed32 — followed by its four bytes. The encoder appends it last, so
// the checksum covers every byte in front of it.
const (
	headerChecksumTag = 6<<3 | 5
	headerTrailer     = 5
)

var headerTable = crc32.MakeTable(crc32.Castagnoli)

type ChecksumAlgorithm uint8

const (
	ChecksumNone ChecksumAlgorithm = iota
	ChecksumCRC32C
	ChecksumSHA256
)

// checksums describes every supported algorithm: its digest length, its wire
// enum, and how to build its hasher (nil for ChecksumNone). An index outside
// this table is a malformed algorithm.
var checksums = [...]struct {
	size   int
	proto  wirev1.ChecksumAlgorithm
	hasher func() hash.Hash
}{
	ChecksumNone:   {0, wirev1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED, nil},
	ChecksumCRC32C: {4, wirev1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_CRC32C, func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }},
	ChecksumSHA256: {sha256.Size, wirev1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256, sha256.New},
}

type Payload struct {
	Body      io.ReaderAt
	Size      int64
	Algorithm ChecksumAlgorithm
	Checksum  []byte
}

func (p Payload) Validate() error {
	if p.Size < 0 || (p.Size > 0 && p.Body == nil) {
		return platform.ErrInvalidRange
	}
	return validateChecksum(p.Algorithm, p.Checksum)
}

func validateChecksum(algorithm ChecksumAlgorithm, checksum []byte) error {
	if int(algorithm) >= len(checksums) || len(checksum) != checksums[algorithm].size {
		return ErrMalformedFrame
	}
	return nil
}

type Outgoing struct {
	// Version is the wire version to encode at. Zero is Oldest.
	Version   uint32
	RequestID uint64
	InReplyTo uint64
	Message   proto.Message
	Payload   Payload
}

func Encode(outgoing Outgoing) (platform.Frame, error) {
	if outgoing.Message == nil {
		return platform.Frame{}, ErrMalformedFrame
	}
	if err := outgoing.Payload.Validate(); err != nil {
		return platform.Frame{}, err
	}
	version := outgoing.Version
	if version == 0 {
		version = Oldest
	}
	if version > Newest {
		return platform.Frame{}, ErrUnsupportedVersion
	}
	message, err := anypb.New(outgoing.Message)
	if err != nil {
		return platform.Frame{}, err
	}
	builder := wirev1.Envelope_builder{
		WireVersion: proto.Uint32(version),
		RequestId:   proto.Uint64(outgoing.RequestID),
		InReplyTo:   proto.Uint64(outgoing.InReplyTo),
		Message:     message,
	}
	if outgoing.Payload.Size > 0 || outgoing.Payload.Algorithm != ChecksumNone {
		algorithm := checksums[outgoing.Payload.Algorithm].proto
		builder.Payload = wirev1.PayloadDescriptor_builder{
			Length:            proto.Uint64(uint64(outgoing.Payload.Size)),
			ChecksumAlgorithm: &algorithm,
			Checksum:          append([]byte(nil), outgoing.Payload.Checksum...),
		}.Build()
	}
	envelope := builder.Build()
	header, err := proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
	if err != nil {
		return platform.Frame{}, err
	}
	header = appendHeaderChecksum(header)
	return platform.Frame{
		Header:      header,
		Payload:     outgoing.Payload.Body,
		PayloadSize: outgoing.Payload.Size,
	}, nil
}

// appendHeaderChecksum appends Envelope.header_checksum over header, as the
// last field of the envelope.
func appendHeaderChecksum(header []byte) []byte {
	sum := crc32.Checksum(header, headerTable)
	return binary.LittleEndian.AppendUint32(append(header, headerChecksumTag), sum)
}

// checkHeader requires an envelope's header checksum to be the last field of
// header and to match every byte before it. An envelope without one is
// accepted only at a version that did not require it.
func checkHeader(header []byte, envelope *wirev1.Envelope) error {
	if !envelope.HasHeaderChecksum() {
		if envelope.GetWireVersion() >= 2 {
			return fmt.Errorf("%w: a version %d header carries no checksum", ErrCorrupt, envelope.GetWireVersion())
		}
		return nil
	}
	body := len(header) - headerTrailer
	if body < 0 || header[body] != headerChecksumTag ||
		binary.LittleEndian.Uint32(header[body+1:]) != envelope.GetHeaderChecksum() ||
		crc32.Checksum(header[:body], headerTable) != envelope.GetHeaderChecksum() {
		return ErrCorrupt
	}
	return nil
}

type Incoming struct {
	// Version is the wire version the frame was encoded at.
	Version     uint32
	RequestID   uint64
	InReplyTo   uint64
	Message     *anypb.Any
	Payload     io.ReadCloser
	PayloadSize int64
	// Checksummed says the payload carried a checksum, which its reader checks.
	Checksummed bool
}

func Decode(frame platform.ReceivedFrame) (Incoming, error) {
	if frame.Payload == nil || frame.PayloadSize < 0 {
		return Incoming{}, ErrMalformedFrame
	}
	envelope := new(wirev1.Envelope)
	if err := proto.Unmarshal(frame.Header, envelope); err != nil {
		_ = frame.Payload.Close()
		// A header that does not parse is one that was damaged on the way far
		// more often than one a peer meant: asking again costs a connection,
		// and calling the peer unusable costs whatever was waiting on it.
		return Incoming{}, errors.Join(ErrCorrupt, err)
	}
	if err := checkHeader(frame.Header, envelope); err != nil {
		_ = frame.Payload.Close()
		return Incoming{}, err
	}
	if version := envelope.GetWireVersion(); !envelope.HasWireVersion() || version < Oldest || version > Newest {
		_ = frame.Payload.Close()
		return Incoming{}, ErrUnsupportedVersion
	}
	if !envelope.HasRequestId() || !envelope.HasInReplyTo() || envelope.GetMessage() == nil {
		_ = frame.Payload.Close()
		return Incoming{}, ErrMalformedFrame
	}
	// The header says what the sender meant. A frame whose prefix disagrees
	// with a header that carried a checksum, and passed it, had its prefix
	// damaged on the way, which asking again repairs.
	mismatch := ErrMalformedFrame
	if envelope.HasHeaderChecksum() {
		mismatch = ErrCorrupt
	}
	descriptor := envelope.GetPayload()
	if descriptor == nil {
		if frame.PayloadSize != 0 {
			_ = frame.Payload.Close()
			return Incoming{}, mismatch
		}
		return Incoming{
			Version:   envelope.GetWireVersion(),
			RequestID: envelope.GetRequestId(),
			InReplyTo: envelope.GetInReplyTo(),
			Message:   envelope.GetMessage(),
			Payload:   frame.Payload,
		}, nil
	}
	if !descriptor.HasLength() || !descriptor.HasChecksumAlgorithm() {
		_ = frame.Payload.Close()
		return Incoming{}, ErrMalformedFrame
	}
	if descriptor.GetLength() != uint64(frame.PayloadSize) {
		_ = frame.Payload.Close()
		return Incoming{}, mismatch
	}
	algorithm, err := fromProtoAlgorithm(descriptor.GetChecksumAlgorithm())
	if err != nil {
		_ = frame.Payload.Close()
		return Incoming{}, err
	}
	if err := validateChecksum(algorithm, descriptor.GetChecksum()); err != nil {
		_ = frame.Payload.Close()
		return Incoming{}, err
	}
	body := frame.Payload
	if algorithm != ChecksumNone {
		body = newVerifyingReader(frame.Payload, algorithm, descriptor.GetChecksum())
	}
	return Incoming{
		Version:     envelope.GetWireVersion(),
		RequestID:   envelope.GetRequestId(),
		InReplyTo:   envelope.GetInReplyTo(),
		Message:     envelope.GetMessage(),
		Payload:     body,
		PayloadSize: frame.PayloadSize,
		Checksummed: algorithm != ChecksumNone,
	}, nil
}

func (i Incoming) UnmarshalTo(message proto.Message) error {
	if message == nil || i.Message == nil {
		return ErrMalformedFrame
	}
	return i.Message.UnmarshalTo(message)
}

// computeChecksum digests a payload a reader streams, which is what a sender
// that cannot hold the payload in memory would need. Nothing outside this
// package's own tests calls it: the peer server computes its CRC over the
// buffer it already holds.
func computeChecksum(ctx context.Context, reader io.ReaderAt, size int64, algorithm ChecksumAlgorithm) ([]byte, error) {
	if size < 0 || (size > 0 && reader == nil) {
		return nil, platform.ErrInvalidRange
	}
	hasher, err := newHasher(algorithm)
	if err != nil {
		return nil, err
	}
	if hasher == nil {
		return nil, nil
	}
	section := io.NewSectionReader(reader, 0, size)
	buffer := make([]byte, 256<<10)
	for {
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		default:
		}
		n, readErr := section.Read(buffer)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return hasher.Sum(nil), nil
}

func fromProtoAlgorithm(algorithm wirev1.ChecksumAlgorithm) (ChecksumAlgorithm, error) {
	for value := range checksums {
		if checksums[value].proto == algorithm {
			return ChecksumAlgorithm(value), nil
		}
	}
	return ChecksumNone, ErrMalformedFrame
}

func newHasher(algorithm ChecksumAlgorithm) (hash.Hash, error) {
	if int(algorithm) >= len(checksums) {
		return nil, ErrMalformedFrame
	}
	if checksums[algorithm].hasher == nil {
		return nil, nil
	}
	return checksums[algorithm].hasher(), nil
}

type verifyingReader struct {
	reader   io.ReadCloser
	hasher   hash.Hash
	expected []byte
	verified bool
	// failed is the mismatch once found, which every later read reports too:
	// a reader that read exactly the payload's length may only learn of it on
	// the read after, and must not be told EOF there.
	failed error
}

func newVerifyingReader(reader io.ReadCloser, algorithm ChecksumAlgorithm, expected []byte) io.ReadCloser {
	hasher, _ := newHasher(algorithm)
	return &verifyingReader{reader: reader, hasher: hasher, expected: append([]byte(nil), expected...)}
}

func (r *verifyingReader) Read(destination []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	n, err := r.reader.Read(destination)
	if n > 0 {
		_, _ = r.hasher.Write(destination[:n])
	}
	if err == io.EOF && !r.verified {
		r.verified = true
		if !bytesEqual(r.hasher.Sum(nil), r.expected) {
			r.failed = ErrChecksumMismatch
			return n, r.failed
		}
	}
	return n, err
}

func (r *verifyingReader) Close() error { return r.reader.Close() }

func bytesEqual(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

// EncodeCRC32C converts a CRC32C value to the canonical network byte order
// used by Payload.Checksum.
func EncodeCRC32C(value uint32) []byte {
	encoded := make([]byte, 4)
	binary.BigEndian.PutUint32(encoded, value)
	return encoded
}
