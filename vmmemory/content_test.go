package vmmemory_test

import (
	"testing"
	"testing/synctest"
)

// Every page a fault reads in, its own and the rest of its window a prefetch
// brings in behind it, holds the bytes its volume holds, and so does every
// copy a store makes, beside the guest's store into it. The faults run under
// the fixture's runtime, so the guard SPROUTFS_SIM_BUG names — one that
// makes a page without the bytes read or copied into it — is on in them.
func TestEveryPageAFaultReadsOrAStoreCopiesHoldsItsBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, aroundConfig())
		r, m, _ := aroundRegion(t, f, f.source, nil)
		// The region's first fault reads its page and prefetches its window.
		if err := r.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(8) {
			requirePage(t, m, page)
		}
		// A store into a page of the next window, which the region does not
		// map, copies the page the volume holds, which it reads in first. The
		// page it copied from is mapped by nothing then, and idle.
		before := hostStats(t, f)
		data := accessUnder(f.ctx, t, r, m, 9, true)
		if got := data[1]; got != 10 {
			t.Fatalf("the copy a store into page 9 made holds %d, want the volume's 10", got)
		}
		data[0] = 99
		if got := accessUnder(f.ctx, t, r, m, 9, false)[0]; got != 99 {
			t.Fatalf("page 9 reads %d after the store, want 99", got)
		}
		s := hostStats(t, f)
		if s.CopyOnWrites-before.CopyOnWrites != 1 || s.UnmappedCopyOnWrites-before.UnmappedCopyOnWrites != 1 ||
			s.IdlePages != 1 || s.DirtyPages != 1 {
			t.Fatalf("the store counted %d copies, %d of unmapped pages, and left %d idle and %d dirty; want 1, 1, 1 and 1",
				s.CopyOnWrites-before.CopyOnWrites, s.UnmappedCopyOnWrites-before.UnmappedCopyOnWrites,
				s.IdlePages, s.DirtyPages)
		}
		// A sibling reading page 9 maps the volume's bytes, not the copy, from
		// the page the store copied from, which is idle no more.
		sibling, sm, _ := aroundRegion(t, f, f.source, nil)
		if err := sibling.Fault(f.ctx, 9, false); err != nil {
			t.Fatal(err)
		}
		requirePage(t, sm, 9)
		if got := hostStats(t, f).IdlePages; got != 0 {
			t.Fatalf("with the sibling mapping it, %d pages are idle, want none", got)
		}
	})
}
