package vmmemory_test

import (
	"testing"
	"testing/synctest"
)

func TestSparseSiblingsSurviveArenaPressureWithoutRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 1, 9, 4)
		ab, bb := f.newBacking(4), f.newBacking(4)
		clear(ab.data)
		clear(bb.data)
		for page := range uint64(4) {
			ab.zero[page], bb.zero[page] = true, true
		}
		a, am := f.attach(ab)
		b, bm := f.attach(bb)
		for page := range uint64(4) {
			if access(t, a, am, page, false)[0] != 0 || access(t, b, bm, page, false)[0] != 0 {
				t.Fatal("sparse page was not zero")
			}
		}
		// Reclaiming real data must not need to contact either zero-only region.
		am.failRevoke, bm.failRevoke = true, true
		data, dm, _ := f.region(1)
		if got := access(t, data, dm, 0, false)[0]; got != 1 {
			t.Fatalf("data page = %d", got)
		}
		if len(am.pages) != 4 || len(bm.pages) != 4 {
			t.Fatal("arena pressure removed sparse mappings")
		}
		am.failRevoke = false
		access(t, a, am, 2, true)[0] = 42
		access(t, a, am, 3, true)[0] = 43 // Evict and spill the first private page.
		if access(t, a, am, 2, false)[0] != 42 || access(t, b, bm, 2, false)[0] != 0 {
			t.Fatal("private spill/refault changed a sparse sibling")
		}
		if ab.loads != 0 || bb.loads != 0 {
			t.Fatal("explicit zero pages needed backing reads")
		}
	})
}
