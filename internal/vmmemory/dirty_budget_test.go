package vmmemory_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The dirty budget is the host's, so the wait for one is too: a store into any
// region waits for the reservations a checkpoint of any other region is about
// to release. Failing it instead fails the fault, which closes the session and
// kills the VMM of a guest that only had to wait.
func TestDirtyBudgetWaitsForAnotherRegionsCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 2)
		a, am, ab := f.region(2)
		b, bm, _ := f.region(2)
		access(t, a, am, 0, true)[0] = 11
		access(t, a, am, 1, true)[0] = 12
		// The whole budget is A's checkpoint's now, and only its publication
		// will give it back.
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		stored := make(chan error, 1)
		go func() { stored <- b.Fault(t.Context(), 0, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("the store did not wait for the draining checkpoint: %v", err)
		default:
		}
		f.finishCheckpoint(a, ab)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the checkpoint retired its pages: %v", err)
		}
		if access(t, b, bm, 0, false)[0] != 1 {
			t.Fatal("the admitted store lost the page's inherited bytes")
		}
	})
}

// With nothing draining, a full budget is not a dead end either: the host asks
// its supervisor for an immediate checkpoint of the region holding the largest
// dirty set, out of the interval's turn, and the store lands when that
// checkpoint does.
func TestDirtyBudgetRequestsACheckpointOfTheLargestDirtyRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 6, 12, 3)
		a, am, ab := f.region(2)
		b, bm, _ := f.region(2)
		access(t, a, am, 0, true)[0] = 11
		access(t, a, am, 1, true)[0] = 12
		access(t, b, bm, 0, true)[0] = 21
		// Every reservation is a live private frame and no checkpoint is in
		// flight, so only a new one can admit this store.
		requested := make(chan *vmmemory.Region, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(r *vmmemory.Region) bool {
			select {
			case requested <- r:
			default:
			}
			return true
		}})
		stored := make(chan error, 1)
		go func() { stored <- b.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("the store did not wait for the checkpoint it asked for: %v", err)
		default:
		}
		if got := <-requested; got != a {
			t.Fatal("the host asked to checkpoint a region other than the largest dirty one")
		}
		f.mustCheckpoint(a, ab)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the requested checkpoint landed: %v", err)
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyPages != 2 {
			t.Fatalf("%d dirty pages after the checkpoint, want the two stores it did not take: %v", s.DirtyPages, err)
		}
	})
}

// A fork point is a seal held open until the children it named have the pages
// they inherited, so it releases no reservation a waiting store may wait for.
// It must not answer for the whole host: a store in another region still gets
// the checkpoint of its own that admits it.
func TestForkHoldDoesNotAnswerAnotherRegionsPressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 3)
		a, am, _ := f.region(2)
		b, bm, bb := f.region(3)
		access(t, a, am, 0, true)[0] = 11
		access(t, b, bm, 0, true)[0] = 21
		access(t, b, bm, 1, true)[0] = 22
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Naming the sealed frames is what a fork instant does; from here the
		// seal lasts as long as the children, not as long as an upload.
		if err := a.Checkpoint().Share(t.Context(), control.Ref{VM: "child", Sequence: 1}, "ram0"); err != nil {
			t.Fatal(err)
		}
		requested := make(chan *vmmemory.Region, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(region *vmmemory.Region) bool {
			select {
			case requested <- region:
			default:
			}
			return true
		}})
		stored := make(chan error, 1)
		go func() { stored <- b.Fault(t.Context(), 2, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("the store did not wait for a checkpoint: %v", err)
		default:
		}
		select {
		case got := <-requested:
			if got != b {
				t.Fatal("the host asked to checkpoint a region other than the one holding a dirty set it can release")
			}
		default:
			t.Fatal("the fork hold answered the pressure of another region, so no checkpoint was asked for")
		}
		f.mustCheckpoint(b, bb)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the requested checkpoint landed: %v", err)
		}
	})
}

