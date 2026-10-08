package vmmemory_test

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The tests in this file put another goroutine's work in a gap where a fault
// or an eviction gives a lock up and takes it again (TASK-108), and require
// that what the code decided before the gap still holds after it.

// A refault reads a spilled page's protection, then gives the region up to
// take a slot for it. A capture of the disk taken in that gap journals the
// page and write-protects it. The refault maps it read-only, as the capture
// left it: mapped writable, the guest's next store would land without a fault,
// unjournaled by nothing, and the next flush would leave it out.
func TestARefaultMapsAPageACaptureProtectedWhileItReclaimedReadOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 8)
		r, m, _ := f.pmemRegion(4)
		f.storeAt(r, m, 0, 3, 0xa1)
		for page := uint64(1); page < 4; page++ {
			accessUnder(f.ctx, t, r, m, page, false)
		}
		if _, mapped := m.pages[0]; mapped {
			t.Fatal("page 0 is still mapped, so nothing below is a refault")
		}
		var captured []uint64
		var once sync.Once
		vmmemory.SetReclaimSeam(t, func(index uint64) {
			if index != 0 {
				return
			}
			once.Do(func() {
				c, err := r.Capture(f.ctx, r.Unjournaled())
				if err != nil {
					t.Errorf("capturing while the refault reclaimed: %v", err)
					return
				}
				captured = c.Pages()
			})
		})
		if got, err := memoryByte(f.ctx, r, m, 0, nil); err != nil || got != 1 {
			t.Fatalf("the refaulted page reads %d, want 1: %v", got, err)
		}
		if !slices.Equal(captured, []uint64{0}) {
			t.Fatalf("the capture in the refault's reclaim took pages %v, want [0]", captured)
		}
		writableIsUnjournaled(t, r, m)
		if p := m.pages[0]; p.writable {
			t.Fatalf("the refault mapped page 0 %+v writable, want read-only as the capture protected it", p)
		}
		f.storeAt(r, m, 0, 4, 0xa2)
		writableIsUnjournaled(t, r, m)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after storing into the refaulted page, want [0]", got)
		}
	})
}

// A seal leaves out of its set each cold copy the guest did not change, one
// at a time, and gives those back to the region's dirty set once it has
// compared them all. A comparison that fails partway, here a spilled copy
// whose read fails, leaves the copies it had already left out in the set, so
// the seal takes them as dirty pages. Dropped from both, a copy the guest
// stores into later is dirty and in no set, and no checkpoint ever holds the
// store.
func TestASealWhoseColdCopyCompareFailsKeepsTheCopiesItComparedBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 8)
		r, m, b := f.memoryRegion(4)
		// Store traps on pages the guest does not map make cold copies, which
		// hold their origins' bytes: page 1's first, so it is the older.
		for _, page := range []uint64{1, 0} {
			same := byte(page + 1)
			if _, err := memoryByte(f.ctx, r, m, page, &same); err != nil {
				t.Fatal(err)
			}
		}
		// The arena holds both copies and both origins; a read of page 2
		// spills the older copy.
		if _, err := memoryByte(f.ctx, r, m, 2, nil); err != nil {
			t.Fatal(err)
		}
		if _, mapped := m.pages[1]; mapped || hostStats(t, f).Spills != 1 {
			t.Fatalf("page 1 mapped %t after %d spills, want its copy spilled alone", mapped, hostStats(t, f).Spills)
		}
		// The seal compares page 0's copy with its origin, then fails to read
		// page 1's back from the spill.
		f.disk.FailNext(sim.DiskRead, 1)
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		f.finishCheckpoint(r, b)
		stored := byte(0x77)
		if _, err := memoryByte(f.ctx, r, m, 0, &stored); err != nil {
			t.Fatal(err)
		}
		f.mustCheckpoint(r, b)
		if got := b.data[0]; got != stored {
			t.Fatalf("the checkpoint after the store published %d for page 0, want %d", got, stored)
		}
	})
}

