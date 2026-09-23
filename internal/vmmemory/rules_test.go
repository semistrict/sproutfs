package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// gap is how near a store must land to a page its range already holds for the
// pages between to be made private with it. It is the pager's own constant,
// restated here so a test that moves it reads as a test that moved it.
const gap = 16

// held makes the pages [first, last) of a region resident and shared, which is
// what a guest has read and what a fork's attach populates. The rules copy only
// pages whose bytes this host already holds, because a rule evicts for nothing
// the guest did not write, so this is the state they act on.
func held(t *testing.T, r *vmmemory.Region, m *mapping, first, last uint64) {
	t.Helper()
	for page := first; page < last; page++ {
		access(t, r, m, page, false)
	}
}

// A store within the gap of a page its range already holds makes the pages
// between them private in the same fault: one mapping command, one run, and the
// pages it copied counted exactly.
func TestAStoreNearAPrivatePageClosesTheGapInOneMapping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedRegion(t, 2*rangePages)
		const first = 100
		held(t, r, m, first, first+gap+1)
		access(t, r, m, first, true)[0] = 7
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("a store with nothing near it copied %d pages by a rule, want none", got)
		}
		commands := m.maps
		access(t, r, m, first+gap, true)[0] = 7
		if got := m.maps - commands; got != 1 {
			t.Fatalf("the store that closed the gap issued %d mapping commands, want 1", got)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("two stores %d pages apart are %d private mappings, want 1", gap, got)
		}
		if got := hostStats(t, f).RuleCopies; got != gap-1 {
			t.Fatalf("closing a gap of %d pages copied %d of them, want %d", gap, got, gap-1)
		}
		// Every page of the run is the guest's own, at its own offset, and its
		// bytes are the ones it had.
		for page := uint64(first); page <= first+gap; page++ {
			if got := m.pages[page].slot % rangePages; got != int(page%rangePages) {
				t.Fatalf("page %d is at offset %d, want one congruent to %d modulo %d",
					page, m.pages[page].slot, page%rangePages, rangePages)
			}
			if !m.pages[page].writable {
				t.Fatalf("page %d of the closed gap is not the guest's to store into", page)
			}
		}
		if got := access(t, r, m, first+1, false)[0]; got != first+1+1 {
			t.Fatalf("a page the gap rule copied reads %d, want the %d it held", got, first+2)
		}
	})
}

// A store past the gap is its own run: the rule is a bound and not a habit.
func TestAStorePastTheGapIsItsOwnMapping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedRegion(t, 2*rangePages)
		const first = 100
		held(t, r, m, first, first+gap+2)
		access(t, r, m, first, true)[0] = 7
		access(t, r, m, first+gap+1, true)[0] = 7
		if got := privateMappings(m); got != 2 {
			t.Fatalf("two stores %d pages apart are %d private mappings, want 2", gap+1, got)
		}
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("a store past the gap copied %d pages by a rule, want none", got)
		}
	})
}

// A gap is never closed across a range's boundary: the extent belongs to the
// range, so the pages either side of one are two runs however near they are.
func TestAGapIsNeverClosedAcrossARangeBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedRegion(t, 2*rangePages)
		held(t, r, m, rangePages-1, rangePages+4)
		access(t, r, m, rangePages-1, true)[0] = 7
		access(t, r, m, rangePages+3, true)[0] = 7
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("a store four pages past a range boundary copied %d pages, want none", got)
		}
		if s := hostStats(t, f); s.PrivateExtents != 2 {
			t.Fatalf("a page either side of a range boundary owns %d extents, want 2", s.PrivateExtents)
		}
		if got := privateMappings(m); got != 2 {
			t.Fatalf("a page either side of a range boundary is %d private mappings, want 2", got)
		}
	})
}

// A range whose pages reach half of it is filled: the rest are copied into the
// holes of its extent, the range is one mapping and one write-protect command
// at a seal, and nothing already there is copied again.
func TestARangeThatIsHalfPrivateBecomesWhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const half = rangePages / 2
		f := placedFixture(t, 4*rangePages)
		b := f.newBacking(2 * rangePages)
		r, m := f.attach(b)
		held(t, r, m, 0, rangePages)
		for page := range uint64(half) {
			access(t, r, m, page, true)[0] = 7
		}
		s := hostStats(t, f)
		if s.RuleCopies != half {
			t.Fatalf("filling a range that held %d pages copied %d, want %d: nothing already"+
				" private is copied again", half, s.RuleCopies, half)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("a whole range is %d private mappings, want 1", got)
		}
		if got := len(m.pages); got != rangePages {
			t.Fatalf("a whole range maps %d pages, want %d", got, rangePages)
		}
		for page := range uint64(rangePages) {
			if !m.pages[page].writable {
				t.Fatalf("page %d of a whole range is not the guest's to store into", page)
			}
		}
		// One run of consecutive dirty pages is one write-protect command.
		protects := m.protects
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.protects - protects; got != 1 {
			t.Fatalf("sealing a whole range issued %d write-protect commands, want 1", got)
		}
		if got := len(r.Checkpoint().DirtyPages()); got != rangePages {
			t.Fatalf("the seal took %d pages of a whole range, want %d", got, rangePages)
		}
		f.finishCheckpoint(r, b)
	})
}

