package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// seal captures r and requires the pause to end before anything reads the
// checkpoint: a Seal that has not returned once every goroutine is blocked is
// waiting for something, which is what the guest's pause time must never
// include.
func seal(t *testing.T, r *vmmemory.MemoryRegion) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Seal(t.Context()) }()
	synctest.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Seal waited for something beyond page-table work")
	}
}

// gate suspends the first goroutine that reaches a simulated hook, so a test
// can run against exactly the suspended state rather than racing it. Later
// arrivals pass through, and reopening is harmless.
type gate struct {
	entered chan struct{}
	resume  chan struct{}
	open    func()
}

func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}, 1), resume: make(chan struct{})}
	g.open = sync.OnceFunc(func() { close(g.resume) })
	t.Cleanup(g.open) // a failed assertion must not strand the suspended goroutine
	return g
}

// stop holds its caller until the test reopens the gate.
func (g *gate) stop() {
	select {
	case g.entered <- struct{}{}:
		<-g.resume
	default:
	}
}
func (g *gate) reached(t *testing.T) {
	t.Helper()
	<-g.entered
}

// A checkpoint publishes the sealed pages themselves: what reaches the volume
// is the memory region exactly as it stood at the seal, and afterwards those pages are
// clean under the checkpoint that now holds them.
func TestCheckpointPublishesTheSealedPagesAndRetiresThemClean(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(8)
		for page := range uint64(4) {
			access(t, r, m, page, true)[0] = byte(50 + page)
		}
		seal(t, r)
		if got := b.checkpoints.Load(); got != 0 {
			t.Fatalf("%d publications completed before Seal returned, want 0", got)
		}
		if got := r.Checkpoint().DirtyPages(); len(got) != 4 {
			t.Fatalf("the checkpoint holds %v, want the four dirty pages", got)
		}
		f.finishCheckpoint(r, b)
		for page := range 4 {
			if b.data[page*pageSize] != byte(50+page) {
				t.Fatalf("page %d of the checkpoint is %d, want %d", page, b.data[page*pageSize], 50+page)
			}
		}
		if got := b.checkpointPages.Load(); got != 4 {
			t.Fatalf("the checkpoint carried %d pages, want 4", got)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if s.DirtyPages != 0 {
			t.Fatalf("the published checkpoint left %d dirty pages, want none", s.DirtyPages)
		}
		if s.CheckpointPages != 4 {
			t.Fatalf("the seal counted %d checkpoint pages, want 4", s.CheckpointPages)
		}
		// The pages are the checkpoint's now, so the guest reads them through the
		// clean pages the retirement published rather than through private state.
		for page := range uint64(4) {
			if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != byte(50+page) {
				t.Fatalf("page %d reads %d after the publication: %v", page, got, err)
			}
		}
	})
}

// A store into a page of the checkpoint runs immediately and copies on write:
// the guest sees its new byte while the checkpoint still publishes the sealed
// one.
func TestStoreIntoASealedPageKeepsTheCheckpointBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 90
		access(t, r, m, 1, true)[0] = 91
		seal(t, r)
		stored := make(chan error, 1)
		go func() {
			value := byte(99)
			_, err := memoryByte(t.Context(), r, m, 0, &value)
			stored <- err
		}()
		synctest.Wait()
		select {
		case err := <-stored:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("a store into a sealed page waited for the checkpoint to be published")
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 99 {
			t.Fatalf("the guest reads %d after its store: %v", got, err)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 90 || b.data[pageSize] != 91 {
			t.Fatalf("the checkpoint published %d and %d, want 90 and 91", b.data[0], b.data[pageSize])
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 99 {
			t.Fatalf("the guest reads %d after the checkpoint landed: %v", got, err)
		}
		// The store the guest made during the publication is the next checkpoint's.
		f.mustCheckpoint(r, b)
		if b.data[0] != 99 {
			t.Fatalf("the checkpoint after the store published %d, want 99", b.data[0])
		}
	})
}

