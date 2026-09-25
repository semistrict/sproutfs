package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// What a capture's pause costs.
//
// A seal's pause is its write-protect commands and nothing else. From the
// moment a run is protected no store to it can land without trapping and
// waiting for the memory region, so the set is fixed there; what the seal has to do per
// page — move the binding into the checkpoint, hand it the page's reservation
// and the page it was copied from — is a walk that runs afterwards, with the
// guest already running and holding the memory region the seal took. On GCE a capture
// of 2,204,672 sealed RAM pages paused 2.14 s, of which 0.18 s was its 2,264
// protect commands and 1.97 s was that walk.

// The pause over one run of private pages is one command, and it takes no page
// into the checkpoint: that is the walk's, behind it.
func TestASealsPauseIsItsWriteProtectCommandsAndNotItsPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 1024
		f, r, m, b := placedMemoryRegion(t, 2*rangePages)
		held(t, r, m, 0, pages)
		// Every page of this backing holds its own number plus one, so a zero is
		// a byte none of them had and the settle finds every one of them changed.
		for page := range uint64(pages) {
			access(t, r, m, page, true)[0] = 0
		}
		// The walk is held where it has taken nothing, so what is counted below
		// is the pause alone.
		walking, release := make(chan struct{}), make(chan struct{})
		vmmemory.SetSealWalkSeam(t, func() { close(walking); <-release })
		before, protects := hostStats(t, f), m.protects
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.protects - protects; got != 1 {
			t.Fatalf("the pause issued %d write-protect commands over one run of %d pages, want 1", got, pages)
		}
		<-walking
		if got := hostStats(t, f).CheckpointPages - before.CheckpointPages; got != 0 {
			t.Fatalf("the pause took %d pages into the checkpoint, want none: the walk behind it does", got)
		}
		close(release)
		if got := len(r.Checkpoint().DirtyPages()); got != pages {
			t.Fatalf("the walk took %d pages into the checkpoint, want %d", got, pages)
		}
		if got := hostStats(t, f).CheckpointPages - before.CheckpointPages; got != pages {
			t.Fatalf("the walk counted %d sealed pages, want %d", got, pages)
		}
		if got := f.settle(r); got != 0 {
			t.Fatalf("the settle handed back %d pages the guest stored into, want none", got)
		}
		f.finishCheckpoint(r, b)
		for page := range uint64(pages) {
			if got := access(t, r, m, page, false)[0]; got != 0 {
				t.Fatalf("page %d reads %d after its checkpoint, want the 0 the guest stored", page, got)
			}
		}
	})
}

// A guest that resumes into the walk behind the pause is a guest that faults:
// its store into a sealed page traps, waits for the memory region the walk holds, and
// is served the moment the walk gives it back. Nothing of it is lost.
func TestAStoreIntoASealedPageWaitsForTheWalkBehindThePause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 32
		f, r, m, b := placedMemoryRegion(t, 2*rangePages)
		held(t, r, m, 0, pages)
		for page := range uint64(pages) {
			access(t, r, m, page, true)[0] = 0
		}
		walking, release := make(chan struct{}), make(chan struct{})
		vmmemory.SetSealWalkSeam(t, func() { close(walking); <-release })
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-walking
		stored := make(chan error, 1)
		go func() { stored <- r.Fault(t.Context(), 5, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("the store into a sealed page was served while the walk held the memory region: %v", err)
		default:
		}
		close(release)
		if err := <-stored; err != nil {
			t.Fatal(err)
		}
		m.pages[5] = mapped{m.pages[5].slot, true}
		access(t, r, m, 5, true)[1] = 9
		if got := len(r.Checkpoint().DirtyPages()); got != pages {
			t.Fatalf("the checkpoint holds %d pages, want the %d the seal froze", got, pages)
		}
		f.finishCheckpoint(r, b)
		if got := access(t, r, m, 5, false)[1]; got != 9 {
			t.Fatalf("page 5 reads %d, want the 9 the guest stored after the seal", got)
		}
	})
}
