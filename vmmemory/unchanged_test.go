package vmmemory_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A write fault is not always a store: KVM finishes a guest's cold read from a
// worker thread that always asks for the page writable, and an architecture can
// report a guest kernel's cache maintenance as a write. The page is copied
// either way, and the copy is not dirty if its bytes are the ones the page it
// was copied from still holds: the checkpoint publishes nothing for it and the
// guest goes back to sharing the page it came from.
func TestASealedPageThatNeverChangedIsNotPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "four pages shared by two memory regions")
		// The whole of what the guest does: it takes the page writable and
		// stores not one byte into it.
		access(t, a, am, 0, true)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// A settle runs with the guest running and holds neither the memory region nor
		// the window that serializes a page's mappings against one another, so
		// the only replacement it may issue is the one that installs no page
		// table and wakes nothing. Installing the origin over the page the guest
		// still maps is what corrupted the fan-out's children.
		maps, revokes := am.maps, am.revokes
		if unchanged := f.settle(a); unchanged != 1 {
			t.Fatalf("the settle found %d unchanged pages, want exactly one", unchanged)
		}
		if am.maps != maps {
			t.Fatalf("the settle issued %d mapping commands, want none: it may only revoke",
				am.maps-maps)
		}
		if am.revokes != revokes+1 {
			t.Fatalf("the settle issued %d revocations, want exactly one", am.revokes-revokes)
		}
		if pages := a.Checkpoint().DirtyPages(); len(pages) != 0 {
			t.Fatalf("the checkpoint publishes %v, want no page at all", pages)
		}
		stats := hostStats(t, f)
		if stats.UnchangedPages != 1 {
			t.Errorf("the pager counted %d unchanged pages, want exactly one", stats.UnchangedPages)
		}
		if stats.DirtyPages != 0 {
			t.Errorf("%d dirty reservations are still held, want none", stats.DirtyPages)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "after the settle")
		wantMemoryRegion(t, a, 4, 0, 4, "the memory region that faulted and stored nothing")
		wantMemoryRegion(t, b, 4, 0, 4, "the memory region that never faulted for writing")
		// The guest's mapping of the copy is taken away rather than swapped for
		// the origin underneath a running guest: replacing it in place is what
		// corrupted the fan-out's children. So the page is missing here, and the
		// guest's next access is one fault that maps the origin.
		if _, mapped := am.pages[0]; mapped {
			t.Fatal("the settle left the guest mapping the copy it released")
		}
		before := hostStats(t, f).Faults
		if got := access(t, a, am, 0, false)[0]; got != 1 {
			t.Fatalf("the re-shared page reads %d, want the byte the volume holds", got)
		}
		if after := hostStats(t, f).Faults; after != before+1 {
			t.Fatalf("reading the re-shared page took %d faults, want exactly one", after-before)
		}
		if am.pages[0].place != bm.pages[0].place {
			t.Fatal("the re-shared page does not share its sibling's resident page again")
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}

// A page the guest really did store into is published exactly as it is today:
// the settle compares it with the page it was copied from, finds the bytes
// changed, and leaves it in the checkpoint.
func TestASealedPageTheGuestChangedIsPublishedAsBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, ab := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		access(t, a, am, 0, true)[0] = 99
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(a); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages of a guest that stored, want none", unchanged)
		}
		pages := a.Checkpoint().DirtyPages()
		if len(pages) != 1 || pages[0] != 0 {
			t.Fatalf("the checkpoint publishes %v, want page 0 alone", pages)
		}
		if s := hostStats(t, f); s.UnchangedPages != 0 {
			t.Errorf("the pager counted %d unchanged pages, want none", s.UnchangedPages)
		}
		f.finishCheckpoint(a, ab)
		if ab.data[0] != 99 {
			t.Fatalf("the volume holds %d, want the byte the guest stored", ab.data[0])
		}
	})
}

// A store that lands between the seal and the settle copies the guest away from
// the checkpoint, as it does today. The sealed bytes are still the ones the
// origin holds, so the checkpoint publishes nothing for the page — and the
// guest keeps its own copy, which the next checkpoint publishes.
func TestAStoreBetweenTheSealAndTheSettleKeepsTheGuestsOwnPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, ab := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		access(t, a, am, 0, true)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		access(t, a, am, 0, true)[0] = 99
		if unchanged := f.settle(a); unchanged != 1 {
			t.Fatalf("the settle found %d unchanged pages, want exactly one", unchanged)
		}
		if pages := a.Checkpoint().DirtyPages(); len(pages) != 0 {
			t.Fatalf("the checkpoint publishes %v, want no page at all", pages)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		wantMemoryRegion(t, a, 4, 1, 3, "the memory region that stored after the seal")
		if got := access(t, a, am, 0, false)[0]; got != 99 {
			t.Fatalf("the page reads %d, want the byte the guest stored", got)
		}
		// The guest's page was copied from the checkpoint's own held copy, so it
		// remembers no origin and nothing compares it: the next checkpoint
		// publishes it like any stored page.
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(a); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages copied from the checkpoint's own copy, want none", unchanged)
		}
		f.finishCheckpoint(a, ab)
		if ab.data[0] != 99 {
			t.Fatalf("the volume holds %d, want the byte the guest stored", ab.data[0])
		}
	})
}

