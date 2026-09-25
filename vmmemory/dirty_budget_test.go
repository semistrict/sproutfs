package vmmemory_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The dirty budget is the host's, so the wait for one is too: a store into any
// memory region waits for the reservations a checkpoint of any other memory region is about
// to release. Failing it instead fails the fault, which closes the session and
// kills the VMM of a guest that only had to wait.
func TestDirtyBudgetWaitsForAnotherMemoryRegionsCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 2)
		a, am, ab := f.memoryRegion(2)
		b, bm, _ := f.memoryRegion(2)
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
// its supervisor for an immediate checkpoint of the memory region holding the largest
// dirty set, out of the interval's turn, and the store lands when that
// checkpoint does.
func TestDirtyBudgetRequestsACheckpointOfTheLargestDirtyMemoryRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 6, 12, 3)
		a, am, ab := f.memoryRegion(2)
		b, bm, _ := f.memoryRegion(2)
		access(t, a, am, 0, true)[0] = 11
		access(t, a, am, 1, true)[0] = 12
		access(t, b, bm, 0, true)[0] = 21
		// Every reservation is a live private page and no checkpoint is in
		// flight, so only a new one can admit this store.
		requested := make(chan *vmmemory.MemoryRegion, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(r *vmmemory.MemoryRegion) bool {
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
			t.Fatal("the host asked to checkpoint a memory region other than the largest dirty one")
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
// It must not answer for the whole host: a store in another memory region still gets
// the checkpoint of its own that admits it.
func TestForkHoldDoesNotAnswerAnotherMemoryRegionsPressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 3)
		a, am, _ := f.memoryRegion(2)
		b, bm, bb := f.memoryRegion(3)
		access(t, a, am, 0, true)[0] = 11
		access(t, b, bm, 0, true)[0] = 21
		access(t, b, bm, 1, true)[0] = 22
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Naming the sealed pages is what a fork point does; from here the
		// seal lasts as long as the children, not as long as an upload.
		if err := a.Checkpoint().Share(t.Context(), control.Ref{VM: "child", Sequence: 1}, "ram0"); err != nil {
			t.Fatal(err)
		}
		requested := make(chan *vmmemory.MemoryRegion, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(memoryRegion *vmmemory.MemoryRegion) bool {
			select {
			case requested <- memoryRegion:
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
				t.Fatal("the host asked to checkpoint a memory region other than the one holding a dirty set it can release")
			}
		default:
			t.Fatal("the fork hold answered the pressure of another memory region, so no checkpoint was asked for")
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
		f, r, m, _ := holeMemoryRegion(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 8,
			DirtyPages: 8, ReadAheadPages: 8, WriteAheadPages: 8}, 8)
		requested := make(chan *vmmemory.MemoryRegion, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(memoryRegion *vmmemory.MemoryRegion) bool {
			select {
			case requested <- memoryRegion:
			default:
			}
			return true
		}})
		// One store into fresh memory, which gives its whole run private pages
		// and takes a reservation for each: three quarters of this budget is six
		// pages and the run is eight.
		access(t, r, m, 0, true)[0] = 42
		if s := hostStats(t, f); s.DirtyPages != 8 {
			t.Fatalf("the write-ahead run holds %d reservations, want the 8 the budget has", s.DirtyPages)
		}
		select {
		case got := <-requested:
			if got != r {
				t.Fatalf("the host asked to checkpoint %v, want the memory region holding the dirty set", got)
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
		r, m, _ := f.memoryRegion(2)
		var stoppedMemoryRegion *vmmemory.MemoryRegion
		var stoppedCause error
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return false },
			Stop: func(memoryRegion *vmmemory.MemoryRegion, cause error) bool {
				stoppedMemoryRegion, stoppedCause = memoryRegion, cause
				return true
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
		if stoppedMemoryRegion != r || !errors.Is(stoppedCause, vmmemory.ErrDirtyStalled) {
			t.Fatalf("the host stopped %v for %v, want the stalled memory region", stoppedMemoryRegion, stoppedCause)
		}
	})
}