// A publication that never lands hands every page back to the guest as ordinary
// dirty state, which includes the write access the seal took away: the seal
// replaced their mappings with the read-only form the guest traps on, and
// nothing else reinstalls a writable one.
func TestAbandonedCheckpointHandsBackWritablePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		for page := range uint64(4) {
			access(t, r, m, page, true)[0] = byte(60 + page)
		}
		seal(t, r)
		if err := r.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 4 {
			t.Fatalf("the abandoned checkpoint left %d dirty pages, want 4: %v", s.DirtyPages, err)
		}
		value := byte(77)
		if got, err := memoryByte(t.Context(), r, m, 1, &value); err != nil || got != 77 {
			t.Fatalf("a store into a page the abandoned checkpoint handed back read %d: %v", got, err)
		}
		if got, err := memoryByte(t.Context(), r, m, 1, nil); err != nil || got != 77 {
			t.Fatalf("the guest reads %d after its store: %v", got, err)
		}
		f.mustCheckpoint(r, b)
		want := []byte{60, 77, 62, 63}
		for page := range uint64(4) {
			if b.data[page*uint64(pageSize)] != want[page] {
				t.Fatalf("page %d published %d, want %d", page, b.data[page*uint64(pageSize)], want[page])
			}
		}
	})
}

// Unseal is the abandon a caller reaches for when no checkpoint is going to take
// the checkpoint, and it leaves the memory region sealable again.
func TestUnsealAbandonsTheCheckpointAndAllowsAnotherSeal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 21
		seal(t, r)
		if err := r.Seal(t.Context()); !errors.Is(err, vmmemory.ErrSealed) {
			t.Fatalf("sealing a sealed memory region = %v, want ErrSealed", err)
		}
		if err := r.Unseal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if r.Checkpoint() != nil {
			t.Fatal("the memory region still holds a checkpoint after its unseal")
		}
		if err := r.Unseal(t.Context()); err != nil {
			t.Fatalf("unsealing an unsealed memory region: %v", err)
		}
		access(t, r, m, 1, true)[0] = 22
		f.mustCheckpoint(r, b)
		if b.data[0] != 21 || b.data[pageSize] != 22 {
			t.Fatalf("the checkpoint after the unseal published %d and %d, want 21 and 22", b.data[0], b.data[pageSize])
		}
	})
}

// A retire walks a whole dirty set, which is as large as a capture's. It takes
// the memory region in batches, so a fault on the memory region waits for one batch and not
// for the walk, and its volume metadata is one lookup per read-ahead window,
// taken before it holds the memory region or any page.
func TestRetireLocatesPerWindowAndFreesTheMemoryRegionBetweenBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetCheckpointBatchPages(t, 2)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 4})
		r, m, b := f.memoryRegion(8)
		for page := range uint64(4) {
			access(t, r, m, page, true)[0] = byte(40 + page)
		}
		seal(t, r)
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		// Every sealed page is in the window at offset zero; the fault below
		// looks up the one after it, which is not the retire's work.
		locates := 0
		retiring := newGate(t)
		b.onLocate = func(offset, _ uint64) {
			if offset != 0 {
				return
			}
			locates++
			retiring.stop()
		}
		retired := make(chan error, 1)
		go func() { retired <- r.Checkpoint().Retire(t.Context(), true) }()
		retiring.reached(t)
		faulted := make(chan error, 1)
		go func() { faulted <- r.Fault(t.Context(), 6, false) }()
		synctest.Wait()
		var faultErr error
		blocked := false
		select {
		case faultErr = <-faulted:
		default:
			blocked = true
		}
		retiring.open()
		if blocked {
			t.Fatal("a fault waited for the retire's volume metadata")
		}
		if faultErr != nil {
			t.Fatal(faultErr)
		}
		if err := <-retired; err != nil {
			t.Fatal(err)
		}
		if locates != 2 {
			t.Fatalf("the retire located %d times, want one per batch", locates)
		}
		for page := range uint64(4) {
			if b.data[page*uint64(pageSize)] != byte(40+page) {
				t.Fatalf("the checkpoint published %d for page %d", b.data[page*uint64(pageSize)], page)
			}
			if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != byte(40+page) {
				t.Fatalf("the retired page %d reads %d: %v", page, got, err)
			}
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.DirtyPages != 0 {
			t.Fatalf("the published checkpoint left %d dirty pages: %v", s.DirtyPages, err)
		}
	})
}

