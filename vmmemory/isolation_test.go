package vmmemory_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// isolatedFixture is a pager whose arena is split by who may read each page,
// whatever the suite's mode: the tests in this file are of what that split
// does.
func isolatedFixture(t *testing.T, cfg vmmemory.Config) *fixture {
	t.Helper()
	cfg.Arena = vmmemory.ArenaIsolated
	return newPinnedFixture(t, cfg)
}

// number is the number this mapping was given the file one of its pages maps
// under, -1 where the page maps no file.
func (m *mapping) number(page uint64) int {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	p, ok := m.pages[page]
	if !ok {
		return -1
	}
	for number, f := range m.files {
		if f.id == p.file {
			return number
		}
	}
	return -1
}

// inheritor is a backing that inherits what b's last checkpoint published, as
// a fork of that checkpoint does.
func (f *fixture) inheritor(b *backing) *backing {
	inherits := f.newBacking(len(b.data) / f.pageSize)
	copy(inherits.data, b.data)
	inherits.source = b.source
	return inherits
}

// A page two memory regions inherit is read into the shared file, which each
// maps read-only as its file 1. A store copies it into the storing region's own
// file, at the page's own offset, which is file 0 and the only one it maps
// writable. The other region never sees the store.
func TestAnIsolatedArenaPutsAPrivatePageInItsRegionsOwnFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		access(t, a, am, 2, false)
		access(t, b, bm, 2, false)
		if am.pages[2].place != bm.pages[2].place || am.number(2) != 1 || bm.number(2) != 1 {
			t.Fatalf("the regions map %+v as file %d and %+v as file %d, want one page of the shared file, file 1",
				am.pages[2], am.number(2), bm.pages[2], bm.number(2))
		}
		access(t, a, am, 2, true)[0] = 99
		if p := am.pages[2]; am.number(2) != 0 || p.slot != 2 || !p.writable {
			t.Fatalf("the store maps %+v as file %d, want slot 2 of its own file 0, writable", p, am.number(2))
		}
		if got := access(t, b, bm, 2, false)[0]; got != 3 {
			t.Fatalf("the other region reads %d after the store, want the 3 it inherited", got)
		}
		f.mustCheckpoint(a, ab)
		if ab.data[2*f.pageSize] != 99 {
			t.Fatalf("the checkpoint published %d, want 99", ab.data[2*f.pageSize])
		}
	})
}

// A page a checkpoint publishes stays in the private file it was published in,
// and its guest goes on mapping it there. When another memory region inherits
// it, the page is copied into the shared file, the copy is what both map, and
// the private slot goes back. The inheritor reads nothing from its volume.
func TestAPublishedPageMovesIntoTheSharedFileWhenAnotherRegionInheritsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.memoryRegion(4)
		access(t, a, am, 0, true)[0] = 77
		f.mustCheckpoint(a, ab)
		if am.number(0) != 0 {
			t.Fatalf("the published page is mapped from file %d, want the region's own file 0", am.number(0))
		}
		private := am.pages[0].place
		before := hostStats(t, f)
		bb := f.inheritor(ab)
		b, bm := f.attach(bb)
		if got := access(t, b, bm, 0, false)[0]; got != 77 {
			t.Fatalf("the inheritor reads %d, want the 77 the checkpoint published", got)
		}
		after := hostStats(t, f)
		if moved := after.MovedPages - before.MovedPages; moved != 1 || bb.loads != 0 || bm.number(0) != 1 {
			t.Fatalf("moved %d pages, read the volume %d times, mapped file %d; want 1, 0 and the shared file 1",
				moved, bb.loads, bm.number(0))
		}
		if _, mapped := am.pages[0]; mapped {
			t.Fatal("the owner still maps the page in its private file after the move")
		}
		if got := access(t, a, am, 0, false)[0]; got != 77 || am.pages[0].place != bm.pages[0].place {
			t.Fatalf("the owner reads %d from %+v, want 77 from the shared copy %+v", got, am.pages[0], bm.pages[0])
		}
		if f.a.page(private) != nil {
			t.Fatalf("the private slot %+v still holds the page after the move", private)
		}
	})
}

// A VMM holds a published page of its own read-only, so its stores copy it and
// its bytes stay what the upload read. One that writes the page anyway, through
// the private file it was given, is found when another memory region inherits
// the page: the copy's digest is not the upload's. Its session ends with
// ErrTampered, and the inheritor reads the published bytes from its own volume.
func TestAPublishedPageItsVMMChangedEndsThatVMMsSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.memoryRegion(4)
		access(t, a, am, 0, true)[0] = 77
		f.mustCheckpoint(a, ab)
		f.a.mu.Lock()
		f.a.page(am.pages[0].place)[0] = 13
		f.a.mu.Unlock()
		bb := f.inheritor(ab)
		b, bm := f.attach(bb)
		if got := access(t, b, bm, 0, false)[0]; got != 77 {
			t.Fatalf("the inheritor reads %d, want the 77 the checkpoint published", got)
		}
		s := hostStats(t, f)
		if s.Tampered != 1 || s.MovedPages != 0 || bb.loads != 1 || bm.number(0) != 1 {
			t.Fatalf("tampered %d, moved %d, volume reads %d, file %d; want 1, 0, 1 and the shared file 1",
				s.Tampered, s.MovedPages, bb.loads, bm.number(0))
		}
		if err := a.Fault(t.Context(), 1, false); !errors.Is(err, vmmemory.ErrTampered) {
			t.Fatalf("the owner's next fault = %v, want ErrTampered", err)
		}
	})
}

