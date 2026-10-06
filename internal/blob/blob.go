// Package blob encodes independently readable, bounded raw or Zstandard blobs.
// The envelope identifies its format explicitly and checks the decoded bytes.
// It does not change the page identity of the data it contains.
//
// An envelope is a header of HeaderSize bytes and then its payload:
//
//	0  4  magic "SPB2": "SPB" and the format version
//	4  1  the codec: 0 raw, 1 Zstandard
//	5  3  zero
//	8  8  the decoded length
//	16 16 the XXH3-128 of the decoded bytes
//
// The digest finds corruption. It is not a defence against a forger: every
// envelope a host reads was written by a host of the same deployment. Format 1
// carried a SHA-256 in a 48-byte header, and is refused.
package blob

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/klauspost/compress/zstd"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/zeebo/xxh3"
)

const (
	HeaderSize = 32
	MaxSize    = 256 << 20
	// magic opens every envelope. Its last byte is the format version.
	magic      = "SPB2"
	windowSize = 1 << 20
	// DefaultWorkers is how many codecs of each kind the package-wide pool
	// holds, for a caller that supplies none of its own.
	DefaultWorkers = 4
	// MaximumWorkers bounds one pool: a codec's workspace is memory a caller
	// asked for without saying so.
	MaximumWorkers = 256
	// WorkEncode is the work an encode costs a processor, as a simulation
	// prices it (sim.Work): the bytes it encodes, spent while its encoder is
	// held, so a pool of n encoders does n of them side by side.
	WorkEncode = "blob/encode"
)

var (
	ErrInvalid = errors.New("blob: invalid or oversized encoding")
	// ErrInvalidConfig reports a pool size this package cannot build.
	ErrInvalidConfig = errors.New("blob: invalid codec pool size")
)

// Codecs is a bounded pool of Zstandard codecs. Encoding and decoding draw on
// separate pools, so the path that publishes checkpoints and the path that
// serves a guest's page fault do not wait on each other and can be sized apart:
// a host wants many small decodes in flight and only as many encodes as it
// uploads. Each codec is synchronous, and an idle one retains no caller-owned
// buffers. A Codecs is safe for concurrent use.
type Codecs struct {
	encoders chan *zstd.Encoder
	decoders chan *zstd.Decoder
}

// NewCodecs builds a pool of encode and decode workers.
func NewCodecs(encode, decode int) (*Codecs, error) {
	if encode < 1 || decode < 1 || encode > MaximumWorkers || decode > MaximumWorkers {
		return nil, ErrInvalidConfig
	}
	c := &Codecs{encoders: make(chan *zstd.Encoder, encode), decoders: make(chan *zstd.Decoder, decode)}
	for range encode {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1),
			zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(windowSize),
			zstd.WithSingleSegment(false), zstd.WithEncoderCRC(false))
		if err != nil {
			return nil, err
		}
		// Initialize the codec's internal channels along with the pool, rather
		// than binding a reused codec to its first caller's synctest scope.
		encoder.EncodeAll(nil, nil)
		c.encoders <- encoder
	}
	for range decode {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(MaxSize), zstd.WithDecoderMaxWindow(windowSize),
			zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return nil, err
		}
		c.decoders <- decoder
	}
	return c, nil
}

// Encoders is how many encodes this pool runs at once.
func (c *Codecs) Encoders() int { return cap(c.encoders) }

// Default is the package-wide pool, which a caller that has not sized one of
// its own encodes and decodes through.
func Default() *Codecs { return defaultCodecs }

var defaultCodecs = func() *Codecs {
	c, err := NewCodecs(DefaultWorkers, DefaultWorkers)
	if err != nil {
		panic(err)
	} // Constant sizes are a programming invariant.
	return c
}()

func acquireEncoder(ctx context.Context, c *Codecs) (*zstd.Encoder, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case w := <-c.encoders:
		return w, nil
	}
}

func acquireDecoder(ctx context.Context, c *Codecs) (*zstd.Decoder, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case w := <-c.decoders:
		return w, nil
	}
}

// Encode returns an owned envelope through the package-wide pool. Raw fallback
// bounds the stored size by len(data)+HeaderSize even for incompressible input.
// No dictionary is needed.
func Encode(ctx context.Context, data []byte) ([]byte, error) {
	return defaultCodecs.AppendEncode(ctx, nil, data)
}

// AppendEncode appends data's envelope to dst through the package-wide pool.
func AppendEncode(ctx context.Context, dst, data []byte) ([]byte, error) {
	return defaultCodecs.AppendEncode(ctx, dst, data)
}

// Decode verifies an entire envelope through the package-wide pool.
func Decode(ctx context.Context, data []byte, maximum int) ([]byte, error) {
	return defaultCodecs.Decode(ctx, data, maximum)
}