// Retiring a page of a published checkpoint must never leave its private page
// reachable from a binding that owns neither a spill reservation nor a
// checkpoint: an eviction in that window would punch the page with nowhere to
// put its bytes.
func TestCheckpointRetirementNeverStrandsItsPrivatePage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 1, 8, 4)
		r, m, b := f.memoryRegion(1)
		other, _, _ := f.memoryRegion(1)
		access(t, r, m, 0, true)[0] = 41
		seal(t, r)
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		retiring := newGate(t)
		b.onLocate = func(uint64, uint64) { retiring.stop() }
		retired := make(chan error, 1)
		go func() { retired <- r.Checkpoint().Retire(t.Context(), true) }()
		retiring.reached(t)
		if err := other.Fault(t.Context(), 0, false); err != nil {
			t.Fatalf("an unrelated volume's fault during the checkpoint's retirement failed: %v", err)
		}
		retiring.open()
		if err := <-retired; err != nil {
			t.Fatal(err)
		}
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d, want 41", b.data[0])
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 41 {
			t.Fatalf("the retired page reads %d: %v", got, err)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 0 {
			t.Fatalf("the published checkpoint left %d dirty pages: %v", s.DirtyPages, err)
		}
	})
}

// A seal whose protection fails partway captures nothing, and takes no page at
// all: the runs it did protect have their mappings taken away so the guest
// faults and maps them writable again, and sealing again takes a checkpoint of
// everything that is dirty then. The pause is the protect commands, so a
// capture abandoned inside one has nothing to hand back but them.
func TestSealRetriedAfterAPartialSealCapturesEveryDirtyPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8})
		r, m, b := f.memoryRegion(4)
		// Two runs: the gap at page 1 is what makes the seal two commands, so
		// the capture can be abandoned between them.
		access(t, r, m, 2, true)[0] = 62
		access(t, r, m, 0, true)[0] = 61
		vmmemory.SetCheckpointBatchPages(t, 1)
		ctx, cancel := context.WithCancelCause(t.Context())
		stop := errors.New("capture abandoned")
		commands := 0
		m.onProtect = func(uint64, int) {
			if commands++; commands == 2 {
				cancel(stop)
			}
		}
		if err := r.Seal(ctx); !errors.Is(err, stop) {
			t.Fatalf("Seal = %v, want the cancelled capture", err)
		}
		m.onProtect = nil
		if commands != 2 {
			t.Fatalf("the abandoned seal issued %d write-protect commands, want 2", commands)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.CheckpointPages != 0 {
			t.Fatalf("the abandoned seal took %d pages into a checkpoint, want none: its pause is its commands: %v",
				s.CheckpointPages, err)
		}
		if r.Checkpoint() != nil {
			t.Fatal("a seal that failed partway left a checkpoint behind")
		}
		// The failed seal captured nothing, so this store belongs to the retry —
		// and it reaches a page the abandoned seal had write-protected.
		value := byte(99)
		if _, err := memoryByte(t.Context(), r, m, 0, &value); err != nil {
			t.Fatal(err)
		}
		f.mustCheckpoint(r, b)
		if b.data[0] != 99 || b.data[2*pageSize] != 62 {
			t.Fatalf("the retried checkpoint published %d and %d, want 99 and 62", b.data[0], b.data[2*pageSize])
		}
		if s, err := f.h.Stats(t.Context()); err != nil || s.CheckpointPages != 2 || s.DirtyPages != 0 {
			t.Fatalf("the retry counted %d checkpoint pages and left %d dirty, want 2 and 0: %v", s.CheckpointPages, s.DirtyPages, err)
		}
	})
}

