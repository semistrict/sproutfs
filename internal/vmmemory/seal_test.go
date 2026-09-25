package vmmemory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A seal write-protects the pages the guest already has, in place: one range
// command per run of consecutive dirty pages, whatever pages those pages hold.
// The mappings are not replaced, so a dirty set scattered across the arena costs
// the pause a few commands rather than one per page.
func TestSealProtectsRunsOfDirtyPagesWithoutReplacingTheirMappings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(8)
		// Dirtying backwards gives consecutive pages descending slots, which is
		// exactly the run a mapping command cannot cover but a range protection
		// can. A real guest's dirty set is at least this fragmented. Each store
		// takes two slots — the page it read in to copy from and its own copy —
		// so the pages descend in steps rather than one at a time.
		for page := 3; page >= 0; page-- {
			access(t, r, m, uint64(page), true)[0] = byte(60 + page)
		}
		slots := map[uint64]int{}
		for page := range uint64(4) {
			slots[page] = m.pages[page].slot
		}
		if slots[0] <= slots[1] || slots[1] <= slots[2] || slots[2] <= slots[3] {
			t.Fatalf("the fixture did not fragment the run: %v", slots)
		}
		maps := m.maps
		before, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		seal(t, r)
		after, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if m.maps != maps {
			t.Fatalf("the seal issued %d mapping commands, want none: it must protect the pages in place", m.maps-maps)
		}
		if after.Protections-before.Protections != 1 || after.ProtectedPages-before.ProtectedPages != 4 {
			t.Fatalf("the seal issued %d protections over %d pages, want 1 over 4",
				after.Protections-before.Protections, after.ProtectedPages-before.ProtectedPages)
		}
		for page := range uint64(4) {
			if m.pages[page].slot != slots[page] {
				t.Fatalf("page %d moved from slot %d to %d", page, slots[page], m.pages[page].slot)
			}
			if m.pages[page].writable {
				t.Fatalf("page %d stayed writable through the seal", page)
			}
			if got, err := memoryByte(t.Context(), r, m, page, nil); err != nil || got != byte(60+page) {
				t.Fatalf("page %d reads %d after the seal: %v", page, got, err)
			}
		}
		// The next store still traps and copies on write, leaving the checkpoint
		// alone.
		value := byte(99)
		if _, err := memoryByte(t.Context(), r, m, 1, &value); err != nil {
			t.Fatal(err)
		}
		if _, err := f.publishCheckpoint(t.Context(), r, b); err != nil {
			t.Fatal(err)
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		for page := range 4 {
			if b.data[page*pageSize] != byte(60+page) {
				t.Fatalf("the checkpoint published %d for page %d, want %d", b.data[page*pageSize], page, 60+page)
			}
		}
		if got, err := memoryByte(t.Context(), r, m, 1, nil); err != nil || got != 99 {
			t.Fatalf("the guest reads %d after its store: %v", got, err)
		}
	})
}

// A seal is the vCPU pause, so nothing in it may wait for bytes. A fault
// blocked in its backing load — a volume read, or a migration source that keeps
// answering BUSY — holds the memory region only for the planning and page-table work
// around that read, and the seal runs straight through it.
func TestSealDoesNotWaitForAFaultsBackingLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		// One dirty page, so the seal has a page to take and a protection to
		// issue rather than nothing to do.
		access(t, r, m, 0, true)[0] = 41
		release := make(chan struct{})
		b.onLoad = func(uint64, int) { <-release }
		faulted := make(chan error, 1)
		go func() { faulted <- r.Fault(t.Context(), 2, false) }()
		synctest.Wait()
		sealed := make(chan error, 1)
		go func() { sealed <- r.Seal(t.Context()) }()
		synctest.Wait()
		var sealErr error
		blocked := false
		select {
		case sealErr = <-sealed:
		default:
			blocked = true
		}
		// The load is released whatever happened, so a seal that did wait for
		// it reports that rather than leaving the bubble deadlocked.
		close(release)
		if blocked {
			t.Fatal("the seal waited for the fault's backing load")
		}
		if sealErr != nil {
			t.Fatalf("the seal failed while a fault was loading: %v", sealErr)
		}
		if err := <-faulted; err != nil {
			t.Fatalf("the fault failed after the seal: %v", err)
		}
		if got := access(t, r, m, 2, false)[0]; got != 3 {
			t.Fatalf("page 2 reads %d after the load the seal ran through, want 3", got)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d for the dirty page, want 41", b.data[0])
		}
	})
}

