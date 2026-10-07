package vmmemory_test

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The store's races between two holds of a lock (TASK-108): each test puts
// another goroutine in the moment between a store's look and its act, which
// no seeded campaign reached.

// A rule copies a page of another read-ahead window only while no fault of
// that window runs: a store of that page in between would make it private a
// second time, and one of the two copies would be lost with its reservation.
// Before 2026-10-07 the gap rule took the pages beside a store whatever
// window they were in, and a store of one of them met it there.
func TestARuleNeverCopiesAPageAStoreOfItsOwnWindowIsCopying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, b := placedMemoryRegion(t, 2*rangePages)
		r.PressMappings()
		const first = 100
		held(t, r, m, first, first+gap+1)
		access(t, r, m, first, true)[0] = 7
		// The gap rule works down from the store at first+gap, so this is
		// the first page it copies. Every page is a window of its own here.
		const page = first + gap - 1
		var other sync.WaitGroup
		var stored error
		started := false
		vmmemory.SetRuleSeam(t, func(at uint64) {
			if at != page || started {
				return
			}
			started = true
			// The guest stores into the page the rule is about to copy, and
			// its fault goes as far as it can before the rule goes on.
			other.Go(func() { stored = r.Fault(f.ctx, page, true) })
			synctest.Wait()
		})
		accessUnder(f.ctx, t, r, m, first+gap, true)[0] = 9
		other.Wait()
		if stored != nil {
			t.Fatal(stored)
		}
		access(t, r, m, page, true)[0] = 8
		if found := f.h.Unreachable(); len(found) != 0 {
			t.Fatalf("pages nothing will give back: %v", found)
		}
		f.mustCheckpoint(r, b)
		want := map[uint64]byte{first: 7, page: 8, first + gap: 9}
		for at := uint64(first); at <= first+gap; at++ {
			value, stored := want[at]
			if !stored {
				value = byte(at + 1)
			}
			if got := b.data[at*uint64(f.pageSize)]; got != value {
				t.Fatalf("the checkpoint published %d at page %d, want %d", got, at, value)
			}
		}
	})
}

// A page a rule joins to a store's run because it is the region's own already
// is held until the store's command lands, so no eviction takes it from
// under the command and leaves the guest mapping a slot that went back.
// Before 2026-10-07 the rule only looked at the page, and an eviction between
// the look and the command made the command map a punched slot.
func TestAPageARuleJoinsToARunIsNotEvictedBeforeTheRunIsMapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, _ := placedMemoryRegion(t, 2*rangePages)
		held(t, r, m, 0, rangePages)
		// The store of the page that makes the range half private makes the
		// whole range private, and joins the pages stored into before it.
		const half = rangePages / 2
		for page := range uint64(half - 1) {
			access(t, r, m, page, true)[0] = 0x40
		}
		const joined = 100
		var eviction sync.WaitGroup
		var evicted bool
		var evictErr error
		started := false
		vmmemory.SetRuleSeam(t, func(at uint64) {
			if at != joined || started {
				return
			}
			started = true
			eviction.Go(func() { evicted, evictErr = vmmemory.EvictPage(f.ctx, r, joined) })
			synctest.Wait()
		})
		accessUnder(f.ctx, t, r, m, half-1, true)[0] = 0x40
		eviction.Wait()
		if evictErr != nil {
			t.Fatal(evictErr)
		}
		if !evicted {
			t.Fatalf("page %d was not evicted once the store's command landed", joined)
		}
		if got := hostStats(t, f).RuleCopies; got != rangePages-half {
			t.Fatalf("making the range whole copied %d pages, want %d", got, rangePages-half)
		}
		if got := access(t, r, m, joined, false)[0]; got != 0x40 {
			t.Fatalf("page %d reads %d after its eviction, want the %d the guest stored", joined, got, 0x40)
		}
	})
}