// The high-water mark is what asks for a checkpoint before any store has to
// wait for one, so every page admitted to the dirty budget counts against it —
// including the pages write-ahead and a peer-served load take without waiting,
// which can carry the budget past the mark on their own.
func TestWriteAheadPastTheHighWaterMarkAsksForACheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := holeRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
			DirtyPages: 8, ReadAheadPages: 8, WriteAheadPages: 8}, 8)
		requested := make(chan *vmmemory.Region, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(region *vmmemory.Region) bool {
			select {
			case requested <- region:
			default:
			}
			return true
		}})
		// One store into fresh memory, which gives its whole run private frames
		// and takes a reservation for each: three quarters of this budget is six
		// pages and the run is eight.
		access(t, r, m, 0, true)[0] = 42
		if s := hostStats(t, f); s.DirtyPages != 8 {
			t.Fatalf("the write-ahead run holds %d reservations, want the 8 the budget has", s.DirtyPages)
		}
		select {
		case got := <-requested:
			if got != r {
				t.Fatalf("the host asked to checkpoint %v, want the region holding the dirty set", got)
			}
		default:
			t.Fatal("the write-ahead run carried the dirty budget past its high-water mark without asking for a checkpoint")
		}
		if s := hostStats(t, f); s.CheckpointRequests != 1 {
			t.Fatalf("the crossing asked for %d checkpoints, want exactly one", s.CheckpointRequests)
		}
	})
}

// Only a budget no checkpoint can relieve fails a store, and it fails it as a
// stall the supervisor stops the VM for, never as the capacity error the fault
// path turns into a killed VMM.
func TestDirtyBudgetStallIsADeliberateStopNotACapacityFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 4, 1)
		r, m, _ := f.region(2)
		var stoppedRegion *vmmemory.Region
		var stoppedCause error
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.Region) bool { return false },
			Stop: func(region *vmmemory.Region, cause error) {
				stoppedRegion, stoppedCause = region, cause
			},
		})
		access(t, r, m, 0, true)[0] = 33
		err := r.Fault(t.Context(), 1, true)
		if !errors.Is(err, vmmemory.ErrDirtyStalled) {
			t.Fatalf("the stalled store failed with %v, want a dirty-budget stall", err)
		}
		if errors.Is(err, vmmemory.ErrCapacity) {
			t.Fatal("a stalled store must not reach the fault path as capacity exhaustion")
		}
		if stoppedRegion != r || !errors.Is(stoppedCause, vmmemory.ErrDirtyStalled) {
			t.Fatalf("the host stopped %v for %v, want the stalled region", stoppedRegion, stoppedCause)
		}
	})
}

// A store waiting for the dirty budget wakes on any change to the host — a
// frame freed, a page adopted — and asks again what will relieve it. A seal
// takes the dirty set into the checkpoint page by page before it records the
// checkpoint on the region, and a waiter that asks in between finds a region
// with neither a draining checkpoint nor a dirty page: told that nothing will
// relieve it, it is stalled, and the guest is stopped for a checkpoint that was
// an instruction away from admitting it.
func TestAStoreWokenDuringASealIsNotStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 1)
		r, m, b := f.region(2)
		access(t, r, m, 0, true)[0] = 11
		var stalled atomic.Bool
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.Region) bool { return true },
			Stop:       func(*vmmemory.Region, error) { stalled.Store(true) },
		})
		// The whole budget is page 0's, so this store waits for the checkpoint
		// it asked for.
		stored := make(chan error, 1)
		go func() { stored <- r.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		vmmemory.SetSealSeam(t, func() {
			vmmemory.Signal(f.h)
			synctest.Wait()
		})
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if stalled.Load() {
			t.Fatal("a store woken during the seal was stalled, with the checkpoint that admits it being taken")
		}
		f.finishCheckpoint(r, b)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the checkpoint retired its pages: %v", err)
		}
	})
}
