package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A retire gives a page back to the volume, and the identity the volume now
// gives it is what makes the page clean. A page the volume holds no object for
// is given up altogether: the volume reproduces it without one, which for a
// publication that writes an all-zero page as a sparse hole means it reads back
// as zeros. That is the whole justification, so it holds only for zeros.
//
// A page with the guest's bytes in it that the volume reports no object for is
// bytes that exist nowhere else. Dropping it hands the guest an older version of
// memory it wrote, which is what a fan-out's children died of. The retire fails
// instead: the checkpoint is durable either way, the page stays sealed, and the
// guest keeps its memory.
func TestARetireRefusesToDropAPageTheVolumeCannotReproduce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 1, true)[0] = 0x2b
		seal(t, r)
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		// The volume answers for this page as it would for one it never wrote:
		// no object of its own, so nothing it can reproduce but zeros.
		b.mu.Lock()
		b.zero[1] = true
		b.mu.Unlock()

		err := r.Checkpoint().Retire(t.Context(), true)
		if !errors.Is(err, vmmemory.ErrUndroppable) {
			t.Fatalf("the retire of a page the volume cannot reproduce = %v, want ErrUndroppable", err)
		}
		// The guest still has its bytes: nothing was taken away.
		if got := access(t, r, m, 1, false)[0]; got != 0x2b {
			t.Fatalf("page 1 reads %#x after the refused retire, want the guest's 0x2b", got)
		}
	})
}