// A reclaim revokes its victim's mappings and writes the bytes to the spill
// file, both of which can be slow: a VMM that is not answering, a disk that is
// not keeping up. A seal taken meanwhile must not wait for either, including
// when the page being reclaimed is one of the pages the seal is taking.
func TestSealDoesNotWaitForAnEvictionsSpill(t *testing.T) {
	for _, mode := range []struct {
		name  string
		write bool
	}{{"read", false}, {"store", true}} {
		t.Run(mode.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { sealThroughAReclaim(t, mode.write) })
		})
	}
}

// sealThroughAReclaim runs one fault into a full arena, holds its reclaim in
// the victim's spill read, and takes a seal of the memory region meanwhile.
func sealThroughAReclaim(t *testing.T, write bool) {
	t.Helper()
	f := newFixture(t, 2, 8, 4)
	r, m, b := f.memoryRegion(4)
	access(t, r, m, 0, true)[0] = 41
	access(t, r, m, 1, false)
	// Page 0 is the least recently used page and the whole dirty set, so the
	// next fault reclaims exactly the page a seal has to take.
	victim := m.pages[0].slot
	release := make(chan struct{})
	f.a.onRead = func(slot int) {
		if slot == victim {
			<-release
		}
	}
	faulted := make(chan error, 1)
	go func() { faulted <- r.Fault(t.Context(), 2, write) }()
	synctest.Wait()
	sealed := make(chan error, 1)
	go func() { sealed <- r.Seal(t.Context()) }()
	synctest.Wait()
	var sealErr error
	blocked := false
	select {
	case sealErr = <-sealed:
	default:
		blocked = true
	}
	close(release)
	if blocked {
		t.Fatal("the seal waited for the eviction's spill")
	}
	if sealErr != nil {
		t.Fatalf("the seal failed while a page was being reclaimed: %v", sealErr)
	}
	if err := <-faulted; err != nil {
		t.Fatalf("the fault failed after the seal: %v", err)
	}
	f.finishCheckpoint(r, b)
	if b.data[0] != 41 {
		t.Fatalf("the checkpoint published %d for the reclaimed page, want 41", b.data[0])
	}
	if got := access(t, r, m, 0, false)[0]; got != 41 {
		t.Fatalf("page 0 reads %d after the checkpoint, want 41", got)
	}
}

// A fault gives the memory region up across its backing read and takes it again, and
// what holds that second acquisition up is a seal, a retire or an unseal — page
// table work, which can itself be waiting on a VMM that is not answering. A
// fault whose guest is gone must not be stuck behind it: the wait is the
// caller's to end, exactly like every other wait a fault does.
func TestAFaultWaitingForTheMemoryRegionAfterItsLoadHonoursCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 41 // one dirty page, so the seal has work
		loading := newGate(t)
		b.onLoad = func(uint64, int) { loading.stop() }
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		faulted := make(chan error, 1)
		go func() { faulted <- r.Fault(ctx, 2, false) }()
		loading.reached(t)
		// The seal takes the memory region the fault gave up, and holds it in a
		// page-table command that is not coming back.
		protecting := newGate(t)
		m.onProtect = func(uint64, int) { protecting.stop() }
		sealed := make(chan error, 1)
		go func() { sealed <- r.Seal(t.Context()) }()
		protecting.reached(t)
		loading.open()
		synctest.Wait()
		cancel()
		if err := <-faulted; !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled fault waiting for the memory region back = %v, want the cancellation", err)
		}
		protecting.open()
		if err := <-sealed; err != nil {
			t.Fatalf("the seal the cancelled fault was waiting for: %v", err)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d, want 41", b.data[0])
		}
	})
}

