package blob_test

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/semistrict/sproutfs/internal/blob"
)

// These controls measure codec costs, not a representative guest workload.
func BenchmarkBlob(b *testing.B) {
	random := make([]byte, 2<<20)
	r := rand.NewChaCha8([32]byte{9})
	_, _ = r.Read(random)
	mixed := bytes.Clone(random)
	clear(mixed[len(mixed)/2:])
	for _, corpus := range []struct {
		name string
		data []byte
	}{
		{"zero-page", make([]byte, 2<<20)},
		{"half-random-page", mixed},
		{"random-page", random},
	} {
		encoded, err := blob.Encode(b.Context(), corpus.data)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(corpus.name+"/encode", func(b *testing.B) {
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := blob.Encode(b.Context(), corpus.data); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(encoded)), "encoded-B")
		})
		b.Run(corpus.name+"/decode", func(b *testing.B) {
			b.SetBytes(int64(len(corpus.data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := blob.Decode(b.Context(), encoded, len(corpus.data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