// A store whose page's own offset stops being its to have while it waits for
// room takes an ordinary slot, as it would have had it looked then. Before
// 2026-10-07 it went on waiting for that offset, evicting page after page of
// a shared arena until the region holding the last extent lost its page too.
func TestAStoreWhoseRangeLostItsExtentWhileItWaitedTakesAnOrdinarySlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A shared arena of 4 KiB pages with room for four pages and one
		// extent.
		f := newPinnedFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB, Arena: vmmemory.ArenaShared,
			ResidentPages: 4, ArenaOffsets: 2 * rangePages, LogicalPages: 8, DirtyPages: 8,
			ReadAheadPages: 1, WriteAheadPages: 1})
		a, am, _ := f.memoryRegion(4)
		other, om, _ := f.memoryRegion(4)
		for page := range uint64(4) {
			access(t, a, am, page, false)
		}
		taken := false
		vmmemory.SetAllocateSeam(t, func() {
			if taken {
				return
			}
			taken = true
			// The other region's store takes the arena's one extent while
			// this store waits for a page of room.
			access(t, other, om, 3, true)[0] = 0x55
		})
		accessUnder(f.ctx, t, a, am, 0, true)[0] = 0x77
		if !taken {
			t.Fatal("the store never waited for room")
		}
		if got := hostStats(t, f).Spills; got != 0 {
			t.Fatalf("the store spilled %d pages, want none: the other region's page had to stay", got)
		}
		if _, ok := om.mappedPage(3); !ok {
			t.Fatal("the other region's private page was evicted for the store")
		}
		if got := access(t, other, om, 3, false)[0]; got != 0x55 {
			t.Fatalf("the other region's page reads %d, want %d", got, 0x55)
		}
		if got := access(t, a, am, 0, false)[0]; got != 0x77 {
			t.Fatalf("the store's page reads %d, want %d", got, 0x77)
		}
	})
}

// The backstop of a refused mapping makes a range whole around the store's
// page only where that page is at its own offset. A store that copied away
// from a sealed page is not: its own offset holds the copy the checkpoint is
// publishing. So the backstop leaves that range alone, the store is refused,
// and the fault is served again once the process has room. Before 2026-10-07
// the backstop mapped the range as one run from its first offset, so the
// guest's page was mapped onto the checkpoint's copy and the guest's next
// store changed what the checkpoint published.
func TestARangeMadeWholeAroundACopyAwayFromACheckpointMapsTheGuestsOwnCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, r, m, b := placedMemoryRegion(t, 2*rangePages)
		const page = 100
		held(t, r, m, page-2, page+3)
		access(t, r, m, page, true)[0] = 7
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The guest shares the copy the seal froze at the page's own offset,
		// so its next store copies away from it. The process refuses that
		// store's mapping command, which is what reaches the backstop.
		commands := 0
		m.onMap = func(uint64, int) { commands++; m.refuseMap = commands == 1 }
		if err := r.Fault(f.ctx, page, true); !errors.Is(err, vmmemory.ErrMappingRefused) {
			t.Fatalf("the refused store returned %v, want %v", err, vmmemory.ErrMappingRefused)
		}
		// The process has room again, and the guest's store is served again.
		m.onMap, m.refuseMap = nil, false
		accessUnder(f.ctx, t, r, m, page, true)[0] = 9
		f.finishCheckpoint(r, b)
		if got := b.data[page*f.pageSize]; got != 7 {
			t.Fatalf("the checkpoint published %d at page %d, want the %d the guest stored before its seal", got, page, 7)
		}
		if got := access(t, r, m, page, false)[0]; got != 9 {
			t.Fatalf("page %d reads %d, want the %d the guest stored after the seal", page, got, 9)
		}
	})
}