// A reclaim decides which reservation a page's bytes belong in by reading the
// page's aliases and then the reservations those aliases name. A seal of the
// same page in between joins the checkpoint's copy to the page and hands it
// the page's reservation, so the reclaim sees an alias set without that copy in
// it and a page that no longer names a reservation. The page is punched
// either way, so what it writes is the page's only copy: it has to write the
// reservation the seal moved, not skip the page.
func TestSealInsideAReclaimsAliasWalkKeepsThePagesOnlyCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 4)
		r, m, b := f.memoryRegion(4)
		access(t, r, m, 0, true)[0] = 41
		access(t, r, m, 1, false)
		// Page 0 is the least recently used page and the whole dirty set, so
		// the next fault reclaims exactly the page the seal has to take.
		victim := m.pages[0].slot
		var once sync.Once
		reached, release := make(chan struct{}), make(chan struct{})
		vmmemory.SetEvictionSeam(t, func(slot int) {
			if slot != victim {
				return
			}
			once.Do(func() {
				close(reached)
				<-release
			})
		})
		faulted := make(chan error, 1)
		go func() { faulted <- r.Fault(t.Context(), 2, false) }()
		<-reached
		if err := r.Seal(t.Context()); err != nil {
			t.Fatalf("sealing while the page was being reclaimed: %v", err)
		}
		close(release)
		if err := <-faulted; err != nil {
			t.Fatalf("the fault whose reclaim the seal ran through: %v", err)
		}
		f.finishCheckpoint(r, b)
		if b.data[0] != 41 {
			t.Fatalf("the checkpoint published %d for the reclaimed page, want 41", b.data[0])
		}
		if got, err := memoryByte(t.Context(), r, m, 0, nil); err != nil || got != 41 {
			t.Fatalf("page 0 reads %d after the checkpoint, want 41: %v", got, err)
		}
	})
}

// A seal and a reclaim meet on one page whenever a checkpoint is taken of a
// guest whose pages are under pressure: the reclaim is deciding which
// reservation the page's bytes belong in exactly while the seal hands that
// reservation to the checkpoint's copy of the page. The page is punched
// either way, so what the reclaim wrote is the page's only copy, and both the
// checkpoint reading it and the guest refaulting it have to find those bytes.
//
// Real parallelism is what makes the two meet, so this runs outside a synctest
// bubble: four guests store into a two-page arena while a checkpoint of their
// memory region is sealed, published and retired in a loop.
func TestSealTakingAReclaimingPagesReservationKeepsItsBytes(t *testing.T) {
	const pages, steps = 8, 400
	f := newFixture(t, 2, 32, 24)
	r, m, b := f.memoryRegion(pages)
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for page := range uint64(pages) {
		wg.Go(func() {
			want := byte(page + 1)
			for step := range steps {
				if stopped.Load() {
					return
				}
				got, err := memoryByte(t.Context(), r, m, page, nil)
				if err != nil {
					t.Errorf("page %d step %d: reading the guest's page: %v", page, step, err)
					return
				}
				if got != want {
					t.Errorf("page %d step %d reads %d, want the %d the guest stored", page, step, got, want)
					return
				}
				want = byte(page)<<6 | byte(step%60+1)
				if _, err := memoryByte(t.Context(), r, m, page, &want); err != nil {
					t.Errorf("page %d step %d: storing into the guest's page: %v", page, step, err)
					return
				}
			}
		})
	}
	guests := make(chan struct{})
	go func() {
		wg.Wait()
		close(guests)
	}()
	// A migration source lists what this memory region holds while it runs, which is
	// the other reader of the per-page state a seal and a reclaim are handing
	// between them.
	go func() {
		for {
			select {
			case <-guests:
				return
			default:
			}
			if _, err := r.Stats(t.Context()); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-guests:
			return
		default:
		}
		if err := f.checkpoint(r, b); err != nil {
			t.Errorf("checkpointing the memory region the guests are storing into: %v", err)
			stopped.Store(true)
			<-guests
			return
		}
	}
}
