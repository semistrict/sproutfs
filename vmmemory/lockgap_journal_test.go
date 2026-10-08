package vmmemory_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A capture write-protects only the pages the guest still maps when it holds
// the region's protection. An eviction takes a page's mapping away under that
// page's lock and the protection shared, not the region, so one between the
// capture taking its pages and its protection leaves a page the guest no
// longer maps; a write-protect of it is refused, and the region was terminal.
// The unscheduled soak found it.
func TestACaptureProtectsNoPageAnEvictionUnmappedAfterItTookThePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0xee)
		evicted := false
		vmmemory.SetCaptureSeam(t, func(taken []uint64) {
			if evicted || !slices.Contains(taken, 0) {
				return
			}
			evicted = true
			went, err := vmmemory.EvictPage(f.ctx, r, 0)
			if err != nil || !went {
				t.Errorf("evicting page 0 under the capture = %v, %v; want it gone", went, err)
			}
		})
		c, err := r.Capture(f.ctx, r.Unjournaled())
		if err != nil {
			t.Fatalf("a capture beside an eviction of its page = %v, want it to take the page", err)
		}
		if want := []uint64{blockOf(0, 0)}; !slices.Equal(c.Blocks, want) || c.Data[0] != 0xee {
			t.Fatalf("the capture took blocks %v starting %#x, want %v starting 0xee", c.Blocks, c.Data[0], want)
		}
		f.storeAt(r, m, 0, 1, 0xef)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after a store into the evicted page, want [0]", got)
		}
		writableIsUnjournaled(t, r, m)
	})
}

// A capture holds the region exclusively and reads the page a dirty page was
// copied from, to keep only the blocks the guest changed. A fault that holds
// that page, a root's, in its plan gives the region up for its read and takes
// it again before it lets the page go. So the capture never waits for the
// origin's lock: where something holds it, the capture writes the whole page.
// Before 2026-10-08 it waited, and the two waited for each other; the GCE
// soak's rules world hung there.
func TestACaptureNeverWaitsForTheLockOfThePageItsCopyWasMadeFrom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		sibling, sm, _ := f.pmemRegion(2)
		r, m, _ := f.pmemRegion(2)
		// The sibling reads page 0 in, and the region copies it away: the
		// copy's origin is the root's page the sibling maps.
		accessUnder(f.ctx, t, sibling, sm, 0, false)
		f.storeAt(r, m, 0, 0, 0xee)
		release, err := vmmemory.HoldPage(f.ctx, sibling, 0)
		if err != nil {
			t.Fatal(err)
		}
		var c *vmmemory.Captured
		var captured error
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, captured = r.Capture(f.ctx, r.Unjournaled())
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			release()
			<-done
			t.Fatal("the capture waited for the lock of the page its copy was made from")
		}
		release()
		if captured != nil {
			t.Fatal(captured)
		}
		want := make([]uint64, blocksPerPage())
		for i := range want {
			want[i] = blockOf(0, uint64(i*vmmemory.BlockBytes))
		}
		if !slices.Equal(c.Blocks, want) || c.Data[0] != 0xee {
			t.Fatalf("the capture took blocks %v starting %#x, want the whole page %v starting 0xee",
				c.Blocks, c.Data[0], want)
		}
	})
}

// A settle reshares a checkpoint's copy onto the root's page it was made
// from with the region held exclusively, and a prefetch may hold that page
// waiting for a window's stripe that a fault holds waiting for the region.
// So the reshare never waits for the origin's lock: where something holds
// it, the copy stays in the checkpoint.
func TestASettleNeverWaitsForTheLockOfThePageItsCopyWasMadeFrom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8,
			ReadAheadPages: 1, SettleWorkers: 1})
		sibling, sm, _ := f.memoryRegion(2)
		r, m, b := f.memoryRegion(2)
		// The sibling reads page 0 in, and the guest reads it and then takes
		// it writable, storing nothing: its copy is unchanged from the root's
		// page, and not cold, so the seal keeps it for the settle.
		access(t, sibling, sm, 0, false)
		access(t, r, m, 0, false)
		access(t, r, m, 0, true)
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		var release func()
		vmmemory.SetSettleComparedSeam(t, func() {
			var err error
			if release, err = vmmemory.HoldPage(f.ctx, sibling, 0); err != nil {
				t.Error(err)
			}
		})
		var unchanged int
		var settled error
		done := make(chan struct{})
		go func() {
			defer close(done)
			unchanged, settled = r.Checkpoint().Settle(f.ctx)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			release()
			<-done
			t.Fatal("the settle waited for the lock of the page its copy was made from")
		}
		release()
		if settled != nil || unchanged != 0 {
			t.Fatalf("the settle reshared %d pages and returned %v, want the copy kept and no error", unchanged, settled)
		}
		f.finishCheckpoint(r, b)
	})
}