// A store that needs a place of its own in an isolated arena, where both of
// its page's places are taken, gives up the one that holds an idle published
// page. It chooses that page under the host's lock and takes the page's lock
// after. A settle in between finds the checkpoint's copy unchanged and hands
// the region back that very page, and an eviction's look ages it old enough to
// give up. The store must see that the page is mapped again and take the
// place the settle freed: given up, the pager frees a page a region names, and
// panics.
func TestAStoreGivesUpNoOwnPlaceASettleHandedBackWhileItLocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		r, m, b := f.memoryRegion(4)
		first := byte(99)
		if _, err := memoryByte(f.ctx, r, m, 0, &first); err != nil {
			t.Fatal(err)
		}
		f.mustCheckpoint(r, b)
		// A store of the same byte copies the published page into the page's
		// other place, and the published page in its home place goes idle.
		if _, err := memoryByte(f.ctx, r, m, 0, &first); err != nil {
			t.Fatal(err)
		}
		if p := m.pages[0]; m.number(0) != 0 || p.slot != 4 || !p.writable {
			t.Fatalf("the second store maps %+v as file %d, want slot 4 of the region's own file 0, writable", p, m.number(0))
		}
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		settled := 0
		var once sync.Once
		vmmemory.SetAllocateOwnSeam(t, func(index uint64) {
			if index != 0 {
				return
			}
			once.Do(func() {
				var err error
				if settled, err = r.Checkpoint().Settle(f.ctx); err != nil {
					t.Errorf("settling while the store reclaimed: %v", err)
				}
				f.h.AgeEveryPage()
			})
		})
		// The store runs on a goroutine of its own, so that a pager that frees
		// a page a region names panics there and ends the run at once, rather
		// than leaving the region's locks held for the cleanup to wait on.
		next := byte(100)
		var stored error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, stored = memoryByte(f.ctx, r, m, 0, &next)
		}()
		<-done
		if stored != nil {
			t.Fatal(stored)
		}
		if settled != 1 {
			t.Fatalf("the settle in the store's reclaim dropped %d pages, want the unchanged copy", settled)
		}
		f.finishCheckpoint(r, b)
		if p := m.pages[0]; m.number(0) != 0 || p.slot != 4 || !p.writable {
			t.Fatalf("the store maps %+v as file %d, want the place the settle freed, slot 4 of file 0, writable",
				p, m.number(0))
		}
		if got, err := memoryByte(f.ctx, r, m, 0, nil); err != nil || got != next {
			t.Fatalf("page 0 reads %d after the store, want %d: %v", got, next, err)
		}
	})
}

// A fault on a page that is the region's own state, bound and not mapped, as
// an unseal leaves it, gives the page's lock back once it has found nothing
// to complete, and looks the page up. An eviction in between spills the
// page: the layer holds nothing there, and only the page's reservation holds
// its bytes. The lookup must send the fault back to refault it. Going on to
// the volume, the fault read the bytes the guest had stored over, and the
// store was lost (the unscheduled soak, as an invalid resolution).
func TestAFaultRefaultsItsOwnPageAnEvictionSpilledBeforeItsLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8,
			ReadAheadPages: 1})
		r, m, _ := f.memoryRegion(4)
		stored := byte(77)
		if _, err := memoryByte(f.ctx, r, m, 0, &stored); err != nil {
			t.Fatal(err)
		}
		// An unseal gives the guest its page back as dirty state, unmapped.
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if _, mapped := m.pages[0]; mapped {
			t.Fatal("page 0 is mapped after the unseal, want it revoked")
		}
		evicted := false
		vmmemory.SetLoadUnboundSeam(t, func(index uint64) {
			if index != 0 || evicted {
				return
			}
			evicted = true
			went, err := vmmemory.EvictPage(f.ctx, r, 0)
			if err != nil || !went {
				t.Errorf("evicting page 0 before the fault's lookup = %t, %v; want it spilled", went, err)
			}
		})
		got, err := memoryByte(f.ctx, r, m, 0, nil)
		if err != nil || got != stored {
			t.Fatalf("page 0 reads %d after its eviction, want the %d the guest stored: %v", got, stored, err)
		}
		if !evicted {
			t.Fatal("the fault never looked its unmapped page up")
		}
	})
}

// A refault of a spilled page looks for a place of its own in an isolated
// arena. One place holds the checkpoint's copy, and the other the page an
// eviction is taking: its aliases are off but its slot is not back yet.
// Nothing maps that page and it is no root's, so it is neither mapped nor
// idle. The refault must wait for the eviction and take the place it frees:
// counting the page as mapped, it reported both places taken, and the guest's
// session ended with ErrCapacity (the unscheduled soak).
func TestARefaultWaitsForAnEvictionThatHasNotGivenItsPlaceBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		r, m, b := f.memoryRegion(4)
		first := byte(99)
		if _, err := memoryByte(f.ctx, r, m, 0, &first); err != nil {
			t.Fatal(err)
		}
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		// The store copies away from the checkpoint's copy, in the page's own
		// place, into its other place.
		next := byte(100)
		if _, err := memoryByte(f.ctx, r, m, 0, &next); err != nil {
			t.Fatal(err)
		}
		if p := m.pages[0]; m.number(0) != 0 || p.slot != 4 || !p.writable {
			t.Fatalf("the store maps %+v as file %d, want slot 4 of the region's own file 0, writable", p, m.number(0))
		}
		// The refault reads the page's bytes back from its reservation, and
		// then reclaims a place with the eviction still under way.
		reclaiming := make(chan struct{})
		vmmemory.SetReclaimSeam(t, func(index uint64) {
			if index == 0 {
				close(reclaiming)
			}
		})
		var read byte
		var readErr error
		var refault sync.WaitGroup
		vmmemory.SetEvictedSeam(t, func(slot int) {
			if slot != 4 {
				return
			}
			refault.Go(func() { read, readErr = memoryByte(f.ctx, r, m, 0, nil) })
			<-reclaiming
			synctest.Wait()
		})
		if evicted, err := vmmemory.EvictPage(f.ctx, r, 0); err != nil || !evicted {
			t.Fatalf("evicting page 0 = %t, %v; want it gone", evicted, err)
		}
		refault.Wait()
		if readErr != nil || read != next {
			t.Fatalf("page 0 reads %d after the eviction, want %d: %v", read, next, readErr)
		}
		if p := m.pages[0]; m.number(0) != 0 || p.slot != 4 {
			t.Fatalf("the refault maps %+v as file %d, want the place the eviction freed, slot 4 of file 0", p, m.number(0))
		}
		f.finishCheckpoint(r, b)
	})
}