// The origin is a resident page like any other, and a pager short of slots
// takes it. A copy whose origin has been evicted has nothing to compare against,
// so its page is published exactly as it is today.
func TestACopyWhoseOriginWasEvictedIsPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 32, 8)
		a, am, _ := f.memoryRegion(4)
		access(t, a, am, 0, false)
		access(t, a, am, 0, true)
		// Two slots hold the origin and the copy; another memory region's fault takes
		// the least recently used of them, which is the origin.
		c, cm := f.attach(f.newUnrelatedBacking(2))
		access(t, c, cm, 0, false)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(a); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages whose origin was evicted, want none", unchanged)
		}
		pages := a.Checkpoint().DirtyPages()
		if len(pages) != 1 || pages[0] != 0 {
			t.Fatalf("the checkpoint publishes %v, want page 0 alone", pages)
		}
		if err := a.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// A page made from zeros was copied from nothing: no resident page holds bytes
// it could be compared with, so it is published whatever it holds. The same
// goes for a page copied from the checkpoint's own held copy, which is what the
// store above it makes.
func TestAPageMadeFromZerosIsNeverCompared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		b := f.newBacking(4)
		b.zero[1] = true
		clear(b.data[pageSize : 2*pageSize])
		r, m := f.attach(b)
		access(t, r, m, 1, true)
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(r); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages made from zeros, want none", unchanged)
		}
		pages := r.Checkpoint().DirtyPages()
		if len(pages) != 1 || pages[0] != 1 {
			t.Fatalf("the checkpoint publishes %v, want page 1 alone", pages)
		}
		if err := r.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// A page whose bytes exist only on the host that is handing this VM over is not
// the volume's, so nothing published holds them and no copy of one can be
// settled against anything: the memory region's next checkpoint is what makes them
// durable, whatever a write fault did to them first.
func TestAPageOnlyAnotherHostHoldsIsNeverCompared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 16, 8)
		base := f.newBacking(4)
		peer := &peerBacking{backing: base,
			unpublished: map[uint64]bool{1: true},
			served:      map[uint64]byte{1: 71}}
		r, m := f.attach(peer)
		access(t, r, m, 1, true)
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(r); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages only the source holds, want none", unchanged)
		}
		pages := r.Checkpoint().DirtyPages()
		if len(pages) != 1 || pages[0] != 1 {
			t.Fatalf("the checkpoint publishes %v, want page 1 alone", pages)
		}
		f.finishCheckpoint(r, base)
		if base.data[pageSize] != 71 {
			t.Fatalf("the volume holds %d for the source's page, want 71", base.data[pageSize])
		}
	})
}

// The name a fork point lends the parent's unpublished pages is not a published
// identity: nothing is published under a point's reference, ever, so a child
// that copies one of those pages has nothing its copy could be compared with
// and publishes it as its own.
func TestAPageCopiedFromAForkPointsNameIsNeverCompared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The child maps the parent's own page, which is what a shared arena does.
		f := newPinnedFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8})
		parent, pm, _ := f.memoryRegion(4)
		access(t, parent, pm, 0, true)[0] = 44
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The fork point takes a reference of its own and names the parent's
		// sealed pages under it, so a child on this host maps them.
		point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), point, "v"); err != nil {
			t.Fatal(err)
		}
		cb := f.newBacking(4)
		cb.source = point
		child, cm := f.attach(cb)
		if access(t, child, cm, 0, false)[0] != 44 {
			t.Fatal("the child did not inherit the page the fork point named")
		}
		if cm.pages[0].place != pm.pages[0].place {
			t.Fatal("the child did not map the parent's own page")
		}
		access(t, child, cm, 0, true)
		if err := child.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(child); unchanged != 0 {
			t.Fatalf("the settle dropped %d pages copied from a fork point's name, want none", unchanged)
		}
		pages := child.Checkpoint().DirtyPages()
		if len(pages) != 1 || pages[0] != 0 {
			t.Fatalf("the child publishes %v, want page 0 alone", pages)
		}
		if err := child.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := parent.Checkpoint().Retire(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// A memory region whose only private pages turn out to be unchanged holds no
// unpublished write once the settle has run, so the loss window that was
// holding its guest back ends there rather than at the publication.
func TestTheLossWindowEndsWhenEveryPrivatePageWasUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32,
			DirtyPages: 8, LossWindow: lossWindow})
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(*vmmemory.MemoryRegion) bool { return true }})
		access(t, a, am, 0, true)
		time.Sleep(lossWindow + time.Second)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		stored := make(chan error, 1)
		go func() { stored <- a.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store past the loss window did not wait for the sealed checkpoint: %v", err)
		default:
		}
		if unchanged := f.settle(a); unchanged != 1 {
			t.Fatalf("the settle found %d unchanged pages, want exactly one", unchanged)
		}
		if err := <-stored; err != nil {
			t.Fatalf("the store failed after the settle left nothing unpublished: %v", err)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}