// Encode returns an owned envelope from this pool.
func (c *Codecs) Encode(ctx context.Context, data []byte) ([]byte, error) {
	return c.AppendEncode(ctx, nil, data)
}

// AppendEncode appends data's envelope to dst and returns the extended slice.
// A caller assembling a larger buffer encodes into it rather than into an
// allocation of its own, so the envelope is written once and never copied: a
// checkpoint part fills in place, whatever the size of what is written into it.
func (c *Codecs) AppendEncode(ctx context.Context, dst, data []byte) ([]byte, error) {
	if len(data) > MaxSize {
		return nil, ErrInvalid
	}
	e, err := c.Encoder(ctx)
	if err != nil {
		return nil, err
	}
	defer e.Release()
	return e.AppendEncode(ctx, dst, data)
}

// Encoder is one encoder of a pool, held from Codecs.Encoder until Release. A
// caller that encodes several things side by side takes each encoder itself,
// in the order it wants them encoded, rather than leaving the order to
// whichever of its goroutines reaches the pool first. It is not safe for
// concurrent use.
type Encoder struct {
	codecs *Codecs
	zstd   *zstd.Encoder
}

// Encoder takes one encoder of the pool, waiting under ctx while every one is
// held.
func (c *Codecs) Encoder(ctx context.Context) (*Encoder, error) {
	w, err := acquireEncoder(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Encoder{codecs: c, zstd: w}, nil
}

// Release gives the encoder back to its pool. It must be called once.
func (e *Encoder) Release() { e.codecs.encoders <- e.zstd }

// AppendEncode is Codecs.AppendEncode on an encoder already held.
func (e *Encoder) AppendEncode(ctx context.Context, dst, data []byte) ([]byte, error) {
	if len(data) > MaxSize {
		return nil, ErrInvalid
	}
	if err := sim.Work(ctx, WorkEncode, len(data)); err != nil {
		return nil, err
	}
	w := e.zstd
	base := len(dst)
	out := slices.Grow(dst, HeaderSize+len(data))[:base+HeaderSize]
	header := out[base:]
	clear(header)
	copy(header, magic)
	binary.LittleEndian.PutUint64(header[8:16], uint64(len(data)))
	sum := digest(data)
	copy(header[16:HeaderSize], sum[:])
	if len(data) >= 256 {
		out = w.EncodeAll(data, out)
		out[base+4] = 1
	}
	if out[base+4] == 0 || len(out)-base-HeaderSize >= len(data) {
		out = append(out[:base+HeaderSize], data...)
		out[base+4] = 0
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// digest is the check an envelope carries of its decoded bytes.
func digest(data []byte) [16]byte { return xxh3.Hash128(data).Bytes() }

// Size validates framing and the decoded-size claim against the caller's
// logical limit. Decode must still verify the payload before it is trusted.
// An envelope of another format version is refused with that version named.
func Size(data []byte, maximum int) (int, error) {
	if len(data) >= len(magic) && string(data[:3]) == magic[:3] && data[3] != magic[3] {
		return 0, fmt.Errorf("%w: envelope format version %c, want %c", ErrInvalid, data[3], magic[3])
	}
	if maximum < 0 || len(data) < HeaderSize || string(data[:4]) != magic ||
		data[4] > 1 || data[5] != 0 || data[6] != 0 || data[7] != 0 {
		return 0, ErrInvalid
	}
	n := binary.LittleEndian.Uint64(data[8:16])
	if n > uint64(min(maximum, MaxSize)) || uint64(len(data)-HeaderSize) > n ||
		(data[4] == 0 && uint64(len(data)-HeaderSize) != n) {
		return 0, ErrInvalid
	}
	return int(n), nil
}

// Decode verifies an entire envelope from this pool. Raw output aliases data
// and takes no codec; compressed output is allocated only after checking its
// size, and cannot grow past it.
func (c *Codecs) Decode(ctx context.Context, data []byte, maximum int) ([]byte, error) {
	n, err := Size(data, maximum)
	if err != nil {
		return nil, err
	}
	out := data[HeaderSize:]
	if data[4] == 1 {
		w, err := acquireDecoder(ctx, c)
		if err != nil {
			return nil, err
		}
		defer func() { c.decoders <- w }()
		out, err = w.DecodeAll(out, make([]byte, 0, n))
		if err != nil {
			return nil, errors.Join(ErrInvalid, err)
		}
	}
	if len(out) != n {
		return nil, ErrInvalid
	}
	sum := digest(out)
	if !bytes.Equal(sum[:], data[16:HeaderSize]) {
		return nil, ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
