package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// revokeHookedMapping is a test mapping that runs a hook before each
// revocation, which is where a test puts a child's fault while its parent's
// pager holds a page the child maps.
type revokeHookedMapping struct {
	*mapping
	onRevoke func(page uint64)
}

func (m *revokeHookedMapping) Revoke(ctx context.Context, page uint64) error {
	if m.onRevoke != nil {
		m.onRevoke(page)
	}
	return m.mapping.Revoke(ctx, page)
}

// attachRevokeHooked maps one backing as guest RAM through a
// revokeHookedMapping.
func (f *fixture) attachRevokeHooked(b vmmemory.Backing) (*vmmemory.MemoryRegion, *revokeHookedMapping) {
	f.t.Helper()
	m := &revokeHookedMapping{mapping: newMapping(f.a)}
	f.a.mappings = append(f.a.mappings, m.mapping)
	r, err := f.h.Attach(f.ctx, ram(b), m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		clear(m.pages)
		if err := r.Detach(context.Background()); err != nil {
			f.t.Error(err)
		}
	})
	return r, m
}

// lendingParent is a parent memory region of two pages, 44 and 55 stored into
// their first bytes, sealed, with a fork point lending the sealed pages under
// point. Its child's backing reads the bytes the point gave those pages, as
// a child's volume does once the point is published.
func lendingParent(t *testing.T, f *fixture) (parent *vmmemory.MemoryRegion, pm *mapping, pb *backing,
	child *backing) {
	t.Helper()
	parent, pm, pb = f.memoryRegion(2)
	access(t, parent, pm, 0, true)[0] = 44
	access(t, parent, pm, 1, true)[0] = 55
	if err := parent.Seal(f.ctx); err != nil {
		t.Fatal(err)
	}
	child = f.newBacking(2)
	child.source = control.Ref{VM: f.source.VM + "-point", Sequence: 7}
	child.data[0], child.data[f.pageSize] = 44, 55
	return parent, pm, pb, child
}

// share lends the parent's sealed pages under the child's point.
func share(t *testing.T, f *fixture, parent *vmmemory.MemoryRegion, child *backing) {
	t.Helper()
	if err := parent.Checkpoint().Share(f.ctx, child.source, "v"); err != nil {
		t.Fatal(err)
	}
}

// pointChild is the backing of another child of the point child inherits
// from, which reads what child reads.
func pointChild(f *fixture, child *backing) *backing {
	sibling := f.newBacking(len(child.data) / f.pageSize)
	sibling.source = child.source
	copy(sibling.data, child.data)
	return sibling
}

// A retire that gives back a page a fork point lends takes the point's name
// for it away under the page's lock, so a child that faults on the page while
// the retire goes on reads it through its own volume. Before 2026-10-07 the
// name stayed until the seal ended, and a child that faulted on it in between
// mapped the slot the retire had just given back.
func TestAChildFaultingDuringItsParentsRetireMapsNoPageTheRetireGaveBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 1})
		vmmemory.SetCheckpointBatchPages(t, 1)
		parent, pm, pb, cb := lendingParent(t, f)
		child, cm := f.attach(cb)
		share(t, f, parent, cb)
		// The guest stores into page 0 again: the checkpoint alone holds the
		// page the point lends, and the retire gives it back.
		access(t, parent, pm, 0, true)[0] = 46
		published, err := f.publishCheckpoint(f.ctx, parent, pb)
		if err != nil {
			t.Fatal(err)
		}
		var faulted error
		entered := false
		pb.onLocate = func(offset, _ uint64) {
			if offset != uint64(f.pageSize) || entered {
				return
			}
			// The retire's second batch looks its page up with nothing held,
			// after the first gave page 0 back.
			entered = true
			faulted = child.Fault(f.ctx, 0, false)
		}
		if err := parent.Checkpoint().Retire(f.ctx, published); err != nil {
			t.Fatal(err)
		}
		if !entered || faulted != nil {
			t.Fatalf("the child's fault during the retire ran %t and returned %v, want it run and served", entered, faulted)
		}
		if got := access(t, child, cm, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
		if got := access(t, parent, pm, 0, false)[0]; got != 46 {
			t.Fatalf("the parent reads %d at page 0, want its own 46", got)
		}
	})
}

