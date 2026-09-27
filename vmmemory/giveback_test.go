package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// sharedCopy is two memory regions of one checkpoint mapping four pages each,
// the first of which has taken page 0 writable and stored nothing: the copy a
// cold read makes where KVM asks for every page writable.
func sharedCopy(t *testing.T) (f *fixture, a *vmmemory.MemoryRegion, am *mapping, ab *backing,
	b *vmmemory.MemoryRegion, bm *mapping) {
	t.Helper()
	f = newFixture(t, 8, 32, 8)
	a, am, ab = f.memoryRegion(4)
	b, bm, _ = f.memoryRegion(4)
	for page := uint64(0); page < 4; page++ {
		access(t, a, am, page, false)
		access(t, b, bm, page, false)
	}
	access(t, a, am, 0, true)
	if am.pages[0].place == bm.pages[0].place {
		t.Fatal("the write fault left the guest on the page it shares")
	}
	return f, a, am, ab, b, bm
}

// giveBack is one pass over a memory region with room for every copy it holds.
func giveBack(t *testing.T, r *vmmemory.MemoryRegion) int {
	t.Helper()
	given, err := r.GiveBack(t.Context(), 64)
	if err != nil {
		t.Fatalf("giving back: %v", err)
	}
	return given
}

// A copy the guest never stored into goes back with no checkpoint at all: the
// guest maps the page it was copied from again, and the host holds the page
// once.
func TestAnUnchangedCopyIsGivenBackWithoutACheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, _, _, bm := sharedCopy(t)
		maps, revokes, protects := am.maps, am.revokes, am.protects
		if given := giveBack(t, a); given != 1 {
			t.Fatalf("the give-back gave back %d pages, want exactly one", given)
		}
		if am.pages[0].place != bm.pages[0].place {
			t.Fatal("the guest does not map its sibling's page again")
		}
		if am.pages[0].writable {
			t.Fatal("the page given back is mapped writable, want read-only")
		}
		if am.protects != protects+1 || am.maps != maps+1 || am.revokes != revokes {
			t.Fatalf("the give-back issued %d protections, %d maps and %d revocations, want 1, 1 and 0",
				am.protects-protects, am.maps-maps, am.revokes-revokes)
		}
		stats := hostStats(t, f)
		if stats.GiveBackCompares != 1 || stats.GivenBackPages != 1 || stats.DirtyPages != 0 {
			t.Fatalf("compared %d, gave back %d, holds %d dirty reservations; want 1, 1 and 0",
				stats.GiveBackCompares, stats.GivenBackPages, stats.DirtyPages)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "after the give-back")
		wantMemoryRegion(t, a, 4, 0, 4, "the memory region that gave its copy back")
		if stats, err := a.Stats(t.Context()); err != nil || !stats.DirtySince.IsZero() {
			t.Fatalf("the memory region still holds a write since %v: %v", stats.DirtySince, err)
		}
		// Nothing is left to compare, so the next pass does nothing.
		if given := giveBack(t, a); given != 0 || hostStats(t, f).GiveBackCompares != 1 {
			t.Fatalf("a second pass gave back %d and compared again", given)
		}
	})
}

// The guest is pointed at the origin in place and the page is installed, so its
// next read maps it without a fault. That read is the one that arrives as a
// write where KVM asks for every page writable, and a page that was revoked
// instead would be copied again by it, and given back again, at every pass.
func TestAGuestReadAfterAGiveBackMakesNoNewCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, _, _, _ := sharedCopy(t)
		giveBack(t, a)
		before := hostStats(t, f)
		if got := access(t, a, am, 0, false)[0]; got != 1 {
			t.Fatalf("the page given back reads %d, want the byte the volume holds", got)
		}
		after := hostStats(t, f)
		if after.Faults != before.Faults || after.CopyOnWrites != before.CopyOnWrites {
			t.Fatalf("the read took %d faults and made %d copies, want none",
				after.Faults-before.Faults, after.CopyOnWrites-before.CopyOnWrites)
		}
	})
}

// A store that lands in the copy just before the write-protection is what the
// comparison is for: the bytes differ, so the copy is kept, the protection comes
// off at once, and the store is not lost. The copy forgets its origin, so it is
// never compared again.
func TestACopyWrittenDuringTheCompareIsKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, ab, b, bm := sharedCopy(t)
		copied := am.pages[0].place
		am.onProtect = func(uint64, int) {
			am.onProtect = nil
			am.arena.page(copied)[0] = 99
		}
		if given := giveBack(t, a); given != 0 {
			t.Fatalf("the give-back gave back %d pages the guest stored into, want none", given)
		}
		if p := am.pages[0]; p.place != copied || !p.writable {
			t.Fatalf("the guest maps %+v, want its own copy writable again", p)
		}
		faults := hostStats(t, f).Faults
		access(t, a, am, 0, true)[1] = 98
		if got := hostStats(t, f).Faults; got != faults {
			t.Fatalf("a store after the comparison took %d faults, want none", got-faults)
		}
		if got := access(t, b, bm, 0, false); got[0] != 1 {
			t.Fatalf("the sibling reads %d, want the byte the volume holds", got[0])
		}
		if given := giveBack(t, a); given != 0 || hostStats(t, f).GiveBackCompares != 1 {
			t.Fatalf("a second pass gave back %d and compared the changed copy again", given)
		}
		f.mustCheckpoint(a, ab)
		if ab.data[0] != 99 || ab.data[1] != 98 {
			t.Fatalf("the volume holds %d and %d, want the 99 and 98 the guest stored", ab.data[0], ab.data[1])
		}
	})
}

