package vmmemory_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A seal compares every cold copy it took and leaves out each one the guest did
// not change, whatever kind of memory it is: the checkpoint publishes only the
// cold copy that was really stored into. The one left out stays the guest's,
// writable, and is still the session's to give back.
func TestASealLeavesOutAColdCopyTheGuestDidNotChange(t *testing.T) {
	for _, kind := range []vmmemory.MemoryRegionKind{vmmemory.Ram, vmmemory.Pmem} {
		t.Run(kind.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 8, 32, 8)
				b := f.newBacking(4)
				r, m := f.attachKind(kind, b)
				access(t, r, m, 0, true)
				access(t, r, m, 1, true)[0] = 99
				if s := hostStats(t, f); s.UnmappedCopyOnWrites != 2 {
					t.Fatalf("made %d cold copies, want two", s.UnmappedCopyOnWrites)
				}
				if err := r.Seal(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := r.Checkpoint().DirtyPages(); !slices.Equal(got, []uint64{1}) {
					t.Fatalf("the checkpoint holds pages %v, want only the one the guest stored into", got)
				}
				if s := hostStats(t, f); s.UnchangedPages != 1 || s.CheckpointPages != 1 {
					t.Fatalf("left out %d pages and sealed %d, want 1 and 1", s.UnchangedPages, s.CheckpointPages)
				}
				if !m.pages[0].writable {
					t.Fatal("the cold copy left out of the checkpoint is not writable again")
				}
				f.finishCheckpoint(r, b)
				if b.data[0] != 1 || b.data[f.pageSize] != 99 {
					t.Fatalf("the volume holds %d and %d, want the 1 it held and the 99 the guest stored",
						b.data[0], b.data[f.pageSize])
				}
				if given := giveBackColdCopies(t, r); given != 1 {
					t.Fatalf("gave back %d cold copies after the checkpoint, want the one it left out", given)
				}
			})
		})
	}
}

// An eviction that takes a cold copy the guest did not change gives it back to
// the page it was copied from instead of spilling it: the guest maps that page
// read-only again, and nothing goes to the spill or stays in a reservation.
func TestAnEvictionGivesBackAColdCopyInsteadOfSpillingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 32, 8)
		b := f.newBacking(4)
		r, m := f.attach(b)
		access(t, r, m, 0, true)
		access(t, r, m, 1, false)
		s := hostStats(t, f)
		if s.Spills != 0 || s.GivenBackPages != 1 || s.DirtyPages != 0 {
			t.Fatalf("spilled %d, gave back %d, holds %d dirty reservations; want 0, 1 and 0",
				s.Spills, s.GivenBackPages, s.DirtyPages)
		}
		if p, mapped := m.pages[0]; !mapped || p.writable {
			t.Fatalf("the guest maps page 0 as %+v (mapped %t), want its origin read-only", p, mapped)
		}
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatalf("page 0 reads %d, want the 1 the volume holds", got)
		}
	})
}

// A cold copy the pager spilled is compared too, read back from where the spill
// put it, so a guest under memory pressure does not upload what it only read.
// The page it was copied from is pinned, so the comparison has it: the
// eviction took the copy instead. An eviction spills a cold copy only while a
// lock its give-back needs is held, which the test stands in for by turning
// that give-back off.
func TestASealComparesASpilledColdCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetGiveBackVictims(t, false)
		f := newFixture(t, 2, 32, 8)
		b := f.newBacking(4)
		r, m := f.attach(b)
		access(t, r, m, 0, true)
		access(t, r, m, 1, false)
		access(t, r, m, 2, false)
		if s := hostStats(t, f); s.Spills != 1 {
			t.Fatalf("spilled %d pages, want the cold copy", s.Spills)
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := r.Checkpoint().DirtyPages(); len(got) != 0 {
			t.Fatalf("the checkpoint holds pages %v, want none", got)
		}
		if s := hostStats(t, f); s.UnchangedPages != 1 {
			t.Fatalf("left out %d pages, want the spilled cold copy", s.UnchangedPages)
		}
		f.finishCheckpoint(r, b)
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatalf("page 0 reads %d, want the 1 the volume holds", got)
		}
	})
}

// A cold copy the guest stores into after a seal left it out is the guest's
// state from then on, and the next checkpoint publishes it.
func TestAColdCopyLeftOutIsPublishedOnceTheGuestStoresIntoIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		b := f.newBacking(4)
		r, m := f.attach(b)
		access(t, r, m, 0, true)
		f.mustCheckpoint(r, b)
		access(t, r, m, 0, true)[0] = 77
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := r.Checkpoint().DirtyPages(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("the checkpoint holds pages %v, want the page the guest stored into", got)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 77 {
			t.Fatalf("the volume holds %d, want the 77 the guest stored", b.data[0])
		}
	})
}
