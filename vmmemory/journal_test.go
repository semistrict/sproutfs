package vmmemory_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// pmemRegion attaches a persistent disk of pages pages, the only kind the
// journal holds.
func (f *fixture) pmemRegion(pages int) (*vmmemory.MemoryRegion, *mapping, *backing) {
	f.t.Helper()
	b := f.newBacking(pages)
	r, m := f.attachKind(vmmemory.Pmem, b)
	return r, m, b
}

// storeAt stores value at offset of page, as the guest does: a page it cannot
// store into without a fault is faulted for writing first.
func (f *fixture) storeAt(r *vmmemory.MemoryRegion, m *mapping, page, offset uint64, value byte) {
	f.t.Helper()
	data := accessUnder(f.ctx, f.t, r, m, page, true)
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	data[offset] = value
}

// blockOf is the number of the block that holds offset of page.
func blockOf(page, offset uint64) uint64 {
	return (page*uint64(pageSize) + offset) / vmmemory.BlockBytes
}

// blocksPerPage is how many blocks a page of the suite holds.
func blocksPerPage() int { return pageSize / vmmemory.BlockBytes }

// capture takes every page a flush now has to cover.
func (f *fixture) capture(r *vmmemory.MemoryRegion) *vmmemory.Captured {
	f.t.Helper()
	c, err := r.Capture(f.ctx, r.Unjournaled())
	if err != nil {
		f.t.Fatalf("capturing: %v", err)
	}
	return c
}

// writableIsUnjournaled fails the test where the guest may store into a page
// without a fault and the region does not count it unjournaled.
func writableIsUnjournaled(t *testing.T, r *vmmemory.MemoryRegion, m *mapping) {
	t.Helper()
	if err := unjournaledWritable(r, m); err != nil {
		t.Fatal(err)
	}
}

// unjournaledWritable reports a page the guest may store into without a fault
// that the region does not count unjournaled.
func unjournaledWritable(r *vmmemory.MemoryRegion, m *mapping) error {
	unjournaled := r.Unjournaled()
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	for page, p := range m.pages {
		if p.writable && !slices.Contains(unjournaled, page) {
			return fmt.Errorf("page %d is mapped writable and not unjournaled (unjournaled: %v)", page, unjournaled)
		}
	}
	return nil
}

// A capture writes the blocks the guest changed and nothing else of the page:
// a copy of a volume's page takes its digests from the page it was copied
// from.
func TestACaptureTakesOnlyTheBlocksTheGuestChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(2)
		offset := uint64(pageSize - 1)
		f.storeAt(r, m, 1, offset, 0xee)
		writableIsUnjournaled(t, r, m)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{1}) {
			t.Fatalf("unjournaled pages %v after one store, want [1]", got)
		}
		c := f.capture(r)
		if want := []uint64{blockOf(1, offset)}; !slices.Equal(c.Blocks, want) {
			t.Fatalf("the capture took blocks %v, want %v", c.Blocks, want)
		}
		if len(c.Data) != vmmemory.BlockBytes || c.Data[vmmemory.BlockBytes-1] != 0xee || c.Data[0] != 2 {
			t.Fatalf("the captured block holds %d bytes ending %#x, starting %d; want one block ending 0xee, starting 2",
				len(c.Data), c.Data[len(c.Data)-1], c.Data[0])
		}
		if got := r.Unjournaled(); len(got) != 0 {
			t.Fatalf("unjournaled pages %v after a capture, want none", got)
		}
		if c := f.capture(r); len(c.Blocks) != 0 {
			t.Fatalf("a second capture with no store between took blocks %v, want none", c.Blocks)
		}
	})
}

// A capture write-protects what it took. The guest's next store traps, maps
// the page writable again with nothing copied, and makes it unjournaled, and
// the next capture takes only the block that store changed.
func TestAStoreAfterACaptureTrapsAndCopiesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(1)
		f.storeAt(r, m, 0, 0, 0x11)
		f.capture(r)
		if p, _ := m.mappedPage(0); p.writable {
			t.Fatal("the guest still maps a captured page writable")
		}
		copies := hostStats(t, f).CopyOnWrites
		offset := uint64(pageSize / 2)
		f.storeAt(r, m, 0, offset, 0x22)
		writableIsUnjournaled(t, r, m)
		if got := hostStats(t, f).CopyOnWrites; got != copies {
			t.Fatalf("the store after a capture made %d copies, want none", got-copies)
		}
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after the trapped store, want [0]", got)
		}
		c := f.capture(r)
		if want := []uint64{blockOf(0, offset)}; !slices.Equal(c.Blocks, want) {
			t.Fatalf("the capture after the trapped store took blocks %v, want %v", c.Blocks, want)
		}
	})
}

// A store the guest made before a seal is covered by a capture while the seal
// stands: the capture reads the sealed copy, which the guest still shares.
// The page was unjournaled at the seal, which dropped its digests, so the
// capture writes it whole. An abandoned checkpoint gives the page back
// unjournaled, again with no digests.
func TestACaptureDuringASealTakesTheSealedCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(1)
		offset := uint64(pageSize/2 + 7)
		f.storeAt(r, m, 0, offset, 0x33)
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v under the seal, want [0]", got)
		}
		c := f.capture(r)
		if len(c.Blocks) != blocksPerPage() || c.Data[offset] != 0x33 {
			t.Fatalf("the capture under the seal took %d blocks with byte %#x, want the whole page, %d, with 0x33",
				len(c.Blocks), c.Data[offset], blocksPerPage())
		}
		if err := r.Unseal(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after the abandon, want [0]", got)
		}
		if c := f.capture(r); len(c.Blocks) != blocksPerPage() {
			t.Fatalf("the capture after the abandon took %d blocks, want the whole page, %d", len(c.Blocks), blocksPerPage())
		}
	})
}

