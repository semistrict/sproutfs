package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// lossWindow is the window every fixture here runs with. It is long enough that
// nothing crosses it by accident and short enough to be waited out in one sleep.
const lossWindow = time.Minute

// windowFixture is a pager whose dirty budget is never the thing under test:
// every store it refuses is refused for the age of what the memory region holds, not
// for the room it has.
func windowFixture(t *testing.T, window time.Duration) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32,
		DirtyPages: 8, LossWindow: window})
}

// A VM whose oldest unpublished write is older than the loss window admits no
// further dirty page: the store waits in the pager exactly as a store past the
// dirty budget waits, and lands when the checkpoint it asked for lands. That is
// what makes the bound a bound — without it a guest whose publications keep
// failing goes on building on writes that could be lost.
func TestAStorePastTheLossWindowWaitsForItsCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, b := f.memoryRegion(4)
		requested := make(chan *vmmemory.MemoryRegion, 4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(memoryRegion *vmmemory.MemoryRegion) bool {
			select {
			case requested <- memoryRegion:
			default:
			}
			return true
		}})
		access(t, r, m, 0, true)[0] = 11
		// Nothing is over the window yet, so the guest stores freely.
		access(t, r, m, 1, true)[0] = 12
		if s := hostStats(t, f); s.WindowWaits != 0 {
			t.Fatalf("%d stores waited on the window before it expired, want none", s.WindowWaits)
		}
		time.Sleep(lossWindow + time.Second)
		stored := make(chan error, 1)
		go func() { stored <- r.Fault(t.Context(), 2, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store past the loss window did not wait: %v", err)
		default:
		}
		select {
		case got := <-requested:
			if got != r {
				t.Fatalf("the pager asked to checkpoint %v, want the memory region over its window", got)
			}
		default:
			t.Fatal("a store past the loss window waited without asking for a checkpoint")
		}
		f.mustCheckpoint(r, b)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the checkpoint that ended the window: %v", err)
		}
		if s := hostStats(t, f); s.WindowWaits == 0 {
			t.Fatal("the wait was not counted as a loss-window wait")
		}
		access(t, r, m, 2, true)[0] = 13
		if access(t, r, m, 2, false)[0] != 13 {
			t.Fatal("the store that waited did not write the guest's own bytes")
		}
	})
}

// A window of zero is a deployment that has turned the bound off: the pager
// dates its pages all the same, and no store ever waits for their age.
func TestADisabledLossWindowNeverWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, 0)
		r, m, _ := f.memoryRegion(4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(*vmmemory.MemoryRegion) bool {
			t.Error("a pager with no loss window asked for a checkpoint of its own")
			return false
		}})
		access(t, r, m, 0, true)[0] = 11
		time.Sleep(100 * lossWindow)
		access(t, r, m, 1, true)[0] = 12
		access(t, r, m, 2, true)[0] = 13
		if s := hostStats(t, f); s.WindowWaits != 0 || s.WindowStalls != 0 {
			t.Fatalf("a disabled window waited %d times and stalled %d, want neither",
				s.WindowWaits, s.WindowStalls)
		}
	})
}

// A store waiting on the window where no checkpoint of the VM can ever be taken
// is a store waiting for something that will not happen. It ends the way a
// budget stall ends: the memory region's owner is told, and it stops that VM
// deliberately rather than leaving the guest wedged.
func TestAStorePastTheLossWindowWithNoCheckpointComingIsAStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, _ := f.memoryRegion(4)
		var stoppedMemoryRegion *vmmemory.MemoryRegion
		var stoppedCause error
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return false },
			Stop: func(memoryRegion *vmmemory.MemoryRegion, cause error) {
				stoppedMemoryRegion, stoppedCause = memoryRegion, cause
			},
		})
		access(t, r, m, 0, true)[0] = 11
		time.Sleep(lossWindow + time.Second)
		err := r.Fault(t.Context(), 1, true)
		if !errors.Is(err, vmmemory.ErrWindowStalled) {
			t.Fatalf("the stalled store failed with %v, want a loss-window stall", err)
		}
		if errors.Is(err, vmmemory.ErrCapacity) {
			t.Fatal("a stalled store must not reach the fault path as capacity exhaustion")
		}
		if stoppedMemoryRegion != r || !errors.Is(stoppedCause, vmmemory.ErrWindowStalled) {
			t.Fatalf("the pager stopped %v for %v, want the memory region over its window", stoppedMemoryRegion, stoppedCause)
		}
		if s := hostStats(t, f); s.WindowStalls != 1 {
			t.Fatalf("%d loss-window stalls, want exactly one", s.WindowStalls)
		}
	})
}