// The settle undoes what the rules copied and the guest never wrote: a page
// made private to close a gap has an origin like any other copy, and a settle
// that finds it unchanged hands it back.
func TestASettleHandsBackTheGapPagesTheGuestNeverWrote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, b := placedRegion(t, 2*rangePages)
		const first = 100
		held(t, r, m, first, first+gap+1)
		access(t, r, m, first, true)[0] = 7
		access(t, r, m, first+gap, true)[0] = 7
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := len(r.Checkpoint().DirtyPages()); got != gap+1 {
			t.Fatalf("the seal took %d pages, want the %d of the closed run", got, gap+1)
		}
		if got := f.settle(r); got != gap-1 {
			t.Fatalf("the settle handed back %d pages, want the %d the gap rule copied", got, gap-1)
		}
		if got := len(r.Checkpoint().DirtyPages()); got != 2 {
			t.Fatalf("the checkpoint publishes %d pages, want the 2 the guest stored into", got)
		}
		f.finishCheckpoint(r, b)
		if got := access(t, r, m, first+1, false)[0]; got != first+1+1 {
			t.Fatalf("a page handed back reads %d, want the %d it always held", got, first+2)
		}
		if got := access(t, r, m, first, false)[0]; got != 7 {
			t.Fatalf("the page the guest stored into reads %d, want 7", got)
		}
	})
}

// A whole range stays whole: a settle that handed one of its pages back would
// break it into three mappings again, for a page the guest is about to write.
func TestAWholeRangeStaysWholeThroughASettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const half = rangePages / 2
		f := placedFixture(t, 4*rangePages)
		b := f.newBacking(2 * rangePages)
		r, m := f.attach(b)
		held(t, r, m, 0, rangePages)
		for page := range uint64(half) {
			access(t, r, m, page, true)[0] = 7
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := f.settle(r); got != 0 {
			t.Fatalf("the settle handed %d pages of a whole range back, want none", got)
		}
		if got := len(r.Checkpoint().DirtyPages()); got != rangePages {
			t.Fatalf("a whole range publishes %d pages, want %d", got, rangePages)
		}
		f.finishCheckpoint(r, b)
		// The checkpoint published every page of it in place, so the range is
		// still one mapping — the guest's own no longer, but whole.
		if got := mappings(m); got != 1 {
			t.Fatalf("a whole range is %d mappings after its checkpoint, want 1", got)
		}
	})
}

// The budget behind the rules is a backstop. A store whose mapping the client
// refuses — its process has no mapping left — makes the range the guest is
// writing in whole, so the alternations that were costing that process a
// mapping each stop costing it anything, and the store is served again. A
// counter says it acted, and it is expected never to.
func TestARefusedMappingMakesTheRangeWhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stores, stride = 8, 32
		f, r, m, _ := placedRegion(t, 2*rangePages)
		held(t, r, m, 0, rangePages)
		for i := range uint64(stores) {
			access(t, r, m, 20+stride*i, true)[0] = 7
		}
		if got := privateMappings(m); got != stores {
			t.Fatalf("%d scattered stores are %d private mappings, want %d", stores, got, stores)
		}
		if got := hostStats(t, f).MappingMerges; got != 0 {
			t.Fatalf("the backstop acted %d times before the budget was reached, want 0", got)
		}
		// The next mapping command, and only it, is refused.
		commands := 0
		m.onMap = func(uint64, int) { commands++; m.refuseMap = commands == 1 }
		access(t, r, m, 300, true)[0] = 7
		if s := hostStats(t, f); s.MappingMerges != 1 {
			t.Fatalf("a refused store made the backstop act %d times, want 1", s.MappingMerges)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("the range the backstop made whole is %d private mappings, want 1", got)
		}
		if got := len(m.pages); got != rangePages {
			t.Fatalf("the range the backstop made whole maps %d pages, want %d", got, rangePages)
		}
	})
}

// The offset the placement rule gives a page is not always holding that page.
// A checkpoint freezes the guest's copy where it is, and retiring that
// checkpoint leaves the page published at that same offset — so the next store
// into it finds its own offset occupied and takes an ordinary one, while the
// offset goes on holding the older, published page.
//
// A rule that reads such an offset as "this page is at its own offset" maps the
// guest over the older page: the store the guest made is lost, and every region
// that inherited that published identity has its page written under it. So the
// run a rule maps is the pages whose memory really is at their own offsets, and
// nothing else.
func TestARuleNeverMapsAPageOntoAnOffsetHoldingAnotherPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const half = rangePages / 2
		f := placedFixture(t, 4*rangePages)
		b := f.newBacking(2 * rangePages)
		r, m := f.attach(b)
		held(t, r, m, 0, rangePages)
		// One page of the range is the guest's own and is then published, which
		// is what leaves it at the offset the rule gives it while belonging to
		// the volume rather than to the guest.
		const page = 5
		access(t, r, m, page, true)[0] = 41
		f.mustCheckpoint(r, b)
		// The guest stores into it again. Its own offset holds the page the
		// checkpoint published, so this copy goes to an ordinary offset.
		access(t, r, m, page, true)[0] = 42
		// And the range reaches half its own pages, so the rest of it is copied
		// into the holes of its extent and the whole range becomes one mapping.
		for p := range uint64(half + 1) {
			if p != page {
				access(t, r, m, p, true)[0] = 7
			}
		}
		if got := access(t, r, m, page, false)[0]; got != 42 {
			t.Fatalf("the page the guest stored 42 into reads %d once its range was filled", got)
		}
		if got := access(t, r, m, page, true)[0]; got != 42 {
			t.Fatalf("the page the guest stored 42 into is mapped over something holding %d", got)
		}
	})
}

// A pager whose page is the whole range runs neither rule: it has one page per
// range, so there is no gap to close and nothing to fill.
func TestAPagerWhosePageIsTheRangeRunsNeitherRule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize2MiB,
			ResidentPages: 16, ArenaOffsets: 4096, LogicalPages: 32, DirtyPages: 16,
			ReadAheadPages: 1, WriteAheadPages: 1})
		r, m, _ := f.region(8)
		access(t, r, m, 1, true)[0] = 7
		access(t, r, m, 3, true)[0] = 7
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("a 2 MiB pager copied %d pages by a rule, want none", got)
		}
	})
}
