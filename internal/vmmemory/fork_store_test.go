package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// What a fork's first seconds are made of, and what they must not cost.
//
// A store replaces a mapping; it never takes one away. A copy-on-write has the
// copy to put in the mapping's place and the protocol's MAP over a range
// replaces whatever the pages of it had, so a fork reading memory it inherited —
// which on x86-64 reaches the pager as a store, because KVM finishes a fault
// that had to wait from a worker that asks for the page writable — costs one
// mapping command a window and no revocation at all.
//
// A GCE fan-out on 2026-09-23 spent 12,428 revocations on 12,826
// copy-on-writes, about one per page each fork wrote, so these are the shapes a
// fork's stores come in at a 4 KiB page: a memory region attached over a sibling's
// resident pages, a store into a page the guest has never touched, and a store
// into a page inside a window an earlier read brought.

// forkFixture is the shape a fork's RAM memory region has on a production host: a
// 4 KiB pager whose read-ahead run is one 2 MiB range, with an extent for every
// range its memory regions may write into and room for everything it admits. It is
// that pager whatever page the suite is running at, because a fork at a 2 MiB
// page has one page per range and no window to speak of.
func forkFixture(t *testing.T, pages int) *fixture {
	t.Helper()
	return newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: 4 * pages, ArenaOffsets: 8 * pages, LogicalPages: 4 * pages,
		DirtyPages: 4 * pages, ReadAheadPages: rangePages, WriteAheadPages: rangePages})
}

// revocations is how many revocation commands this pager has issued, which for
// a fork doing nothing but reading and storing must never move.
func revocations(t *testing.T, f *fixture) uint64 {
	t.Helper()
	return hostStats(t, f).Revocations
}

// privatePages is how many pages of one memory region hold bytes of its own, which is
// what a store is supposed to cost a fork: one, whatever its window brought.
func privatePages(t *testing.T, r *vmmemory.MemoryRegion) int {
	t.Helper()
	stats, err := r.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats.PrivatePages
}

func TestAForksFirstStoresRevokeNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 4 * rangePages
		f := forkFixture(t, pages)
		// The parent, whose pages a capture left resident: one read a range, each
		// bringing its whole window, so those pages are in the sharing index under
		// the identity the volume gives them.
		parent, pm, _ := f.memoryRegion(pages)
		for _, page := range []uint64{0, 2 * rangePages, 3 * rangePages} {
			access(t, parent, pm, page, false)
		}
		// The fork: a memory region of the same checkpoint, attached over those pages.
		// The populate maps the runs it finds resident before the guest runs.
		child, cm := f.attach(f.newBacking(pages))

		// Every store below is more than gapPages from the last, so no rule takes
		// a page with it and what each one costs is the page it faulted on alone.
		for _, item := range []struct {
			what string
			page uint64
		}{
			// A store into a page the populate mapped read-only from the
			// sibling's resident page.
			{"a page the attach populated", 8},
			// A store into a page of the same window, well away from the first.
			{"a page of a window the populate brought", 300},
			// A store into a page of a range the fork has never touched at all:
			// the store reads its window in, shared and read-only, and then
			// copies the one page it stored into.
			{"a page the guest has never touched", 2*rangePages + 8},
			// And one inside the window that store brought.
			{"a page inside the window a store read in", 2*rangePages + 300},
		} {
			before, owned := hostStats(t, f), privatePages(t, child)
			access(t, child, cm, item.page, true)[0] = byte(item.page)
			after := hostStats(t, f)
			if got := after.Revocations - before.Revocations; got != 0 {
				t.Errorf("a store into %s cost %d revocations, want 0", item.what, got)
			}
			// The window arrives read-only around the faulting page, so one page
			// of it is copied and the rest stay shared. The stores are far apart,
			// so no rule takes a page with them either.
			if got := privatePages(t, child) - owned; got != 1 {
				t.Errorf("a store into %s made %d pages private, want the one it stored into", item.what, got)
			}
			if got := after.RuleCopies - before.RuleCopies; got != 0 {
				t.Errorf("a store into %s copied %d pages for the rules, want 0", item.what, got)
			}
		}

		// A read of a page the fork has not touched, then a store into another
		// page of the window that read brought: the window is installed
		// read-only, and the store replaces its own page's mapping.
		access(t, child, cm, 3*rangePages+100, false)
		before, owned := hostStats(t, f), privatePages(t, child)
		access(t, child, cm, 3*rangePages+400, true)[0] = 9
		after := hostStats(t, f)
		if got := after.Revocations - before.Revocations; got != 0 {
			t.Errorf("a store into a page a read brought cost %d revocations, want 0", got)
		}
		if got := privatePages(t, child) - owned; got != 1 {
			t.Errorf("a store into a page a read brought made %d pages private, want 1", got)
		}
		// Every other page the windows brought is still the parent's: what a fork's
		// first pass over its memory copies is the pages it faulted on and nothing
		// else, which is the whole of what the read-only window is for.
		if got, want := privatePages(t, child), 5; got != want {
			t.Errorf("the fork owns %d pages after 5 stores, want %d", got, want)
		}
		if got := after.RuleCopies; got != 0 {
			t.Errorf("the fork's stores copied %d pages for the rules, want 0", got)
		}
	})
}