// Abandoning a checkpoint moves its spill reservation back to the guest's page
// under the page's lock. An eviction that observed the two apart would find a
// private page with neither a reservation nor a checkpoint to spill it, and
// would punch the page with nowhere to put its bytes.
func TestAbandonedCheckpointKeepsItsPagesAcrossConcurrentEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 16, 8)
		r, m, b := f.memoryRegion(4)
		other, _, _ := f.memoryRegion(4)
		for page := range uint64(2) {
			access(t, r, m, page, true)[0] = byte(60 + page)
		}
		seal(t, r)
		checkpoint := r.Checkpoint()
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := checkpoint.Retire(t.Context(), false); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			for page := range uint64(4) {
				if err := other.Fault(t.Context(), page, false); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Wait()
		for page := range uint64(2) {
			if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != byte(60+page) {
				t.Fatalf("page %d of the abandoned checkpoint reads %d: %v", page, got, err)
			}
		}
		f.mustCheckpoint(r, b)
		for page := range uint64(2) {
			if b.data[page*uint64(pageSize)] != byte(60+page) {
				t.Fatalf("page %d published %d, want %d", page, b.data[page*uint64(pageSize)], 60+page)
			}
		}
	})
}

// The checkpoint's pages count against the dirty budget until they are
// retired, so a guest that dirties faster than its checkpoint uploads waits for
// it instead of failing or overrunning the budget.
func TestCheckpointPagesHoldTheDirtyBudgetUntilRetired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 2)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 10
		access(t, r, m, 1, true)[0] = 11
		seal(t, r)
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 2 {
			t.Fatalf("the checkpoint accounts %d dirty pages, want 2: %v", s.DirtyPages, err)
		}
		stored := make(chan error, 1)
		go func() {
			value := byte(12)
			_, err := memoryByte(t.Context(), r, m, 2, &value)
			stored <- err
		}()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store beyond the dirty budget returned %v instead of waiting for the checkpoint", err)
		default:
		}
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if err := <-stored; err != nil {
			t.Fatal(err)
		}
		if b.data[0] != 10 || b.data[pageSize] != 11 {
			t.Fatalf("the checkpoint published %d and %d, want 10 and 11", b.data[0], b.data[pageSize])
		}
		s, err = f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 1 {
			t.Fatalf("%d dirty pages remain, want the one store the guest made: %v", s.DirtyPages, err)
		}
		if got, err := memoryByte(t.Context(), r, m, 2, nil); err != nil || got != 12 {
			t.Fatalf("the throttled store left %d: %v", got, err)
		}
	})
}

// The checkpoint's copies are reclaimable like any other private state: an
// arena too small to hold them spills them, and the guest keeps reading and
// storing through pages whose only current bytes are in that spill.
func TestSealedCheckpointSurvivesReclaimAndRefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 8)
		r, m, b := f.memoryRegion(4)
		for page := range uint64(4) {
			access(t, r, m, page, true)[0] = byte(80 + page)
		}
		seal(t, r)
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 80 {
			t.Fatalf("a reclaimed page of the checkpoint reads %d: %v", got, err)
		}
		value := byte(88)
		if _, err := memoryByte(t.Context(), r, m, 1, &value); err != nil {
			t.Fatal(err)
		}
		if got, err := memoryByte(t.Context(), r, m, 3, nil); err != nil || got != 83 {
			t.Fatalf("a reclaimed page of the checkpoint reads %d: %v", got, err)
		}
		f.finishCheckpoint(r, b)
		for page := range 4 {
			if b.data[page*pageSize] != byte(80+page) {
				t.Fatalf("page %d of the checkpoint is %d, want %d", page, b.data[page*pageSize], 80+page)
			}
		}
		if got, err := memoryByte(t.Context(), r, m, 1, nil); err != nil || got != 88 {
			t.Fatalf("the store made during the publication left %d: %v", got, err)
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 1 {
			t.Fatalf("%d dirty pages remain, want the one store the guest made: %v", s.DirtyPages, err)
		}
	})
}

