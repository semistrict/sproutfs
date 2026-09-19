// Package blob encodes independently readable, bounded raw or Zstandard blobs.
// The envelope identifies its format explicitly and checks the decoded bytes.
// It does not change the page identity of the data it contains.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/klauspost/compress/zstd"
)

const (
	HeaderSize = 48
	MaxSize    = 256 << 20
	windowSize = 1 << 20
	// DefaultWorkers is how many codecs of each kind the package-wide pool
	// holds, for a caller that supplies none of its own.
	DefaultWorkers = 4
	// MaximumWorkers bounds one pool: a codec's workspace is memory a caller
	// asked for without saying so.
	MaximumWorkers = 256
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
	w, err := acquireEncoder(ctx, c)
	if err != nil {
		return nil, err
	}
	defer func() { c.encoders <- w }()
	base := len(dst)
	out := slices.Grow(dst, HeaderSize+len(data))[:base+HeaderSize]
	header := out[base:]
	clear(header)
	copy(header, "SPB1")
	binary.LittleEndian.PutUint64(header[8:16], uint64(len(data)))
	sum := sha256.Sum256(data)
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

// Size validates framing and the decoded-size claim against the caller's
// logical limit. Decode must still verify the payload before it is trusted.
func Size(data []byte, maximum int) (int, error) {
	if maximum < 0 || len(data) < HeaderSize || string(data[:4]) != "SPB1" ||
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
	sum := sha256.Sum256(out)
	if !bytes.Equal(sum[:], data[16:HeaderSize]) {
		return nil, ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