// The settle divides a checkpoint's pages between workers that share nothing
// but the count and the set the checkpoint will list, so what it leaves does not
// depend on how many of them there are or on the order they finish in.
func TestTheSettleDoesNotDependOnItsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := func(workers int) ([]uint64, int) {
			t.Helper()
			f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 64, LogicalPages: 128,
				DirtyPages: 64, SettleWorkers: workers})
			a, am, _ := f.memoryRegion(16)
			b, bm, _ := f.memoryRegion(16)
			for page := uint64(0); page < 16; page++ {
				access(t, a, am, page, false)
				access(t, b, bm, page, false)
			}
			for page := uint64(0); page < 16; page++ {
				stored := access(t, a, am, page, true)
				if page%2 == 0 {
					stored[0] = byte(100 + page)
				}
			}
			if err := a.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			unchanged := f.settle(a)
			pages := a.Checkpoint().DirtyPages()
			if err := a.Checkpoint().Retire(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			return pages, unchanged
		}
		alone, aloneUnchanged := run(1)
		many, manyUnchanged := run(16)
		if aloneUnchanged != 8 || manyUnchanged != 8 {
			t.Fatalf("one worker found %d unchanged pages and sixteen found %d, want eight each",
				aloneUnchanged, manyUnchanged)
		}
		if len(alone) != 8 || len(many) != 8 {
			t.Fatalf("one worker published %v and sixteen published %v, want eight pages each", alone, many)
		}
		for i := range alone {
			if alone[i] != many[i] || alone[i]%2 != 0 {
				t.Fatalf("one worker published %v and sixteen published %v, want the even pages", alone, many)
			}
		}
	})
}

// A settle of many unchanged pages takes their mappings away in runs. At 4 KiB
// a guest's working set is thousands of pages and a settle re-shares most of
// them at every checkpoint, so a round trip per page is a stall the guest
// feels: the pages are revoked as one command per run of consecutive pages, as
// an abandoned checkpoint's are.
func TestASettleRevokesUnchangedPagesInRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 64
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 4 * pages,
			LogicalPages: 8 * pages, DirtyPages: 2 * pages, ReadAheadPages: 1,
			SettleWorkers: 4})
		// A sibling holds the same identities, so every page this memory region takes
		// writable has an origin to be compared with and re-shared onto.
		sibling, siblingMap, _ := f.memoryRegion(pages)
		for page := uint64(0); page < pages; page++ {
			access(t, sibling, siblingMap, page, false)
		}
		b := f.newBacking(pages)
		base := newMapping(f.a)
		m := &revokeBatchMapping{mapping: base}
		f.a.mappings = append(f.a.mappings, base)
		r, err := f.h.Attach(t.Context(), ram(b), m)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { clear(base.pages); _ = r.Detach(t.Context()) }()
		// The whole of what the guest does: it takes every page writable and
		// stores not one byte into any of them.
		for page := uint64(0); page < pages; page++ {
			access(t, r, base, page, true)
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		m.singles, m.batches = 0, 0
		if unchanged := f.settle(r); unchanged != pages {
			t.Fatalf("the settle found %d unchanged pages, want all %d", unchanged, pages)
		}
		// One command for the run, whatever order the workers compared it in.
		if m.batches != 1 || m.singles != 0 {
			t.Fatalf("the settle revoked %d contiguous pages with %d batched commands and %d single ones, want 1 and 0",
				pages, m.batches, m.singles)
		}
		if len(base.pages) != 0 {
			t.Fatalf("%d of the settled pages are still mapped to the guest", len(base.pages))
		}
		if got := r.Checkpoint().DirtyPages(); len(got) != 0 {
			t.Fatalf("the checkpoint publishes %d pages, want none", len(got))
		}
		if err := r.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}