// A page of the checkpoint whose memory was reclaimed comes back on a fresh
// resident page when the guest reads it, and that page must still be the
// checkpoint's: once the checkpoint is published it is the page's clean state,
// reclaimable like any other. One the guest alone held would be private with no
// reservation left to spill it once the retirement released the checkpoint's.
func TestAPageOfTheCheckpointRefaultedFromSpillRetiresWithIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 8)
		r, m, b := f.memoryRegion(4)
		// Two slots: storing into pages 2 and 3 spills pages 0 and 1.
		for page := range uint64(4) {
			access(t, r, m, page, true)[0] = byte(80 + page)
		}
		seal(t, r)
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 80 {
			t.Fatalf("a spilled page of the checkpoint reads %d: %v", got, err)
		}
		f.finishCheckpoint(r, b)
		// Reclaim every page, the refaulted one among them, and read back.
		for _, page := range []uint64{1, 2, 3, 0} {
			if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != byte(80+page) {
				t.Fatalf("page %d reads %d after the checkpoint was published: %v", page, got, err)
			}
		}
		s, err := f.h.Stats(t.Context())
		if err != nil || s.DirtyPages != 0 {
			t.Fatalf("%d dirty pages remain after the publication, want none: %v", s.DirtyPages, err)
		}
		for page := range 4 {
			if b.data[page*pageSize] != byte(80+page) {
				t.Fatalf("page %d of the checkpoint is %d, want %d", page, b.data[page*pageSize], 80+page)
			}
		}
	})
}

// Whatever the guest was doing, the bytes a capture delivers are the volume as
// it stood at the seal, and the guest's own view continues from there.
func TestRandomizedCapturesAgainstAnIndependentByteModel(t *testing.T) {
	for seed := uint64(0); seed < 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, 3, 16, 16)
				r, m, b := f.memoryRegion(8)
				want := bytes.Clone(b.data)
				rng := rand.New(rand.NewPCG(seed, seed+11))
				for step := range 200 {
					page := uint64(rng.IntN(8))
					if rng.IntN(3) == 0 {
						offset, value := rng.IntN(pageSize), byte(rng.Uint32())
						access(t, r, m, page, true)[offset] = value
						want[int(page)*pageSize+offset] = value
					} else if got := access(t, r, m, page, false); !bytes.Equal(got, want[int(page)*pageSize:(int(page)+1)*pageSize]) {
						t.Fatalf("step %d: page %d does not match the model", step, page)
					}
					if step%29 != 0 {
						continue
					}
					at := bytes.Clone(want)
					seal(t, r)
					// The guest stores on while the checkpoint is still unpublished.
					stored, value := uint64(rng.IntN(8)), byte(rng.Uint32())
					access(t, r, m, stored, true)[0] = value
					want[int(stored)*pageSize] = value
					if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(b.data, at) {
						t.Fatalf("step %d: the checkpoint is not the volume as it stood at the seal", step)
					}
					if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
						t.Fatal(err)
					}
				}
				f.mustCheckpoint(r, b)
				if !bytes.Equal(b.data, want) {
					t.Fatal("the checkpoint after the last capture lost the guest's stores")
				}
			})
		})
	}
}

