package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The tests in this file each put another goroutine between two holds of a
// lock in an isolated arena's paths, where the work before the release looked
// at something the work after relies on (TASK-108).

// forkedTwice is a parent whose pages 0 and 1 hold 44 and 45 at their first
// byte, sealed and shared as a fork point that lends both pages to its two
// children on this host. Each child's backing reaches the bytes the seal
// holds, as a child's does through the seal. The children attach before the
// point is shared, so neither maps a lent page before it faults.
type forkedTwice struct {
	parent *vmmemory.MemoryRegion
	pm     *mapping
	a, b   *vmmemory.MemoryRegion
	am, bm *mapping
	bb     *backing
}

func forkTwice(t *testing.T, f *fixture) forkedTwice {
	t.Helper()
	var w forkedTwice
	w.parent, w.pm, _ = f.memoryRegion(4)
	accessUnder(f.ctx, t, w.parent, w.pm, 0, true)[0] = 44
	accessUnder(f.ctx, t, w.parent, w.pm, 1, true)[0] = 45
	point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
	child := func() (*vmmemory.MemoryRegion, *mapping, *backing) {
		cb := f.newBacking(4)
		cb.source = point
		cb.data[0], cb.data[f.pageSize] = 44, 45
		r, m := f.attach(cb)
		return r, m, cb
	}
	w.a, w.am, _ = child()
	w.b, w.bm, w.bb = child()
	if err := w.parent.Seal(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.parent.Checkpoint().Share(f.ctx, point, "v"); err != nil {
		t.Fatal(err)
	}
	return w
}

// endTheSealInAForkCopy has child b copy page 0 while the parent detaches,
// with the seal ending while b's copy is at the seam set installs. Child a
// copies page 1 first and waits at the same seam, so the detach gives page 0
// up at once and then waits for page 1. Once b's copy reaches the seam, a's
// goes on, and the detach ends the seal and finishes before b's copy does.
// It reports what b reads of page 0.
func endTheSealInAForkCopy(t *testing.T, f *fixture, w forkedTwice, set func(*testing.T, func(uint64))) byte {
	t.Helper()
	release := make(chan struct{})
	detached := make(chan error, 1)
	set(t, func(page uint64) {
		if page == 1 {
			<-release
			return
		}
		close(release)
		synctest.Wait()
		requireOver(t, "the parent's detach", detached)
	})
	copied := make(chan error, 1)
	go func() { copied <- w.a.Fault(f.ctx, 1, false) }()
	synctest.Wait()
	// The parent's VMM has gone, as a detach requires.
	f.a.mu.Lock()
	clear(w.pm.pages)
	f.a.mu.Unlock()
	go func() { detached <- w.parent.Detach(f.ctx) }()
	synctest.Wait()
	got := accessUnder(f.ctx, t, w.b, w.bm, 0, false)[0]
	requireOver(t, "the other child's copy", copied)
	return got
}

// requireOver reports a goroutine's error, and fails the test where it has
// not finished.
func requireOver(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	default:
		t.Fatalf("%s has not finished", what)
	}
}

// unheldFilesOpen counts the files the arena still holds open that none of
// mappings holds: a file the pager keeps for nobody.
func unheldFilesOpen(f *fixture, mappings ...*mapping) int {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	held := make(map[*arenaFile]bool)
	for _, m := range mappings {
		for _, file := range m.files {
			held[file] = true
		}
	}
	open := 0
	for _, file := range f.a.files {
		if !file.closed && !held[file] {
			open++
		}
	}
	return open
}

