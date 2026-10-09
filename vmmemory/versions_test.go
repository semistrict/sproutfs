package vmmemory_test

import (
	"testing"
	"testing/synctest"
)

// A checkpoint's page whose bytes are in its reservation when the checkpoint
// retires keeps them in the spill file as the version it was published as,
// and the page's next load reads that version rather than its volume: a page
// a host's own checkpoint published comes back from the host's disk
// (plans/local-writeback-2026-10-09.md, step 1).
func TestAPublishedSpilledPageLoadsFromTheSpillFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		r, m, b := f.memoryRegion(4)
		accessUnder(f.ctx, t, r, m, 0, true)[0] = 41
		// Two more pages than the arena holds besides page zero, so page
		// zero is evicted, and its bytes go to its reservation.
		accessUnder(f.ctx, t, r, m, 1, false)
		accessUnder(f.ctx, t, r, m, 2, false)
		if s := hostStats(t, f); s.Spills != 1 {
			t.Fatalf("spilled %d pages, want page zero", s.Spills)
		}
		f.mustCheckpoint(r, b)
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d, want the guest's 41", b.data[0])
		}
		s := hostStats(t, f)
		if s.KeptVersions != 1 || s.Versions != 1 {
			t.Fatalf("kept %d versions, %d now, want page zero's", s.KeptVersions, s.Versions)
		}
		loads := b.loads
		if got := accessUnder(f.ctx, t, r, m, 0, false)[0]; got != 41 {
			t.Fatalf("page zero reads %d after its checkpoint, want 41", got)
		}
		if b.loads != loads {
			t.Fatalf("loading page zero read its volume %d times, want none: the spill file keeps its version", b.loads-loads)
		}
		if s := hostStats(t, f); s.VersionLoads != 1 {
			t.Fatalf("%d pages loaded from versions, want page zero", s.VersionLoads)
		}
	})
}

// A spilled page refaulted as the guest's own is changed from then on with
// nothing passing through the spill, so its reservation's bytes are no version
// of it any more. A checkpoint of the page as the guest changed it keeps none
// of them, and the page's next load reads what the checkpoint published.
func TestARefaultedPageKeepsNoStaleVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		r, m, b := f.memoryRegion(4)
		accessUnder(f.ctx, t, r, m, 0, true)[0] = 41
		accessUnder(f.ctx, t, r, m, 1, false)
		accessUnder(f.ctx, t, r, m, 2, false)
		// Page zero comes back from its reservation, and the guest stores
		// into it again while it is resident.
		accessUnder(f.ctx, t, r, m, 0, true)[0] = 42
		f.mustCheckpoint(r, b)
		if b.data[0] != 42 {
			t.Fatalf("the checkpoint published %d, want the guest's 42", b.data[0])
		}
		if s := hostStats(t, f); s.KeptVersions != 0 {
			t.Fatalf("kept %d versions, want none: the reservation's bytes are the 41 the guest stored over", s.KeptVersions)
		}
		accessUnder(f.ctx, t, r, m, 1, false)
		accessUnder(f.ctx, t, r, m, 2, false)
		if got := accessUnder(f.ctx, t, r, m, 0, false)[0]; got != 42 {
			t.Fatalf("page zero reads %d after its checkpoint, want 42", got)
		}
	})
}