// A full budget that no checkpoint can relieve is taken back from the memory
// region that holds the most of it, not from whichever store came last. A guest
// that stores into all of its RAM holds most of a RAM budget, and no checkpoint
// the interval takes gives RAM back. So its neighbour's next fresh store finds
// the budget full. The neighbour must not be the guest stopped for it: its
// store waits while the owner stops the hog, and lands once the hog's pages are
// back.
func TestAFullBudgetIsTakenBackFromItsLargestHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 4)
		hog, hm, _ := f.memoryRegion(4)
		calm, cm, _ := f.memoryRegion(4)
		access(t, hog, hm, 0, true)[0] = 11
		access(t, hog, hm, 1, true)[0] = 12
		access(t, hog, hm, 2, true)[0] = 13
		access(t, calm, cm, 0, true)[0] = 21
		type stop struct {
			memoryRegion *vmmemory.MemoryRegion
			cause        error
		}
		stops := make(chan stop, 4)
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return false },
			Stop: func(memoryRegion *vmmemory.MemoryRegion, cause error) bool {
				stops <- stop{memoryRegion, cause}
				return true
			},
		})
		stored := make(chan error, 1)
		go func() { stored <- calm.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case s := <-stops:
			if s.memoryRegion != hog {
				t.Fatal("the full budget stopped the memory region that stored last, not the one holding three of its four pages")
			}
			if want := "managed-memory dirty budget stalled: this memory region holds 3 of the budget's 4 pages, more than the store waiting for one"; s.cause == nil || s.cause.Error() != want || !errors.Is(s.cause, vmmemory.ErrDirtyStalled) {
				t.Fatalf("the hog was stopped for %v, want %q", s.cause, want)
			}
		default:
			t.Fatal("nothing was stopped for a budget no checkpoint could relieve")
		}
		select {
		case err := <-stored:
			t.Fatalf("the neighbour's store did not wait for the hog to be stopped: %v", err)
		default:
		}
		// The owner stops the hog: its process exits, and detaching gives every
		// page it held back.
		clear(hm.pages)
		if err := hog.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-stored; err != nil {
			t.Fatalf("the neighbour's store failed once the hog's pages were back: %v", err)
		}
		access(t, calm, cm, 1, true)[0] = 22
		if got := access(t, calm, cm, 0, false)[0]; got != 21 {
			t.Fatalf("the neighbour's first page holds %d, want 21", got)
		}
		select {
		case s := <-stops:
			t.Fatalf("a second stop, of %v, for one full budget", s.memoryRegion)
		default:
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyStalls != 1 || s.DirtyPages != 2 {
			t.Fatalf("%d stalls and %d dirty pages, want the one stop and the neighbour's two pages: %v",
				s.DirtyStalls, s.DirtyPages, err)
		}
	})
}

// An owner stops only the VMs it runs. A holder no owner will stop, such as a
// VM still being started, cannot give the budget back, so the store ends as a
// stall of its own memory region, as it did before anything else was asked.
func TestAFullBudgetNoOwnerWillTakeBackStallsTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 4)
		hog, hm, _ := f.memoryRegion(4)
		calm, cm, _ := f.memoryRegion(4)
		access(t, hog, hm, 0, true)[0] = 11
		access(t, hog, hm, 1, true)[0] = 12
		access(t, hog, hm, 2, true)[0] = 13
		access(t, calm, cm, 0, true)[0] = 21
		var asked []*vmmemory.MemoryRegion
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return false },
			Stop: func(memoryRegion *vmmemory.MemoryRegion, _ error) bool {
				asked = append(asked, memoryRegion)
				return memoryRegion != hog
			},
		})
		if err := calm.Fault(t.Context(), 1, true); !errors.Is(err, vmmemory.ErrDirtyStalled) {
			t.Fatalf("the store failed with %v, want a dirty-budget stall", err)
		}
		if len(asked) != 2 || asked[0] != hog || asked[1] != calm {
			t.Fatalf("the owner was asked to stop %v, want the hog and then the store's own memory region", asked)
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyStalls != 1 {
			t.Fatalf("%d stalls, want one: %v", s.DirtyStalls, err)
		}
	})
}

// A store waiting for the dirty budget wakes on any change to the host — a
// page freed, a page adopted — and asks again what will relieve it. A seal
// takes the dirty set into the checkpoint page by page before it records the
// checkpoint on the memory region, and a waiter that asks in between finds a memory region
// with neither a draining checkpoint nor a dirty page: told that nothing will
// relieve it, it is stalled, and the guest is stopped for a checkpoint that was
// an instruction away from admitting it.
func TestAStoreWokenDuringASealIsNotStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 1)
		r, m, b := f.memoryRegion(2)
		access(t, r, m, 0, true)[0] = 11
		var stalled atomic.Bool
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return true },
			Stop:       func(*vmmemory.MemoryRegion, error) bool { stalled.Store(true); return true },
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