// A page the guest stores into after the seal leaves the seal's list: the
// capture takes the guest's own copy, and the checkpoint's publication then
// leaves nothing unjournaled.
func TestAStoreUnderASealMovesThePageOffTheList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, b := f.pmemRegion(1)
		f.storeAt(r, m, 0, 0, 0x44)
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		f.storeAt(r, m, 0, 1, 0x55)
		writableIsUnjournaled(t, r, m)
		c := f.capture(r)
		if len(c.Blocks) != blocksPerPage() {
			t.Fatalf("the capture of a copy made under the seal took %d blocks, want the whole page, %d",
				len(c.Blocks), blocksPerPage())
		}
		if c.Data[0] != 0x44 || c.Data[1] != 0x55 {
			t.Fatalf("the capture read %#x %#x, want the guest's 0x44 0x55", c.Data[0], c.Data[1])
		}
		f.finishCheckpoint(r, b)
		if got := r.Unjournaled(); len(got) != 0 {
			t.Fatalf("unjournaled pages %v after the publication, want none", got)
		}
	})
}

// A page the journal held at the seal keeps its digests: after the
// publication, a store into one block of it is captured as that block alone.
// A page that was unjournaled at the seal loses them, so a store that puts a
// block back to what an earlier entry held is still captured.
func TestASealKeepsOnlyTheDigestsOfJournaledPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, b := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0x61)
		f.storeAt(r, m, 1, 0, 0x71)
		f.capture(r)
		// Page 1 is unjournaled at the seal, page 0 is not.
		f.storeAt(r, m, 1, 0, 0x72)
		f.mustCheckpoint(r, b)
		offset := uint64(pageSize - 1)
		f.storeAt(r, m, 0, offset, 0x62)
		f.storeAt(r, m, 1, 0, 0x71)
		c := f.capture(r)
		want := []uint64{blockOf(0, offset), blockOf(1, 0)}
		if !slices.Equal(c.Blocks, want) {
			t.Fatalf("the capture after the publication took blocks %v, want %v", c.Blocks, want)
		}
	})
}

// A capture whose journal write failed gives its pages back as unjournaled
// with no digests: its entry may not be on the disk, so the next capture
// writes the pages whole.
func TestAFailedCaptureGivesItsPagesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(1)
		f.storeAt(r, m, 0, 0, 0x81)
		f.capture(r).Fail(f.ctx)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after a failed capture, want [0]", got)
		}
		if c := f.capture(r); len(c.Blocks) != blocksPerPage() {
			t.Fatalf("the capture after a failed one took %d blocks, want the whole page, %d", len(c.Blocks), blocksPerPage())
		}
	})
}

// A page made from zeros takes zero digests, so its first capture writes only
// the blocks the guest stored into.
func TestAPageMadeFromZerosTakesZeroDigests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		b := f.newBacking(1)
		b.zero[0] = true
		r, m := f.attachKind(vmmemory.Pmem, b)
		offset := uint64(pageSize - vmmemory.BlockBytes)
		f.storeAt(r, m, 0, offset, 0x91)
		c := f.capture(r)
		if want := []uint64{blockOf(0, offset)}; !slices.Equal(c.Blocks, want) {
			t.Fatalf("the capture of a page made from zeros took blocks %v, want %v", c.Blocks, want)
		}
	})
}

// A capture reads a page the pager spilled from the spill, and the guest's
// store into it after the capture still traps.
func TestACaptureReadsASpilledPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 2, 8, 8)
		r, m, _ := f.pmemRegion(4)
		f.storeAt(r, m, 0, 3, 0xa1)
		for page := uint64(1); page < 4; page++ {
			access(t, r, m, page, false)
		}
		if hostStats(t, f).Spills == 0 {
			t.Fatal("reading three more pages through two resident ones spilled nothing")
		}
		c := f.capture(r)
		if want := []uint64{blockOf(0, 3)}; !slices.Equal(c.Blocks, want) || c.Data[3] != 0xa1 {
			t.Fatalf("the capture of a spilled page took blocks %v with byte %#x, want %v with 0xa1", c.Blocks, c.Data[3], want)
		}
		f.storeAt(r, m, 0, 4, 0xa2)
		writableIsUnjournaled(t, r, m)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after storing into the reloaded page, want [0]", got)
		}
	})
}

// RAM is not journaled.
func TestRAMIsNotJournaled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, _, _ := f.memoryRegion(1)
		if _, err := r.Capture(f.ctx, nil); !errors.Is(err, vmmemory.ErrNotJournaled) {
			t.Fatalf("capturing RAM: %v, want ErrNotJournaled", err)
		}
	})
}

// A page unjournaled at the seal and stored into under it is written whole:
// once the checkpoint is selected, a replay starts from the sealed bytes, so
// a block the guest put back to what the journal held before the seal is
// still one the replay has to be given.
func TestAPageStoredIntoUnderTheSealIsWrittenWhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, b := f.pmemRegion(1)
		offset := uint64(pageSize - 1)
		f.storeAt(r, m, 0, offset, 0x10)
		f.capture(r)
		f.storeAt(r, m, 0, offset, 0x20)
		if err := r.Seal(f.ctx); err != nil {
			t.Fatal(err)
		}
		f.storeAt(r, m, 0, offset, 0x10)
		c := f.capture(r)
		if !slices.Contains(c.Blocks, blockOf(0, offset)) {
			t.Fatalf("the capture under the seal took blocks %v, without block %d, which the guest put back",
				c.Blocks, blockOf(0, offset))
		}
		f.finishCheckpoint(r, b)
	})
}