// A fork's stores cost no revocation, and its first checkpoint must not either.
// The pages that checkpoint gives back are the write-ahead pages the guest never
// stored into: the publication writes each as a hole, so the volume holds no
// object for it and the retire takes the page away from the guest with nothing
// to put in its place. A revocation is right there — it is what a retire is for
// — but it costs one command per run of consecutive pages, as a settle's and an
// abandoned checkpoint's do. One round trip per page, serialized on the mapping
// lock, is a stall the guest feels: it is where the 12,428 revocations a GCE
// fan-out of two forks recorded on 2026-09-23 against 15,477 write-ahead pages
// came from, one per page each retire handed back.
func TestAForksFirstCheckpointRevokesItsHolesInRunsNotPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Two windows of holes, one store into the first page of each: every
		// other page of both runs is a write-ahead page the guest never stored
		// into, so the retire hands back two runs of 511 pages.
		const pages, run = 2 * 512, 512
		const stores = pages / run
		const handedBack = pages - stores
		f, r, m, b := zeroAheadBatchMemoryRegion(t, pages, run)
		for page := range uint64(stores) {
			access(t, r, m.mapping, page*run, true)[0] = byte(page + 1)
		}
		if got := revocations(t, f); got != 0 {
			t.Fatalf("the stores cost %d revocations, want 0", got)
		}
		f.mustCheckpoint(r, b)
		s := hostStats(t, f)
		if s.WriteAheadZeroPages != handedBack || s.RevokedPages != handedBack {
			t.Fatalf("the retire handed back %d of %d write-ahead pages, want %d of each",
				s.RevokedPages, s.WriteAheadZeroPages, handedBack)
		}
		// One retire batch holds every one of them, and they are two runs of
		// consecutive pages, so the whole retire is one command over two runs.
		if s.Revocations != 1 || s.RevokeRuns != stores {
			t.Fatalf("the retire revoked %d pages with %d commands over %d runs, want 1 command over %d runs",
				s.RevokedPages, s.Revocations, s.RevokeRuns, stores)
		}
		if m.singles != 0 || m.batches != 1 {
			t.Fatalf("the retire used %d single revocations and %d batches, want 0 and 1", m.singles, m.batches)
		}
	})
}

// zeroAheadBatchMemoryRegion is zeroAheadMemoryRegion whose client revokes in batches, which
// is what the real one does: a pager talking to a client that can only revoke a
// page at a time cannot show what a run costs.
func zeroAheadBatchMemoryRegion(t *testing.T, pages, run int) (*fixture, *vmmemory.MemoryRegion, *revokeBatchMapping, *backing) {
	t.Helper()
	f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: pages, LogicalPages: pages, DirtyPages: pages,
		ReadAheadPages: run, WriteAheadPages: run})
	b := f.newBacking(pages)
	clear(b.data)
	for page := range uint64(pages) {
		b.zero[page] = true
	}
	base := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
	m := &revokeBatchMapping{mapping: base}
	f.a.mappings = append(f.a.mappings, base)
	r, err := f.h.Attach(t.Context(), ram(b), m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clear(base.pages)
		if err := r.Detach(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return f, r, m, b
}
