package vmmemory_test

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"

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
