package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A guest that stores into more memory than the arena holds takes a fault, and
// evicts a page, on nearly every store. Eviction took the page faulted in
// longest ago. A hog faults far more often than its neighbour, so the
// neighbour's working set was always the oldest in the arena, and the neighbour
// ran at the speed of its own refaults. Each attached memory region is owed an
// equal share of the arena, and one that has asked for a page within the last
// turnover keeps the pages of its share. An idle one does not, so its pages go
// to whoever faults.
func TestAMemoryRegionKeepsItsShareWhileItFaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Eight pages and two memory regions: a share is four, and a turnover
		// is eight evictions.
		f := newFixture(t, 8, 32, 32)
		// Fresh memory, as a guest's RAM is: a store into a page of zeros copies
		// nothing, so every page in the arena is one a guest maps.
		fresh := func(pages int) (*vmmemory.MemoryRegion, *mapping) {
			b := f.newBacking(pages)
			for page := range uint64(pages) {
				b.zero[page] = true
			}
			return f.attach(b)
		}
		hog, hm := fresh(16)
		calm, cm := fresh(4)
		for page := range uint64(3) {
			access(t, calm, cm, page, true)[0] = byte(21 + page)
		}
		store := func(from, to uint64) {
			for page := from; page < to; page++ {
				access(t, hog, hm, page%16, true)[0] = byte(page)
			}
		}
		// Five pages are free. The hog's next two stores are past its share,
		// and the neighbour has just faulted, so the hog evicts its own.
		store(0, 7)
		residentAre(t, "the hog's first stores past its share", calm, 3, hog, 5)

		// The neighbour asks for nothing for a whole turnover, so its pages go.
		store(7, 23)
		residentAre(t, "a turnover with the neighbour idle", calm, 0, hog, 8)

		// It faults its pages back in, from the hog, which is past its share.
		// Then it keeps them while the hog cycles through its own.
		for page := range uint64(3) {
			if got := access(t, calm, cm, page, false)[0]; got != byte(21+page) {
				t.Fatalf("the neighbour's page %d holds %d, want %d", page, got, 21+page)
			}
		}
		store(23, 28)
		residentAre(t, "the neighbour faulting again", calm, 3, hog, 5)

		// Its own evictions spilled the hog's pages and none was lost: each holds
		// the last of the 28 stores into it.
		for page := range uint64(16) {
			want := byte(page)
			if page < 12 {
				want = byte(page + 16)
			}
			if got := access(t, hog, hm, page, false)[0]; got != want {
				t.Fatalf("the hog's page %d holds %d, want %d", page, got, want)
			}
		}
	})
}

// residentAre checks how many resident pages each of two memory regions holds.
func residentAre(t *testing.T, after string, a *vmmemory.MemoryRegion, wantA int, b *vmmemory.MemoryRegion, wantB int) {
	t.Helper()
	as, err := a.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	bs, err := b.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if as.ResidentPages != wantA || bs.ResidentPages != wantB {
		t.Fatalf("after %s, the neighbour holds %d resident pages and the hog %d, want %d and %d",
			after, as.ResidentPages, bs.ResidentPages, wantA, wantB)
	}
}
