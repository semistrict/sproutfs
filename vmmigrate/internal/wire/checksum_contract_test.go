package wire_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate/internal/wire"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestChecksumAlgorithmsRejectEveryUnsupportedValue(t *testing.T) {
	for value := 0; value <= 255; value++ {
		algorithm := wire.ChecksumAlgorithm(value)
		switch algorithm {
		case wire.ChecksumNone, wire.ChecksumCRC32C, wire.ChecksumSHA256:
			continue
		}
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			for _, data := range [][]byte{nil, []byte("payload")} {
				payload := wire.Payload{Body: bytes.NewReader(data), Size: int64(len(data)), Algorithm: algorithm}
				if err := payload.Validate(); !errors.Is(err, wire.ErrMalformedFrame) {
					t.Fatalf("Validate algorithm %d: %v, want ErrMalformedFrame", value, err)
				}
				if _, err := wire.ComputeChecksum(t.Context(), payload.Body, payload.Size, algorithm); !errors.Is(err, wire.ErrMalformedFrame) {
					t.Fatalf("ComputeChecksum algorithm %d: %v, want ErrMalformedFrame", value, err)
				}
				if _, err := wire.Encode(wire.Outgoing{Message: wrapperspb.String("message"), Payload: payload}); !errors.Is(err, wire.ErrMalformedFrame) {
					t.Fatalf("Encode algorithm %d: %v, want ErrMalformedFrame", value, err)
				}
			}
		})
	}
}

func TestEmptyChecksumsNeedNoReaderAndStillProtectThePayload(t *testing.T) {
	for _, test := range []struct {
		name      string
		algorithm wire.ChecksumAlgorithm
		digest    string
	}{
		{"none", wire.ChecksumNone, ""},
		{"crc32c", wire.ChecksumCRC32C, "00000000"},
		{"sha256", wire.ChecksumSHA256, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checksum, err := wire.ComputeChecksum(t.Context(), nil, 0, test.algorithm)
			if err != nil || hex.EncodeToString(checksum) != test.digest {
				t.Fatalf("empty checksum = %x, %v; want %s", checksum, err, test.digest)
			}
			if _, err := wire.ComputeChecksum(t.Context(), nil, 1, test.algorithm); !errors.Is(err, platform.ErrInvalidRange) {
				t.Fatalf("missing nonempty body: %v, want ErrInvalidRange", err)
			}
			payload := wire.Payload{Algorithm: test.algorithm, Checksum: checksum}
			if err := payload.Validate(); err != nil {
				t.Fatal(err)
			}
			if len(checksum) == 0 {
				return
			}
			payload.Checksum = bytes.Clone(checksum)
			payload.Checksum[0] ^= 1
			frame, err := wire.Encode(wire.Outgoing{Message: wrapperspb.String("empty"), Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			incoming := decodeFrame(t, frame.Header, bytes.NewReader(nil), 0)
			defer incoming.Payload.Close()
			if _, err := io.ReadAll(incoming.Payload); !errors.Is(err, wire.ErrChecksumMismatch) {
				t.Fatalf("corrupt empty checksum: %v, want ErrChecksumMismatch", err)
			}
		})
	}
}

func TestUnchecksummedPayloadRetainsItsBytesAndLength(t *testing.T) {
	data := []byte("payload without a checksum")
	frame := encodeFrame(t, 7, "plain", data, wire.ChecksumNone)
	incoming := decodeFrame(t, frame.Header, bytes.NewReader(data), int64(len(data)))
	defer incoming.Payload.Close()
	got, err := io.ReadAll(incoming.Payload)
	if err != nil || !bytes.Equal(got, data) || incoming.PayloadSize != int64(len(data)) {
		t.Fatalf("plain payload = %q, size %d, %v; want %q", got, incoming.PayloadSize, err, data)
	}
}

func TestChecksumAlgorithmsMatchKnownDigestsAndLengths(t *testing.T) {
	for _, test := range []struct {
		name      string
		algorithm wire.ChecksumAlgorithm
		digest    string
	}{
		{"none", wire.ChecksumNone, ""},
		{"crc32c", wire.ChecksumCRC32C, "364b3fb7"},
		{"sha256", wire.ChecksumSHA256, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, err := hex.DecodeString(test.digest)
			if err != nil {
				t.Fatal(err)
			}
			body := bytes.NewReader([]byte("abc"))
			got, err := wire.ComputeChecksum(t.Context(), body, body.Size(), test.algorithm)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("checksum = %x, %v; want %x", got, err, want)
			}
			for size := 0; size <= 33; size++ {
				payload := wire.Payload{Body: body, Size: body.Size(), Algorithm: test.algorithm, Checksum: make([]byte, size)}
				err := payload.Validate()
				if size == len(want) {
					if err != nil {
						t.Fatalf("valid digest length %d: %v", size, err)
					}
				} else if !errors.Is(err, wire.ErrMalformedFrame) {
					t.Fatalf("digest length %d: %v, want ErrMalformedFrame", size, err)
				}
			}
		})
	}
}