// A child copies a page its fork point lends while the parent detaches, and
// the end of the seal falls between the copy being made and its taking the
// lent page's place. The point no longer lends, so the copy goes back and the
// child reads its own volume; the point's file goes back with the seal.
// Before TASK-108 the copy took the lent page's place first and became one of
// the point's copies only after: the end of the seal missed it, gave it back
// with the root and the point's file with it, and the child was then given
// the file the point no longer had, which panicked on its holders.
func TestAForkCopyWhosePointsSealEndsBeforeItIsKeptGoesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8})
		w := forkTwice(t, f)
		if got := endTheSealInAForkCopy(t, f, w, vmmemory.SetForkCopySeam); got != 44 {
			t.Fatalf("the child reads %d, want the 44 the seal held", got)
		}
		if s := hostStats(t, f); s.ForkCopies != 1 || w.bb.loads != 1 {
			t.Fatalf("%d fork copies and %d reads of the child's volume, want 1 and 1", s.ForkCopies, w.bb.loads)
		}
		if open := unheldFilesOpen(f, w.am, w.bm); open != 0 {
			t.Fatalf("%d files no child holds are open after the seal ended, want none", open)
		}
		if got := accessUnder(f.ctx, t, w.a, w.am, 1, false)[0]; got != 45 {
			t.Fatalf("the other child reads %d after the seal ended, want 45", got)
		}
	})
}

// A child copies a page its fork point lends while the parent detaches, and
// the end of the seal falls between its look at the point and its making the
// point's file. The point no longer lends, so no file is made and the child
// reads its own volume. Before TASK-108 a file was made for the point after
// its seal had ended, and nothing ever gave it back.
func TestAForkCopyMakesNoFileForAPointWhoseSealEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8})
		w := forkTwice(t, f)
		if got := endTheSealInAForkCopy(t, f, w, vmmemory.SetForkFileSeam); got != 44 {
			t.Fatalf("the child reads %d, want the 44 the seal held", got)
		}
		if s := hostStats(t, f); s.ForkCopies != 1 || w.bb.loads != 1 {
			t.Fatalf("%d fork copies and %d reads of the child's volume, want 1 and 1", s.ForkCopies, w.bb.loads)
		}
		if open := unheldFilesOpen(f, w.am, w.bm); open != 0 {
			t.Fatalf("%d files no child holds are open after the seal ended, want none", open)
		}
	})
}

// An inheritor's move finds a published page its owner's VMM changed and
// takes the page from its root, and the owner detaches between that and the
// look at whether the owner still maps it. The page goes back with the
// owner's private file, and the inheritor reads its own volume. Before
// TASK-108 the look came before the detach, and the page was put back in the
// layer the detach had destroyed, which panicked.
func TestAnUnindexWhoseOwnerDetachesGivesThePageBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.memoryRegion(4)
		accessUnder(f.ctx, t, a, am, 0, true)[0] = 77
		f.mustCheckpoint(a, ab)
		private := am.pages[0].file
		f.a.mu.Lock()
		f.a.page(am.pages[0].place)[0] = 13
		f.a.mu.Unlock()
		detached := make(chan error, 1)
		vmmemory.SetUnindexSeam(t, func(uint64) {
			// The owner's VMM has gone, as a detach requires.
			f.a.mu.Lock()
			clear(am.pages)
			f.a.mu.Unlock()
			go func() { detached <- a.Detach(f.ctx) }()
			synctest.Wait()
			requireOver(t, "the owner's detach", detached)
		})
		bb := f.inheritor(ab)
		b, bm := f.attach(bb)
		if got := accessUnder(f.ctx, t, b, bm, 0, false)[0]; got != 77 {
			t.Fatalf("the inheritor reads %d, want the 77 the checkpoint published", got)
		}
		if s := hostStats(t, f); s.Tampered != 1 || s.MovedPages != 0 || bb.loads != 1 {
			t.Fatalf("tampered %d, moved %d, volume reads %d; want 1, 0 and 1", s.Tampered, s.MovedPages, bb.loads)
		}
		f.a.mu.Lock()
		closed := f.a.files[private].closed
		f.a.mu.Unlock()
		if !closed {
			t.Fatal("the detached owner's private file outlived the page that left its root")
		}
	})
}