// A sealed volume keeps serving its guest while the capture publishes: other
// volumes checkpoint, the sealed volume's pages can be reclaimed, and nothing
// the guest stores afterwards reaches the volume until the next checkpoint
// takes it.
func TestSealedCaptureKeepsTheVolumeStableWhileTheGuestRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 8)
		a, am, ab := f.memoryRegion(2)
		b, bm, bb := f.memoryRegion(2)
		access(t, a, am, 0, true)[0] = 90
		seal(t, a)
		if _, err := f.publishCheckpoint(t.Context(), a, ab); err != nil {
			t.Fatal(err)
		}
		if ab.data[0] != 90 {
			t.Fatalf("the checkpoint published %d, want 90", ab.data[0])
		}
		// Checkpoint an unrelated volume while this one is still sealed.
		access(t, b, bm, 0, true)[0] = 40
		access(t, b, bm, 1, true)[0] = 41
		f.mustCheckpoint(b, bb)
		if bb.data[0] != 40 || bb.data[pageSize] != 41 {
			t.Fatalf("the unrelated volume published %d and %d, want 40 and 41", bb.data[0], bb.data[pageSize])
		}
		access(t, a, am, 0, true)[0] = 91
		if ab.data[0] != 90 {
			t.Fatal("a store after the checkpoint changed the captured volume")
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if ab.data[0] != 90 {
			t.Fatal("retiring the checkpoint published a store it did not include")
		}
		f.mustCheckpoint(a, ab)
		if ab.data[0] != 91 {
			t.Fatalf("the checkpoint after the capture published %d, want 91", ab.data[0])
		}
	})
}

// A checkpoint that has ended holds nothing: its pages are the guest's own
// dirty state again or the volume's clean state, and the pages behind them
// hold whatever the guest has done since. A publication still reading it would
// be reading bytes no checkpoint stands behind and publishing them as that
// checkpoint's, so the read fails instead. A memory region that discarded its
// checkpoint reports why it ended.
func TestReadingACheckpointThatHasEndedFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 4)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 41
		seal(t, r)
		retired := r.Checkpoint()
		f.finishCheckpoint(r, b)
		value := byte(99)
		if _, err := memoryByte(t.Context(), r, m, 0, &value); err != nil {
			t.Fatal(err)
		}
		page := make([]byte, pageSize)
		if err := retired.ReadDirty(t.Context(), 0, page); !errors.Is(err, vmmemory.ErrNotSealed) {
			t.Fatalf("reading a retired checkpoint = %v (page reads %d), want ErrNotSealed", err, page[0])
		}
		access(t, r, m, 1, true)[0] = 42
		seal(t, r)
		discarded := r.Checkpoint()
		clear(m.pages) // the memory users are stopped and their slots gone
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := discarded.ReadDirty(t.Context(), 1, page); !errors.Is(err, vmmemory.ErrClosed) {
			t.Fatalf("reading the checkpoint a detached memory region discarded = %v, want ErrClosed", err)
		}
	})
}

// A store into a sealed page copies it away from the checkpoint: it reads the
// sealed bytes, takes an arena slot and fills it. Until that private page is
// bound the page's only bytes are the checkpoint's, so a store that fails on
// the way — the arena refusing the slot it was filling — has to leave the page
// exactly where the seal left it. A page released from the checkpoint before
// its replacement exists is dirty state with no memory, no reservation and no
// checkpoint holding either: its bytes are unreachable for good, and the memory region
// carries on as though nothing had happened.
func TestAStoreThatCannotTakeItsPrivatePageLeavesThePageInTheCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 8, 4)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 41
		seal(t, r)
		f.a.failWrite = true
		value := byte(99)
		if _, err := memoryByte(t.Context(), r, m, 0, &value); !errors.Is(err, errInjected) {
			t.Fatalf("a store whose private page could not be filled = %v, want the injected failure", err)
		}
		f.a.failWrite = false
		// The page is still the checkpoint's, so the guest reads the bytes the
		// seal froze and so does the publication.
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 41 {
			t.Fatalf("the guest reads %d after the failed store, want the sealed 41: %v", got, err)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d for the page the failed store left, want 41", b.data[0])
		}
		// The store the arena refused succeeds once it can be served.
		if _, err := memoryByte(t.Context(), r, m, 0, &value); err != nil {
			t.Fatalf("storing into the page after the checkpoint: %v", err)
		}
		f.mustCheckpoint(r, b)
		if b.data[0] != 99 {
			t.Fatalf("the checkpoint after the retried store published %d, want 99", b.data[0])
		}
	})
}

