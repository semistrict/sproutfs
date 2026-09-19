package vmmemory_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
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
		a, am, _ := f.region(4)
		b, bm, _ := f.region(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "four pages shared by two regions")
		// The whole of what the guest does: it takes the page writable and
		// stores not one byte into it.
		access(t, a, am, 0, true)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if unchanged := f.settle(a); unchanged != 1 {
			t.Fatalf("the settle found %d unchanged pages, want exactly one", unchanged)
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
		wantRegion(t, a, 4, 0, 4, "the region that faulted and stored nothing")
		wantRegion(t, b, 4, 0, 4, "the region that never faulted for writing")
		if am.pages[0].slot != bm.pages[0].slot {
			t.Fatal("the re-shared page does not share its sibling's resident page again")
		}
		// The page is mapped, not missing: a read of it takes no fault at all.
		before := hostStats(t, f).Faults
		if got := access(t, a, am, 0, false)[0]; got != 1 {
			t.Fatalf("the re-shared page reads %d, want the byte the volume holds", got)
		}
		if after := hostStats(t, f).Faults; after != before {
			t.Fatalf("reading the re-shared page took %d faults, want none", after-before)
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
		a, am, ab := f.region(4)
		b, bm, _ := f.region(4)
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
		a, am, ab := f.region(4)
		b, bm, _ := f.region(4)
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
		wantRegion(t, a, 4, 1, 3, "the region that stored after the seal")
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
		a, am, _ := f.region(4)
		access(t, a, am, 0, false)
		access(t, a, am, 0, true)
		// Two slots hold the origin and the copy; another region's fault takes
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
// settled against anything: the region's next checkpoint is what makes them
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
		f := newFixture(t, 8, 32, 8)
		parent, pm, _ := f.region(4)
		access(t, parent, pm, 0, true)[0] = 44
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The fork point takes a reference of its own and names the parent's
		// sealed pages under it, so a child on this host maps them.
		point := control.Ref{VM: t.Name() + "-point", Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), point, "v"); err != nil {
			t.Fatal(err)
		}
		cb := f.newBacking(4)
		cb.source = point
		child, cm := f.attach(cb)
		if access(t, child, cm, 0, false)[0] != 44 {
			t.Fatal("the child did not inherit the page the fork point named")
		}
		if cm.pages[0].slot != pm.pages[0].slot {
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

// A region whose only private pages turn out to be unchanged holds no
// unpublished write once the settle has run, so the loss window that was
// holding its guest back ends there rather than at the publication.
func TestTheLossWindowEndsWhenEveryPrivatePageWasUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32,
			DirtyPages: 8, LossWindow: lossWindow})
		a, am, _ := f.region(4)
		b, bm, _ := f.region(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		f.h.SetPressure(vmmemory.Pressure{Checkpoint: func(*vmmemory.Region) bool { return true }})
		access(t, a, am, 0, true)
		time.Sleep(lossWindow + time.Second)
		stored := make(chan error, 1)
		go func() { stored <- a.Fault(t.Context(), 1, true) }()
		synctest.Wait()
		select {
		case err := <-stored:
			t.Fatalf("a store past the loss window did not wait: %v", err)
		default:
		}
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
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
			a, am, _ := f.region(16)
			b, bm, _ := f.region(16)
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
