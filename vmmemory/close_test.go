package vmmemory_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
)

// heldReleaseArena holds the at'th slot its files give back, counting from
// one, until resume is closed, and lets every other through.
type heldReleaseArena struct {
	*arena
	at       int
	mu       sync.Mutex
	released int
	held     chan struct{}
	resume   chan struct{}
}

func (a *heldReleaseArena) File(ctx context.Context, offsets int) (vmmemory.ArenaFile, error) {
	f, err := a.arena.File(ctx, offsets)
	if err != nil {
		return nil, err
	}
	return heldReleaseFile{f.(*arenaFile), a}, nil
}

type heldReleaseFile struct {
	*arenaFile
	arena *heldReleaseArena
}

func (f heldReleaseFile) Release(ctx context.Context, slot int) error {
	f.arena.mu.Lock()
	f.arena.released++
	hold := f.arena.released == f.arena.at
	f.arena.mu.Unlock()
	if hold {
		close(f.arena.held)
		<-f.arena.resume
	}
	return f.arenaFile.Release(ctx, slot)
}

// A memory region is attached until its pages are back. A pager closed while a
// detach is still giving them back refuses, rather than releasing its files
// and its spill under the pages that detach is about to free into them.
func TestAPagerDoesNotCloseUnderADetachStillGivingItsPagesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := pageBudget(t, 2)
		cfg := vmmemory.Config{ResidentPages: 2, LogicalPages: 2, DirtyPages: 2}
		f := newConfiguredFixture(t, cfg, b)
		spill, err := f.disk.Open(t.Context(), "held-spill", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer spill.Close()
		// A detach of a region that stored into its one page gives back two:
		// the page it was copied from, which nothing maps now, and then its
		// copy, which goes with the region's layer. The second is the one that
		// must find the pager still open.
		a := &heldReleaseArena{arena: f.a, at: 2, held: make(chan struct{}), resume: make(chan struct{})}
		cfg.PageSize = uint64(f.pageSize)
		f.h, err = vmmemory.New(t.Context(), b, cfg, a, spill)
		if err != nil {
			t.Fatal(err)
		}
		r, m, _ := f.memoryRegion(1)
		access(t, r, m, 0, true)
		clear(m.pages)
		detached := make(chan error, 1)
		go func() { detached <- r.Detach(f.ctx) }()
		<-a.held
		if err := f.h.Close(f.ctx); err == nil || !strings.Contains(err.Error(), "still attached") {
			t.Fatalf("the pager closed under a detach still giving its pages back: %v", err)
		}
		close(a.resume)
		if err := <-detached; err != nil {
			t.Fatal(err)
		}
		if err := f.h.Close(f.ctx); err != nil {
			t.Fatalf("the pager did not close once the detach was done: %v", err)
		}
	})
}