// A rule never waits for a page: the store holds the region, and whatever
// holds the page may be waiting to take the region back. Here a read fault of
// another window holds the pages of its window it took from their root across
// its read, a seal waits for the store to give the region up, and the fault's
// retake of the region waits behind the seal. The rule ends the store's run
// at the held page. Before 2026-10-07 the rule waited for the page there, and
// the three waited on each other for ever.
func TestARuleEndsItsRunAtAPageAFaultOfAnotherWindowHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 2 * rangePages
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
			ResidentPages: 2 * pages, ArenaOffsets: 4 * pages, LogicalPages: 2 * pages, DirtyPages: 2 * pages,
			ReadAheadPages: 4, WriteAheadPages: 1})
		// A sibling makes the pages after 112 resident in their root, and
		// not 112 itself.
		sibling, sm, _ := f.memoryRegion(pages)
		for page := uint64(113); page < 116; page++ {
			access(t, sibling, sm, page, false)
		}
		if evicted, err := vmmemory.EvictPage(f.ctx, sibling, 112); err != nil || !evicted {
			t.Fatalf("evicting the sibling's page 112: %t %v", evicted, err)
		}
		r, m := f.attach(f.slowBacking(pages))
		access(t, r, m, 116, false)
		access(t, r, m, 100, true)[0] = 7
		r.PressMappings()
		// A fault in the window before 112's, so the fault at 112 follows it
		// and takes the rest of its window from the root.
		access(t, r, m, 108, false)
		var wg sync.WaitGroup
		var read, stored, sealed error
		wg.Go(func() { read = r.Fault(f.ctx, 112, false) })
		synctest.Wait()
		// The store at 116 closes the gap to 100, working down from 115,
		// which the fault holds.
		wg.Go(func() { stored = r.Fault(f.ctx, 116, true) })
		synctest.Wait()
		wg.Go(func() { sealed = r.Seal(f.ctx) })
		wg.Wait()
		if read != nil || stored != nil || sealed != nil {
			t.Fatalf("the read returned %v, the store %v and the seal %v, want none", read, stored, sealed)
		}
		if got := hostStats(t, f).RuleCopies; got != 0 {
			t.Fatalf("the store's rule copied %d pages, want none past the held page", got)
		}
		if p, ok := m.mappedPage(116); !ok || !p.mappedWritable {
			t.Fatalf("page 116 is mapped %t writable %t, want both", ok, p.mappedWritable)
		}
	})
}

// A store into a page a fork point names and does not hold, which the child
// does not map yet, makes the page the child's own. In an isolated arena such
// a page is read into the child's own file, since the name lasts only as long
// as the point's seal, so the store dirties the page it read where it is.
// Before 2026-10-07 the store copied it as it copies a root's page, the
// copy's supply lost to the page read, and the child mapped a slot that went
// back.
func TestAStoreIntoAPageAForkPointNamesIsTheChildsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 4
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 64, DirtyPages: 32,
			ReadAheadPages: 1})
		// The parent stores into its first two pages, which the point holds,
		// and names the other two, which it does not.
		parent, pm, _ := f.memoryRegion(pages)
		for page := range uint64(pages / 2) {
			access(t, parent, pm, page, true)[0] = byte(0x40 + page)
		}
		if err := parent.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point := control.Ref{VM: f.source.VM + "-point", Sequence: 7}
		if err := parent.Checkpoint().Share(t.Context(), point, "v"); err != nil {
			t.Fatal(err)
		}
		b := f.newBacking(pages)
		b.source = point
		for page := range pages / 2 {
			b.data[page*f.pageSize] = byte(0x40 + page)
		}
		child, cm := f.attach(b)
		accessUnder(f.ctx, t, child, cm, 3, true)[0] = 0x99
		for page, want := range map[uint64]byte{1: 0x41, 2: 3, 3: 0x99} {
			if got := access(t, child, cm, page, false)[0]; got != want {
				t.Fatalf("the child's page %d reads %#x, want %#x", page, got, want)
			}
		}
		if got := access(t, parent, pm, 3, false)[0]; got != 4 {
			t.Fatalf("the parent's page 3 reads %#x, want %#x", got, 4)
		}
	})
}
