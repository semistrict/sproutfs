package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// Write-ahead is for fresh zeros and nothing else: a hole, or a page the guest
// has never touched, is shared with nobody, so making its neighbours private
// gives back no sharing at all. What it does cost is an arena slot and a dirty
// reservation each, and these are the tests that say what happens to them.
//
// They build their pager at 4 KiB explicitly rather than at whatever page the
// suite is running, because a run of thousands of pages is the RAM geometry's
// and a 2 MiB arena of that many slots is gigabytes of fixture.

// zeroAheadMemoryRegion is a memory region of holes at a stated run, with read-ahead and
// write-ahead both that run, which is what a production host gives each pager.
func zeroAheadMemoryRegion(t *testing.T, pages, run int) (*fixture, *vmmemory.MemoryRegion, *mapping, *backing) {
	t.Helper()
	return holeMemoryRegion(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
		ResidentPages: pages, LogicalPages: pages, DirtyPages: pages,
		ReadAheadPages: run, WriteAheadPages: run}, pages)
}

// A guest storing into one page of every 512 of a run of holes — the shape of a
// kernel spreading small structures over fresh memory — makes every page of
// every run it touches private, one run to a mapping command. It pays for them
// only until its next checkpoint: the pages it never stored into are published
// as holes, costing no object and not a byte uploaded, and the retire hands each
// one back with its arena slot and its dirty reservation, leaving the page as
// untouched as it was.
func TestZeroWriteAheadPagesTheGuestNeverStoredIntoAreGivenBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages, run = 4096, 512
		const stores = pages / run
		f, r, m, b := zeroAheadMemoryRegion(t, pages, run)
		for page := range uint64(stores) {
			access(t, r, m, page*run, true)[0] = byte(page + 1)
		}
		s := hostStats(t, f)
		if s.Faults != stores || s.CopyOnWrites != stores || m.maps != stores {
			t.Fatalf("%d stores took %d faults, %d private runs and %d mapping commands, want %d of each",
				stores, s.Faults, s.CopyOnWrites, m.maps, stores)
		}
		if s.WriteAheadPages != pages-stores || s.DirtyPages != pages || s.ResidentPages != pages {
			t.Fatalf("write-ahead %d, dirty %d, resident %d; want %d, %d, %d",
				s.WriteAheadPages, s.DirtyPages, s.ResidentPages, pages-stores, pages, pages)
		}
		if b.loads != 0 || m.revokes != 0 {
			t.Fatalf("the stores read the volume %d times and revoked %d mappings, want none of either", b.loads, m.revokes)
		}
		f.mustCheckpoint(r, b)
		s = hostStats(t, f)
		if s.CheckpointPages != pages || s.WriteAheadZeroPages != pages-stores {
			t.Fatalf("the checkpoint took %d pages of which %d were write-ahead zeroes, want %d and %d",
				s.CheckpointPages, s.WriteAheadZeroPages, pages, pages-stores)
		}
		if got, want := b.publishedPages.Load(), int64(stores); got != want {
			t.Fatalf("the checkpoint published %d pages, want the %d the guest stored into", got, want)
		}
		if got, want := b.publishedBytes.Load(), int64(stores*checkpoint.PageSize4KiB); got != want {
			t.Fatalf("the checkpoint published %d bytes, want %d", got, want)
		}
		if s.DirtyPages != 0 || s.ResidentPages != stores || len(m.pages) != stores {
			t.Fatalf("after the retire dirty %d, resident %d, mapped %d; want 0, %d, %d",
				s.DirtyPages, s.ResidentPages, len(m.pages), stores, stores)
		}
		// Every page is what it was: the stores where the guest made them, and a
		// hole everywhere else, read without touching the volume.
		want := make([]byte, pages*checkpoint.PageSize4KiB)
		for page := range uint64(stores) {
			want[page*run*checkpoint.PageSize4KiB] = byte(page + 1)
		}
		requireBytes(t, r, m, want, checkpoint.PageSize4KiB)
		if b.loads != 0 {
			t.Fatalf("reading the memory region back read the volume %d times, want none: every page is a hole or resident", b.loads)
		}
	})
}

// A page a retire handed back is a hole again, so the next store into it is a
// fresh-zero store like the first: one fault, one mapping command, a whole run
// private, and nothing read from the volume.
func TestAGivenBackZeroAheadPageStoresAsAHoleAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages, run = 1024, 256
		f, r, m, b := zeroAheadMemoryRegion(t, pages, run)
		access(t, r, m, 0, true)[0] = 1
		f.mustCheckpoint(r, b)
		if s := hostStats(t, f); s.ResidentPages != 1 || s.DirtyPages != 0 {
			t.Fatalf("after the retire resident %d, dirty %d; want 1 and 0", s.ResidentPages, s.DirtyPages)
		}
		maps, loads := m.maps, b.loads
		before := hostStats(t, f)
		access(t, r, m, run/2, true)[0] = 2
		after := hostStats(t, f)
		if got := m.maps - maps; got != 1 {
			t.Fatalf("the second store issued %d mapping commands, want 1", got)
		}
		if got := after.Faults - before.Faults; got != 1 {
			t.Fatalf("the second store took %d faults, want 1", got)
		}
		// The run runs to the end of the faulting page's read-ahead window and
		// then back towards its start, stopping at the page the first checkpoint
		// published, which is not a hole.
		if got, want := after.WriteAheadPages-before.WriteAheadPages, uint64(run-2); got != want {
			t.Fatalf("the second store wrote %d pages ahead, want %d", got, want)
		}
		if b.loads != loads {
			t.Fatalf("the second store read the volume %d times, want none", b.loads-loads)
		}
	})
}