// A store that arrives after the write-protection traps and waits for the
// page's window, which the give-back holds. The copy's bytes are still the
// origin's, so it goes back; the store is then served against the origin and
// copies again, and what it wrote is what the guest reads.
func TestAStoreThatTrapsDuringTheCompareIsNotLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, ab, b, bm := sharedCopy(t)
		stored := make(chan error, 1)
		am.onProtect = func(uint64, int) {
			am.onProtect = nil
			go func() { stored <- a.Fault(t.Context(), 0, true) }()
			synctest.Wait()
			select {
			case err := <-stored:
				t.Errorf("the store was served while the give-back held its page: %v", err)
			default:
			}
		}
		copies := hostStats(t, f).CopyOnWrites
		if given := giveBack(t, a); given != 1 {
			t.Fatalf("the give-back gave back %d pages, want exactly one", given)
		}
		if err := <-stored; err != nil {
			t.Fatal(err)
		}
		if got := hostStats(t, f).CopyOnWrites; got != copies+1 {
			t.Fatalf("the trapped store made %d copies, want exactly one", got-copies)
		}
		faults := hostStats(t, f).Faults
		access(t, a, am, 0, true)[0] = 42
		if got := hostStats(t, f).Faults; got != faults {
			t.Fatalf("the store took %d more faults once served, want none", got-faults)
		}
		if got := access(t, b, bm, 0, false)[0]; got != 1 {
			t.Fatalf("the sibling reads %d, want the byte the volume holds", got)
		}
		f.mustCheckpoint(a, ab)
		if ab.data[0] != 42 {
			t.Fatalf("the volume holds %d, want the 42 the guest stored", ab.data[0])
		}
	})
}

// A client out of mapping budget refuses the command that would point the
// guest at the origin, and changes nothing. The guest keeps its copy, writable
// again, and the next pass gives it back.
func TestACopyWhoseMappingIsRefusedIsGivenBackByTheNextPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, _, _, bm := sharedCopy(t)
		copied := am.pages[0].place
		am.refuseMap = true
		if given := giveBack(t, a); given != 0 {
			t.Fatalf("the give-back gave back %d pages with its mapping refused, want none", given)
		}
		if p := am.pages[0]; p.place != copied || !p.writable {
			t.Fatalf("the guest maps %+v, want its own copy writable again", p)
		}
		am.refuseMap = false
		if given := giveBack(t, a); given != 1 {
			t.Fatalf("the next pass gave back %d pages, want exactly one", given)
		}
		if am.pages[0].place != bm.pages[0].place {
			t.Fatal("the guest does not map its sibling's page again")
		}
		if got := hostStats(t, f).GiveBackCompares; got != 2 {
			t.Fatalf("the two passes compared %d times, want 2", got)
		}
	})
}

// The origin is an ordinary resident page, and a pager short of slots takes
// it. A copy whose origin has gone has nothing to be compared with, then or
// ever, so it stays the guest's and is not looked at again.
func TestACopyWhoseOriginWasEvictedIsNotGivenBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 32, 8)
		a, am, _ := f.memoryRegion(4)
		access(t, a, am, 0, false)
		access(t, a, am, 0, true)
		c, cm := f.attach(f.newUnrelatedBacking(2))
		access(t, c, cm, 0, false)
		if given := giveBack(t, a); given != 0 {
			t.Fatalf("the give-back gave back %d pages whose origin was evicted, want none", given)
		}
		if got := hostStats(t, f).GiveBackCompares; got != 0 {
			t.Fatalf("the give-back compared %d pages, want none", got)
		}
		wantMemoryRegion(t, a, 1, 1, 0, "the memory region that keeps its copy")
	})
}

// A sealed page belongs to the checkpoint that froze it, and the settle behind
// that checkpoint is what compares it.
func TestASealedCopyIsLeftToTheSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, _, _, _, _ := sharedCopy(t)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if given := giveBack(t, a); given != 0 || hostStats(t, f).GiveBackCompares != 0 {
			t.Fatalf("the give-back gave back %d sealed pages and compared %d, want none",
				given, hostStats(t, f).GiveBackCompares)
		}
		if unchanged := f.settle(a); unchanged != 1 {
			t.Fatalf("the settle found %d unchanged pages, want exactly one", unchanged)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}

