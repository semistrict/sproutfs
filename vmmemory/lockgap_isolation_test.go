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

// A child's copy of a page its fork point lends holds the end of the seal
// back: the parent detaches while the copy is under way, and its detach waits
// for the copy, then takes it back with the point's file. The end takes each
// page's lent name away under the page's lock, which the copy holds, so the
// copy never has to look again whether its point still lends: forkCopy relies
// on this.
func TestTheEndOfASealWaitsForAForkCopyUnderWay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8})
		parent, pm, _ := f.memoryRegion(4)
		accessUnder(f.ctx, t, parent, pm, 0, true)[0] = 44
		// The child attaches before the point is shared, so it maps no lent
		// page before it faults. Its backing reaches the bytes the seal holds,
		// as a child's does through the seal.
		point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
		cb := f.newBacking(4)
		cb.source = point
		cb.data[0] = 44
		child, cm := f.attach(cb)
		if err := parent.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint().Share(f.ctx, point, "v"); err != nil {
			t.Fatal(err)
		}
		copying, release := make(chan struct{}), make(chan struct{})
		vmmemory.SetForkCopySeam(t, func(uint64) {
			close(copying)
			<-release
		})
		faulted := make(chan error, 1)
		go func() { faulted <- child.Fault(f.ctx, 0, false) }()
		synctest.Wait()
		select {
		case <-copying:
		default:
			t.Fatal("the child's fault made no copy of the lent page")
		}
		// The parent's VMM has gone, as a detach requires.
		f.a.mu.Lock()
		clear(pm.pages)
		f.a.mu.Unlock()
		detached := make(chan error, 1)
		go func() { detached <- parent.Detach(f.ctx) }()
		synctest.Wait()
		select {
		case err := <-detached:
			close(release)
			t.Fatalf("the parent's detach finished (%v) while a child's copy of its page was under way", err)
		default:
		}
		close(release)
		synctest.Wait()
		requireOver(t, "the child's fault", faulted)
		requireOver(t, "the parent's detach", detached)
		if s := hostStats(t, f); s.ForkCopies != 1 || cb.loads != 0 {
			t.Fatalf("%d fork copies and %d reads of the child's volume, want 1 and none", s.ForkCopies, cb.loads)
		}
		if _, mapped := cm.mappedPage(0); mapped {
			t.Fatal("the child still maps the point's copy after the seal ended")
		}
		if open := unheldFilesOpen(f, cm); open != 0 {
			t.Fatalf("%d files the child does not hold are open after the seal ended, want none", open)
		}
		if got := accessUnder(f.ctx, t, child, cm, 0, false)[0]; got != 44 || cb.loads != 1 {
			t.Fatalf("the child reads %d after %d reads of its volume, want 44 from one", got, cb.loads)
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