// A fork point lends its parent's sealed pages to a child on this host through
// a file of the point's own, which the child is given read-only. The page is
// copied there once, and a second child maps the same copy. The parent keeps
// its page and is never remapped. Ending the seal takes the copies away from
// the children and the file back from them.
func TestAForkPointLendsItsPagesThroughAFileOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8})
		parent, pm, _ := f.memoryRegion(4)
		access(t, parent, pm, 0, true)[0] = 44
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), point, "v"); err != nil {
			t.Fatal(err)
		}
		var children []*mapping
		var lent place
		for range 2 {
			cb := f.newBacking(4)
			cb.source = point
			child, cm := f.attach(cb)
			if got := access(t, child, cm, 0, false)[0]; got != 44 {
				t.Fatalf("the child reads %d, want the 44 the point lends", got)
			}
			if cm.number(0) != 2 || cm.pages[0].place == pm.pages[0].place || cb.loads != 0 {
				t.Fatalf("the child maps %+v as file %d after %d volume reads, want a copy in file 2 and no read",
					cm.pages[0], cm.number(0), cb.loads)
			}
			if lent != (place{}) && cm.pages[0].place != lent {
				t.Fatalf("the second child maps %+v, want the first child's copy %+v", cm.pages[0].place, lent)
			}
			lent = cm.pages[0].place
			children = append(children, cm)
		}
		if s := hostStats(t, f); s.ForkCopies != 1 {
			t.Fatalf("the point's page was copied %d times, want once", s.ForkCopies)
		}
		if err := parent.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		for i, cm := range children {
			if _, mapped := cm.pages[0]; mapped || cm.files[2] != nil {
				t.Fatalf("child %d still maps the lent page or holds the point's file after the seal ended", i)
			}
		}
		if !f.a.files[lent.file].closed {
			t.Fatal("the point's file was not given back when its seal ended")
		}
		if got := access(t, parent, pm, 0, false)[0]; got != 44 {
			t.Fatalf("the parent reads %d after its seal ended, want its own 44", got)
		}
	})
}

// A memory region that detaches leaves its private file behind while the file
// holds a published page nobody maps. The next memory region to inherit that
// page moves it out, and the file goes back with it.
func TestADetachedRegionsPrivateFileLastsAsLongAsItsIdlePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, ab := f.memoryRegion(4)
		access(t, a, am, 0, true)[0] = 5
		f.mustCheckpoint(a, ab)
		private := am.pages[0].file
		// Two pages are idle once it detaches: the one its store read in to copy
		// from, in the shared file, and the one it published, in its own.
		clear(am.pages)
		if err := a.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := hostStats(t, f); f.a.files[private].closed || s.IdlePages != 2 {
			t.Fatalf("the detached region's private file went with it (%t), taking the idle page it published (%d idle, %d resident)",
				f.a.files[private].closed, s.IdlePages, s.ResidentPages)
		}
		b, bm := f.attach(f.inheritor(ab))
		if got := access(t, b, bm, 0, false)[0]; got != 5 {
			t.Fatalf("the inheritor reads %d, want the 5 the detached region published", got)
		}
		if !f.a.files[private].closed {
			t.Fatal("the private file outlived its last page")
		}
	})
}

// A VMM can allocate memory in its own private file behind the pager's back.
// Verification finds it and ends the session. Memory a VMM's reads made the
// shared file allocate is given back instead, because it is nobody's.
func TestVerificationEndsARegionWhosePrivateFileHoldsMoreThanThePagerPut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := isolatedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 16, DirtyPages: 8, ReadAheadPages: 1})
		a, am, _ := f.memoryRegion(4)
		access(t, a, am, 1, false)
		access(t, a, am, 0, true)
		if err := a.Verify(t.Context()); err != nil {
			t.Fatalf("a region holding only what the pager put there failed verification: %v", err)
		}
		shared := f.a.files[am.pages[1].file]
		private := f.a.files[am.pages[0].file]
		f.a.mu.Lock()
		shared.put(shared.offsets-1, make([]byte, f.pageSize))
		f.a.mu.Unlock()
		if err := a.Verify(t.Context()); err != nil {
			t.Fatalf("a read of the shared file's hole failed the reader: %v", err)
		}
		if f.a.page(place{shared.id, shared.offsets - 1}) != nil {
			t.Fatal("the memory a read made the shared file allocate was not given back")
		}
		f.a.mu.Lock()
		private.put(3, make([]byte, f.pageSize))
		f.a.mu.Unlock()
		if err := a.Verify(t.Context()); !errors.Is(err, vmmemory.ErrUncounted) {
			t.Fatalf("verifying a private file holding a page the pager never put there = %v, want ErrUncounted", err)
		}
		if err := a.Fault(t.Context(), 2, false); !errors.Is(err, vmmemory.ErrUncounted) {
			t.Fatalf("the region's next fault = %v, want ErrUncounted", err)
		}
	})
}
