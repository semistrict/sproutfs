package blob_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/semistrict/sproutfs/internal/blob"
)

func TestRoundTrip(t *testing.T) {
	random := make([]byte, 2<<20)
	r := rand.NewChaCha8([32]byte{4})
	_, _ = r.Read(random)
	for _, data := range [][]byte{nil, []byte("state"), bytes.Repeat([]byte("guest memory "), 160000), random} {
		encoded, err := blob.Encode(t.Context(), data)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > len(data)+blob.HeaderSize {
			t.Fatal("encoding expanded beyond raw fallback")
		}
		got, err := blob.Decode(t.Context(), encoded, len(data))
		if err != nil || !bytes.Equal(data, got) {
			t.Fatalf("round trip: %v", err)
		}
	}
	encoded, err := blob.Encode(t.Context(), bytes.Repeat([]byte("guest memory "), 160000))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= 20000 {
		t.Fatalf("repeated data was not compressed: %d bytes", len(encoded))
	}
	encoded, err = blob.Encode(t.Context(), random)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != len(random)+blob.HeaderSize {
		t.Fatalf("random data did not use raw fallback: %d", len(encoded))
	}
}

// Encoding into a buffer that already holds bytes writes the same envelope as
// encoding on its own, whatever the capacity that buffer had to spare. This is
// what lets a checkpoint part be filled in place.
func TestAppendEncodeWritesTheSameEnvelope(t *testing.T) {
	random := make([]byte, 2<<20)
	if _, err := rand.NewChaCha8([32]byte{11}).Read(random); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("state"), bytes.Repeat([]byte("guest memory "), 160000), random} {
		alone, err := blob.Encode(t.Context(), data)
		if err != nil {
			t.Fatal(err)
		}
		for _, spare := range []int{0, len(data) + 1<<20} {
			prefix := bytes.Repeat([]byte{0xa5}, 4096)
			buffer := append(make([]byte, 0, len(prefix)+spare), prefix...)
			buffer, err = blob.AppendEncode(t.Context(), buffer, data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buffer[:len(prefix)], prefix) {
				t.Fatal("appending an envelope disturbed the bytes already in the buffer")
			}
			if !bytes.Equal(buffer[len(prefix):], alone) {
				t.Fatalf("appended envelope of %d bytes differs from the one encoded on its own", len(data))
			}
			got, err := blob.Decode(t.Context(), buffer[len(prefix):], len(data))
			if err != nil || !bytes.Equal(data, got) {
				t.Fatalf("round trip: %v", err)
			}
		}
	}
}

func TestRejectInvalidEncoding(t *testing.T) {
	encoded, err := blob.Encode(t.Context(), bytes.Repeat([]byte{7}, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"truncated header":     func(b []byte) []byte { return b[:20] },
		"truncated payload":    func(b []byte) []byte { return b[:len(b)-1] },
		"unknown format":       func(b []byte) []byte { b[3]++; return b },
		"unknown codec":        func(b []byte) []byte { b[4] = 2; return b },
		"reserved bits":        func(b []byte) []byte { b[5] = 1; return b },
		"oversized output":     func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:16], ^uint64(0)); return b },
		"output exceeds claim": func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:16], 1<<20); return b },
		"wrong checksum":       func(b []byte) []byte { b[16]++; return b },
		"trailing garbage":     func(b []byte) []byte { return append(b, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := blob.Decode(t.Context(), mutate(bytes.Clone(encoded)), 2<<20); !errors.Is(err, blob.ErrInvalid) {
				t.Fatalf("accepted corruption: %v", err)
			}
		})
	}
	if _, err := blob.Decode(t.Context(), encoded, (2<<20)-1); !errors.Is(err, blob.ErrInvalid) {
		t.Fatalf("accepted excessive decoded size: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := blob.Encode(ctx, []byte("a")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := blob.Decode(ctx, encoded, 2<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func FuzzDecode(f *testing.F) {
	encoded, _ := blob.Encode(f.Context(), bytes.Repeat([]byte{3}, 4096))
	f.Add(encoded)
	f.Add([]byte("old raw format"))
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := blob.Decode(t.Context(), data, 1<<16)
		if err == nil && len(decoded) > 1<<16 {
			t.Fatal("decoded size escaped limit")
		}
	})
}