// A refault of a page that has been spilled decides, under the memory region, that the
// page is the guest's own dirty state, and then gives the memory region up to find an
// arena slot for it. A checkpoint that runs in that window ends the page's
// dirty epoch: the volume holds its bytes now, so the page is clean state under
// the identity that checkpoint gave it, and the reservation that spilled it has
// gone back. The refault has to see that rather than act on what it decided
// before it gave the memory region up, exactly as a store does across its own reclaim.
//
// Two things go wrong when it does not. A private page bound to a binding that
// owns neither a reservation nor a checkpoint is one a reclaim punches without
// writing it anywhere. And the page is named by nothing, so nothing that
// inherits the identity the checkpoint gave it can map it: every other memory region
// of that volume reads its own copy of bytes this host is already holding.
func TestARefaultWhoseCheckpointRetiresWhileItReclaimsGivesThePageToTheVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 32, 8)
		r, m, b := f.memoryRegion(4)
		stored := byte(91)
		if _, err := memoryByte(t.Context(), r, m, 0, &stored); err != nil {
			t.Fatalf("storing into page 0: %v", err)
		}
		// Page 0 leaves the arena for the spill file, so the access below is the
		// refault this test is about.
		for range 2 {
			for page := uint64(1); page < 4; page++ {
				if _, err := memoryByte(t.Context(), r, m, page, nil); err != nil {
					t.Fatalf("reading page %d to press page 0 out of the arena: %v", page, err)
				}
			}
		}
		if _, mapped := m.pages[0]; mapped {
			t.Fatal("page 0 is still mapped, so nothing below is a refault")
		}
		var once sync.Once
		vmmemory.SetReclaimSeam(t, func(index uint64) {
			if index != 0 {
				return
			}
			once.Do(func() {
				if err := f.checkpoint(r, b); err != nil {
					t.Errorf("checkpointing while the refault reclaimed: %v", err)
				}
			})
		})
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != stored {
			t.Fatalf("the refaulted page reads %d, want the %d the guest stored: %v", got, stored, err)
		}
		// The checkpoint published those bytes, so the page this memory region holds is
		// the volume's: a second memory region of the same volume maps that very page
		// rather than reading the bytes again.
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		second, sm := f.attach(b)
		if got, err := memoryByte(t.Context(), second, sm, 0, nil); err != nil || got != stored {
			t.Fatalf("the second memory region reads %d for page 0, want %d: %v", got, stored, err)
		}
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if sm.pages[0].slot != m.pages[0].slot {
			t.Errorf("the second memory region maps slot %d for page 0 and the first slot %d; the retired page was kept private",
				sm.pages[0].slot, m.pages[0].slot)
		}
		if after.Loads != before.Loads || after.IdentityHits-before.IdentityHits != 1 {
			t.Errorf("the second memory region's page 0 cost %d backing reads and %d identity hits, want 0 and 1",
				after.Loads-before.Loads, after.IdentityHits-before.IdentityHits)
		}
		// Taking the page out of the arena once more and reading it back is
		// where the audit reported the mistake: a binding granted the right to
		// store after its dirty epoch had ended is owed a generation nothing
		// will ever give back, so the published page it is handed here, and the
		// copy the store below takes, are both called a lost write.
		for range 2 {
			for page := uint64(1); page < 4; page++ {
				if _, err := memoryByte(t.Context(), r, m, page, nil); err != nil {
					t.Fatalf("reading page %d to press page 0 out a second time: %v", page, err)
				}
			}
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != stored {
			t.Fatalf("page 0 reads %d after its second eviction, want %d: %v", got, stored, err)
		}
		next := byte(92)
		if _, err := memoryByte(t.Context(), r, m, 0, &next); err != nil {
			t.Fatalf("storing into page 0 after the retire: %v", err)
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != next {
			t.Fatalf("page 0 reads %d after the store, want %d: %v", got, next, err)
		}
	})
}
