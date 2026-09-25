package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// TestAPageASiblingWillNotGiveUpDoesNotFailTheVMEvictingIt. Children of one
// fork share every page they inherited, which is the whole point of forking
// them onto one host. A machine whose memory session has died can no longer
// take a mapping away, so a page it holds can never be reused — but the
// reclaim that finds that out is another machine's, and answering it with the
// dead machine's failure ends that one too, and then the next one that shares a
// page with it. The eviction takes another victim instead; the page stays
// where it is until the memory region that holds it is closed.
func TestAPageASiblingWillNotGiveUpDoesNotFailTheVMEvictingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 32,
			DirtyPages: 2, ReadAheadPages: 1})
		a, am, _ := f.memoryRegion(8)
		b, bm, _ := f.memoryRegion(8)
		// One resident page, two children of one parent reachable from it.
		access(t, a, am, 0, false)
		access(t, b, bm, 0, false)
		if am.pages[0].slot != bm.pages[0].slot {
			t.Fatal("the two memory regions did not share the resident page of one stored identity")
		}
		// The second machine stops answering. Its memory region has taken no failure of
		// its own yet: what says so is the first mapping command that does not
		// come back, and the first one anybody sends is the revocation the
		// sibling's next eviction needs.
		bm.failRevoke = true

		// The arena holds two pages and this memory region walks eight pages, so every
		// fault past the second evicts, and the least recently used page is the
		// one the dead machine will not give up.
		for page := uint64(1); page < 8; page++ {
			if err := a.Fault(t.Context(), page, false); err != nil {
				t.Fatalf("a fault of a live VM whose sibling will not give up a shared page: %v", err)
			}
		}
		if _, err := f.h.Stats(t.Context()); err != nil {
			t.Fatalf("the pager was made terminal by one machine's death: %v", err)
		}
		// The machine that would not answer is the one that is finished, and it
		// is finished from the first command it refused.
		if err := b.Fault(t.Context(), 1, false); err == nil {
			t.Fatal("the memory region whose mapping stopped answering went on serving faults")
		}
	})
}
