package main

import (
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/blob"
)

// The guest's pages compress about as far as the Valkey heap of the real
// application's restore did: 6.09 GB to about 2.8 GB, 46 %.
func TestTheGuestCompressesLikeTheValkeyHeap(t *testing.T) {
	g := newGuest(8)
	encoded := 0
	for page := range g.pages() {
		envelope, err := blob.Encode(t.Context(), g.memory[page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB])
		if err != nil {
			t.Fatal(err)
		}
		encoded += len(envelope)
	}
	if got, want := encoded, 7_759_956; got != want {
		t.Fatalf("8 pages encode to %d bytes (%.3f of %d), want %d", got,
			float64(got)/float64(len(g.memory)), len(g.memory), want)
	}
}