// A pass compares no more copies than it is allowed, and the next one starts
// where it stopped rather than at the first page again.
func TestAGiveBackPassIsBoundedAndTheNextOneGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 16, 32, 8)
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		for _, page := range []uint64{0, 2, 3} {
			access(t, a, am, page, true)
		}
		for pass, want := range []uint64{0, 2, 3} {
			given, err := a.GiveBack(t.Context(), 1)
			if err != nil || given != 1 {
				t.Fatalf("pass %d gave back %d pages: %v; want exactly one", pass, given, err)
			}
			if am.pages[want].place != bm.pages[want].place {
				t.Fatalf("pass %d did not give back page %d", pass, want)
			}
		}
		if s := hostStats(t, f); s.GivenBackPages != 3 || s.GiveBackCompares != 3 {
			t.Fatalf("gave back %d and compared %d, want 3 and 3", s.GivenBackPages, s.GiveBackCompares)
		}
	})
}

// coldCopy is two memory regions of kind mapping one checkpoint's four pages,
// the second of which reads page 0 and the first of which then takes it
// writable without having mapped it: the store trap KVM's async fault worker
// makes of a guest's cold read.
func coldCopy(t *testing.T, kind vmmemory.MemoryRegionKind) (f *fixture, a *vmmemory.MemoryRegion, am *mapping,
	bm *mapping) {
	t.Helper()
	f = newFixture(t, 8, 32, 8)
	a, am = f.attachKind(kind, f.newBacking(4))
	b, bm := f.attachKind(kind, f.newBacking(4))
	access(t, b, bm, 0, false)
	access(t, a, am, 0, true)
	if am.pages[0].place == bm.pages[0].place {
		t.Fatal("the store trap left the guest on the page it shares")
	}
	if s := hostStats(t, f); s.UnmappedCopyOnWrites != 1 {
		t.Fatalf("the store trap made %d copies of a page the guest did not map, want one", s.UnmappedCopyOnWrites)
	}
	return f, a, am, bm
}

// giveBackColdCopies gives back the cold copies one memory region recorded.
func giveBackColdCopies(t *testing.T, r *vmmemory.MemoryRegion) int {
	t.Helper()
	given, err := r.GiveBackColdCopies(t.Context())
	if err != nil {
		t.Fatalf("giving back cold copies: %v", err)
	}
	return given
}

// A cold copy the guest never stored into goes back as soon as the session asks,
// with no interval and no checkpoint, whatever kind of memory it is: a disk's
// copy is no more the guest's state than a RAM one's.
func TestAColdCopyIsGivenBackAtOnce(t *testing.T) {
	for _, kind := range []vmmemory.MemoryRegionKind{vmmemory.Ram, vmmemory.Pmem} {
		t.Run(kind.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, a, am, bm := coldCopy(t, kind)
				if given := giveBackColdCopies(t, a); given != 1 {
					t.Fatalf("gave back %d cold copies, want exactly one", given)
				}
				if am.pages[0].place != bm.pages[0].place || am.pages[0].writable {
					t.Fatalf("the guest maps %+v, want its sibling's page read-only", am.pages[0])
				}
				stats := hostStats(t, f)
				if stats.GiveBackCompares != 1 || stats.GivenBackPages != 1 || stats.DirtyPages != 0 {
					t.Fatalf("compared %d, gave back %d, holds %d dirty reservations; want 1, 1 and 0",
						stats.GiveBackCompares, stats.GivenBackPages, stats.DirtyPages)
				}
				// Each cold copy is given back once: nothing is left to take.
				if given := giveBackColdCopies(t, a); given != 0 || hostStats(t, f).GiveBackCompares != 1 {
					t.Fatalf("a second call gave back %d and compared again", given)
				}
			})
		})
	}
}

// A cold copy that was a real store is compared once and kept, writable, with
// what the guest stored in it.
func TestAColdCopyTheGuestStoredIntoIsKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, am, bm := coldCopy(t, vmmemory.Ram)
		copied := am.pages[0].place
		am.arena.page(copied)[0] = 77
		if given := giveBackColdCopies(t, a); given != 0 {
			t.Fatalf("gave back %d cold copies the guest stored into, want none", given)
		}
		if p := am.pages[0]; p.place != copied || !p.writable {
			t.Fatalf("the guest maps %+v, want its own copy writable again", p)
		}
		if got := access(t, a, am, 0, false)[0]; got != 77 {
			t.Fatalf("the guest reads %d, want the 77 it stored", got)
		}
		if got := bm.arena.page(bm.pages[0].place)[0]; got == 77 {
			t.Fatal("the store reached the sibling's page")
		}
		if s := hostStats(t, f); s.GiveBackCompares != 1 || s.GivenBackPages != 0 {
			t.Fatalf("compared %d and gave back %d, want 1 and 0", s.GiveBackCompares, s.GivenBackPages)
		}
	})
}

// A copy a protect trap made is a store into a page the guest mapped, which KVM
// asks for only when the guest stores, so it is not a cold copy and is left to
// the interval.
func TestACopyOfAMappedPageIsNotAColdCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, _, _, _, _ := sharedCopy(t)
		if given := giveBackColdCopies(t, a); given != 0 {
			t.Fatalf("gave back %d copies of a page the guest mapped, want none", given)
		}
		if s := hostStats(t, f); s.GiveBackCompares != 0 {
			t.Fatalf("compared %d copies, want none", s.GiveBackCompares)
		}
	})
}