// A lookup's resolver finds a root holding the page under the root's lock,
// lets it go, and the lookup locks the root again to go down into it. An idle
// drop in between gives the page up. Going on down, the lookup sent a READ to
// the root's source with no h.mu held, so it could land inside another fork's
// prefetch, between its look for the reads under way and its send, and the
// prefetch's request met it: the pager panicked, as TASK-105 did by another
// path. The lookup looks again under the root's lock and asks its own source.
func TestALookupAsksNoRootThatGaveItsPageUpBeforeItWentDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		c, cm := f.attach(f.slowBacking(8))
		a, am := f.attach(f.slowBacking(8))
		b, bm := f.attach(f.slowBacking(8))
		// A third fork reads the window in and goes: every page of it is
		// idle in the forks' root.
		if err := c.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := c.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		requirePage(t, cm, 4)
		clear(cm.pages)
		if err := c.Detach(f.ctx); err != nil {
			t.Fatal(err)
		}
		located, checked, sent := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var locating, checking, sending atomic.Bool
		vmmemory.SetLocateSeam(t, func(page uint64) {
			if page != 4 || !locating.CompareAndSwap(false, true) {
				return
			}
			// The root's page goes before the lookup goes down into it.
			if _, err := f.h.DropIdle(f.ctx); err != nil {
				t.Error(err)
			}
			close(located)
			<-checked
		})
		vmmemory.SetPrefetchCheckedSeam(t, func(uint64) {
			if !checking.CompareAndSwap(false, true) {
				return
			}
			close(checked)
			<-sent
		})
		vmmemory.SetLookupSentSeam(t, func(page uint64) {
			if page == 4 && sending.CompareAndSwap(false, true) {
				close(sent)
			}
		})
		// Each fault runs on a goroutine of its own, so that a pager whose
		// requests meet panics there and ends the run at once.
		faulted := make(chan error, 2)
		go func() { faulted <- a.Fault(f.ctx, 4, false) }()
		<-located
		go func() { faulted <- b.Fault(f.ctx, 0, false) }()
		for range 2 {
			if err := <-faulted; err != nil {
				t.Fatal(err)
			}
		}
		if !sending.Load() {
			t.Fatal("the lookup of page 4 sent no READ request")
		}
		for _, r := range []*vmmemory.MemoryRegion{a, b} {
			if err := r.SettlePrefetches(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
		requirePage(t, am, 4)
		requirePage(t, bm, 0)
	})
}

// A child's lookup finds a page in the root its parent's fork point lends,
// and the point's seal ends before the lookup goes down into the root: the
// root leaves Host.roots and its pages go. Going on down, the lookup sent a
// READ to the source of a root no longer there, and the pager panicked. The
// lookup looks again under the root's lock and reads the page from its own
// backing, as every child does once the seal has ended.
func TestALookupAsksNoLentRootWhoseSealEndedBeforeItWentDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		parent, pm, _ := f.memoryRegion(4)
		stored := byte(44)
		if _, err := memoryByte(f.ctx, parent, pm, 0, &stored); err != nil {
			t.Fatal(err)
		}
		// The child attaches before the point lends anything, so it maps
		// nothing at attach and its first access is a fault.
		point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
		cb := f.newBacking(4)
		cb.source = point
		child, cm := f.attach(cb)
		if err := parent.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint().Share(f.ctx, point, "v"); err != nil {
			t.Fatal(err)
		}
		var ended atomic.Bool
		vmmemory.SetLocateSeam(t, func(page uint64) {
			if page != 0 || !ended.CompareAndSwap(false, true) {
				return
			}
			// The point's seal ends before the lookup goes down into its root.
			if err := parent.Checkpoint().Retire(f.ctx, false); err != nil {
				t.Error(err)
			}
		})
		// The fault runs on a goroutine of its own, so that a pager that
		// panics ends the run at once.
		var got byte
		var faultErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, faultErr = memoryByte(f.ctx, child, cm, 0, nil)
		}()
		<-done
		if faultErr != nil {
			t.Fatal(faultErr)
		}
		if !ended.Load() {
			t.Fatal("the child's lookup found no page in the lent root")
		}
		if want := cb.data[0]; got != want {
			t.Fatalf("the child reads %d after the point's seal ended, want its backing's %d", got, want)
		}
		if got, err := memoryByte(f.ctx, parent, pm, 0, nil); err != nil || got != stored {
			t.Fatalf("the parent reads %d after its seal ended, want its own %d: %v", got, stored, err)
		}
	})
}