// An unseal that hands a page a fork point lends back to its guest takes the
// point's name for it away under the page's lock, so a child that faults on
// the page while the unseal goes on reads it through its own volume and never
// sees what the parent stores there next. Before 2026-10-07 the name stayed
// until the seal ended, and the child mapped the parent's own dirty page.
func TestAChildFaultingDuringItsParentsUnsealNeverSeesThePagesParentStoresNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The child maps the parent's own page, which is what a shared arena does.
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, pm, _, cb := lendingParent(t, f)
		child, cm := f.attachRevokeHooked(cb)
		share(t, f, parent, cb)
		for page, want := range []byte{44, 55} {
			if got := access(t, child, cm.mapping, uint64(page), false)[0]; got != want {
				t.Fatalf("the child reads %d at page %d, want the %d the point lends", got, page, want)
			}
		}
		faulted := make(chan error, 1)
		cm.onRevoke = func(page uint64) {
			switch page {
			case 0:
				// The unseal takes page 0 from the child, holding it: the
				// child faults on it again at once, and waits for it.
				go func() { faulted <- child.Fault(f.ctx, 0, false) }()
			case 1:
				// The unseal has given page 0 back to the parent and holds
				// page 1: the child's fault on page 0 goes on to its end.
				synctest.Wait()
			}
		}
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		cm.onRevoke = nil
		if err := <-faulted; err != nil {
			t.Fatal(err)
		}
		access(t, parent, pm, 0, true)[0] = 99
		if got := access(t, child, cm.mapping, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
	})
}

// A detach that drops a page a fork point lends takes the point's name for it
// away under the page's lock, so a child that faults on the page while the
// detach goes on reads it through its own volume. Before 2026-10-07 the name
// stayed until the seal ended, the child mapped the page again, and the
// detach's layer gave back a page the child mapped: the pager panicked.
func TestAChildFaultingDuringItsParentsDetachMapsNoPageTheDetachDrops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, pm, _, cb := lendingParent(t, f)
		child, cm := f.attachRevokeHooked(cb)
		share(t, f, parent, cb)
		for page := range uint64(2) {
			access(t, child, cm.mapping, page, false)
		}
		faulted := make(chan error, 1)
		cm.onRevoke = func(page uint64) {
			switch page {
			case 0:
				go func() { faulted <- child.Fault(f.ctx, 0, false) }()
			case 1:
				synctest.Wait()
			}
		}
		clear(pm.pages)
		if err := parent.Detach(f.ctx); err != nil {
			t.Fatal(err)
		}
		cm.onRevoke = nil
		if err := <-faulted; err != nil {
			t.Fatal(err)
		}
		if got := access(t, child, cm.mapping, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point gave it", got)
		}
	})
}

// The end of a fork point's seal leaves a page a child read under the point's
// name where it is: in an arena that shares it, that is the point's root,
// which stays the root of the name, and the next child maps the page without
// a read. Before 2026-10-07 the end destroyed a root the point had not
// published, with every page in it, and the pager panicked freeing the page
// the child mapped.
func TestTheEndOfASealLeavesThePagesAChildReadUnderItsPointsName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaShared, ResidentPages: 16, LogicalPages: 32,
			DirtyPages: 8, ReadAheadPages: 1})
		parent, _, _, lent := lendingParent(t, f)
		share(t, f, parent, lent)
		// The child's page 2 is under the point's name too, and the parent,
		// two pages long, lends nothing there: the child reads it through its
		// own volume.
		cb := f.newBacking(3)
		cb.source = lent.source
		cb.data[2*f.pageSize] = 77
		child, cm := f.attach(cb)
		if got := access(t, child, cm, 2, false)[0]; got != 77 || cb.loads != 1 {
			t.Fatalf("the child reads %d at page 2 after %d reads, want 77 read once", got, cb.loads)
		}
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := access(t, child, cm, 2, false)[0]; got != 77 {
			t.Fatalf("the child reads %d at page 2 after the seal ended, want its 77", got)
		}
		sibling := pointChild(f, cb)
		next, nm := f.attach(sibling)
		if got := access(t, next, nm, 2, false)[0]; got != 77 || sibling.loads != 0 {
			t.Fatalf("the next child reads %d at page 2 after %d reads, want the 77 its sibling read, mapped",
				got, sibling.loads)
		}
	})
}

// A Share after its seal has ended lends nothing: a child of the point reads
// what it inherited through its own backing, and its siblings map what it
// read, as any read page. Before 2026-10-07 such a Share named the point's
// root for a seal that had ended and would never take it back, and in an
// isolated arena every child of the point read each of its pages alone.
func TestAShareAfterItsSealEndedLendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 1})
		parent, _, _, cb := lendingParent(t, f)
		checkpoint := parent.Checkpoint()
		if err := parent.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := checkpoint.Share(f.ctx, cb.source, "v"); err != nil {
			t.Fatal(err)
		}
		sibling := pointChild(f, cb)
		first, fm := f.attach(cb)
		if got := access(t, first, fm, 0, false)[0]; got != 44 || cb.loads != 1 {
			t.Fatalf("the first child reads %d at page 0 after %d reads, want 44 read once", got, cb.loads)
		}
		second, sm := f.attach(sibling)
		if got := access(t, second, sm, 0, false)[0]; got != 44 || sibling.loads != 0 {
			t.Fatalf("the second child reads %d at page 0 after %d reads, want the 44 the first read, mapped",
				got, sibling.loads)
		}
	})
}
