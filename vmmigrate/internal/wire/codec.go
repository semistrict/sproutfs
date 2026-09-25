// Package wire encodes typed protobuf control messages and keeps optional bulk
// bulk data in a separate raw payload frame.
package wire

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"

	"github.com/semistrict/sproutfs/platform"
	wirev1 "github.com/semistrict/sproutfs/vmmigrate/internal/gen/sproutfs/wire/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const Version uint32 = 1

var (
	ErrChecksumMismatch   = errors.New("payload checksum mismatch")
	ErrMalformedFrame     = errors.New("malformed frame")
	ErrUnsupportedVersion = errors.New("unsupported wire version")
)

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
	message, err := anypb.New(outgoing.Message)
	if err != nil {
		return platform.Frame{}, err
	}
	builder := wirev1.Envelope_builder{
		WireVersion: proto.Uint32(Version),
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
	return platform.Frame{
		Header:      header,
		Payload:     outgoing.Payload.Body,
		PayloadSize: outgoing.Payload.Size,
	}, nil
}

type Incoming struct {
	RequestID   uint64
	InReplyTo   uint64
	Message     *anypb.Any
	Payload     io.ReadCloser
	PayloadSize int64
}

func Decode(frame platform.ReceivedFrame) (Incoming, error) {
	if frame.Payload == nil || frame.PayloadSize < 0 {
		return Incoming{}, ErrMalformedFrame
	}
	envelope := new(wirev1.Envelope)
	if err := proto.Unmarshal(frame.Header, envelope); err != nil {
		_ = frame.Payload.Close()
		return Incoming{}, errors.Join(ErrMalformedFrame, err)
	}
	if !envelope.HasWireVersion() || envelope.GetWireVersion() != Version {
		_ = frame.Payload.Close()
		return Incoming{}, ErrUnsupportedVersion
	}
	if !envelope.HasRequestId() || !envelope.HasInReplyTo() || envelope.GetMessage() == nil {
		_ = frame.Payload.Close()
		return Incoming{}, ErrMalformedFrame
	}
	descriptor := envelope.GetPayload()
	if descriptor == nil {
		if frame.PayloadSize != 0 {
			_ = frame.Payload.Close()
			return Incoming{}, ErrMalformedFrame
		}
		return Incoming{
			RequestID: envelope.GetRequestId(),
			InReplyTo: envelope.GetInReplyTo(),
			Message:   envelope.GetMessage(),
			Payload:   frame.Payload,
		}, nil
	}
	if !descriptor.HasLength() || !descriptor.HasChecksumAlgorithm() || descriptor.GetLength() != uint64(frame.PayloadSize) {
		_ = frame.Payload.Close()
		return Incoming{}, ErrMalformedFrame
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
		RequestID:   envelope.GetRequestId(),
		InReplyTo:   envelope.GetInReplyTo(),
		Message:     envelope.GetMessage(),
		Payload:     body,
		PayloadSize: frame.PayloadSize,
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
// package's own tests calls it: the page source computes its CRC over the
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
}

func newVerifyingReader(reader io.ReadCloser, algorithm ChecksumAlgorithm, expected []byte) io.ReadCloser {
	hasher, _ := newHasher(algorithm)
	return &verifyingReader{reader: reader, hasher: hasher, expected: append([]byte(nil), expected...)}
}

func (r *verifyingReader) Read(destination []byte) (int, error) {
	n, err := r.reader.Read(destination)
	if n > 0 {
		_, _ = r.hasher.Write(destination[:n])
	}
	if err == io.EOF && !r.verified {
		r.verified = true
		if !bytesEqual(r.hasher.Sum(nil), r.expected) {
			return n, ErrChecksumMismatch
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