// A checkpoint that did not publish gives its pages back to the guest, and their
// age comes back with them: the window is measured from the guest's own store,
// not from the last attempt to publish it. A window that restarted at every
// failed publication would bound nothing, since a host that cannot reach the
// store is exactly the host that keeps failing.
func TestAnAbandonedCheckpointHandsTheLossWindowBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, _ := f.memoryRegion(4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(*vmmemory.MemoryRegion) bool { return true }})
		access(t, r, m, 0, true)[0] = 11
		time.Sleep(lossWindow + time.Second)
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The publication failed, so the pages go back to the guest as dirty state
		// exactly as old as they were.
		if err := r.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		stored := make(chan error, 1)
		go func() { stored <- r.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store past a window an abandoned checkpoint gave back did not wait: %v", err)
		default:
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after a checkpoint of it landed: %v", err)
		}
	})
}

// The window is a VM's, because the checkpoint is: a memory region holding nothing old
// of its own still waits while another memory region of the same VM holds a write past
// the window, since the checkpoint that ends the wait covers both. The pager
// knows nothing about VMs — it asks whoever owns the memory region.
func TestTheLossWindowIsTheVMsRatherThanOneMemoryRegions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		ram, ramMapping, ramBacking := f.memoryRegion(4)
		disk, diskMapping, _ := f.memoryRegion(4)
		oldest := func(memoryRegion *vmmemory.MemoryRegion) time.Time {
			// Both memory regions belong to one VM, so each answers for the pair.
			first := ram.OldestUnpublished()
			if second := disk.OldestUnpublished(); !second.IsZero() && (first.IsZero() || second.Before(first)) {
				first = second
			}
			return first
		}
		f.h.SetPressure(vmmemory.Pressure{
			Checkpoint: func(*vmmemory.MemoryRegion) bool { return true },
			Oldest:     oldest,
		})
		access(t, ram, ramMapping, 0, true)[0] = 11
		time.Sleep(lossWindow + time.Second)
		// This memory region holds nothing at all, so only its VM's other memory region can be
		// what stops this store.
		stored := make(chan error, 1)
		go func() { stored <- disk.Fault(t.Context(), 0, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store in a memory region whose VM is past the window did not wait: %v", err)
		default:
		}
		f.mustCheckpoint(ram, ramBacking)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after its VM's other memory region was checkpointed: %v", err)
		}
		access(t, disk, diskMapping, 0, true)[0] = 21
	})
}

// A handoff carries how old the memory region's oldest unpublished write is, so the
// destination inherits the source's window rather than restarting it. A
// migration that reset the clock would let a VM be handed from host to host for
// ever without its writes ever being bounded.
func TestAHandoffCarriesTheAgeOfTheOldestUnpublishedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, _ := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 11
		time.Sleep(90 * time.Second)
		age, err := r.Handoff(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if age != 90*time.Second {
			t.Fatalf("the handoff reports an unpublished age of %s, want 90s", age)
		}
	})
}

// A memory region with nothing unpublished hands off an age of zero: there is nothing
// for the destination to date, and a destination given one would start its
// guest already inside a window it never wrote in.
func TestAHandoffOfACleanMemoryRegionCarriesNoAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 11
		f.mustCheckpoint(r, b)
		time.Sleep(90 * time.Second)
		age, err := r.Handoff(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if age != 0 {
			t.Fatalf("a memory region holding nothing unpublished hands off an age of %s, want none", age)
		}
	})
}

// The destination dates the pages it receives from the age the handoff carried,
// on its own clock, so a VM that arrives already past its window waits for its
// first checkpoint here rather than for a whole window more.
func TestAReceivedMemoryRegionInheritsItsSourcesWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := windowFixture(t, lossWindow)
		r, m, b := f.memoryRegion(4)
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(*vmmemory.MemoryRegion) bool { return true }})
		// The source held this VM's writes for nine tenths of the window; a tenth
		// of it is all this host has.
		r.SetUnpublishedAge(lossWindow - lossWindow/10)
		access(t, r, m, 0, true)[0] = 11
		time.Sleep(lossWindow/10 + time.Second)
		stored := make(chan error, 1)
		go func() { stored <- r.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store past a window inherited from the source did not wait: %v", err)
		default:
		}
		f.mustCheckpoint(r, b)
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the destination's first checkpoint: %v", err)
		}
	})
}
