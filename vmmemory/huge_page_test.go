package vmmemory_test

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The two tests here are about the large page itself — what one 2 MiB unit of
// ownership costs a store and carries through a spill — so they name that page
// rather than running at whichever one the suite is exercising.
func TestHugePageIsSharedWholeAndCopiesWholePage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const huge = checkpoint.PageSize2MiB
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: huge, ResidentPages: 3, LogicalPages: 4, DirtyPages: 4})
		a := f.newBacking(1)
		for i, offset := range []int{0, 4096, 1 << 20, (2 << 20) - 4096, (2 << 20) - 1} {
			a.data[offset] = byte(31 + i)
		}
		want := bytes.Clone(a.data)
		r, m := f.attach(a)
		first := access(t, r, m, 0, false)
		firstByte, lastByte := first[0], first[huge-1]
		if a.loads != 1 || a.loadedBytes != huge {
			t.Fatalf("default huge-page read loaded %d bytes in %d reads", a.loadedBytes, a.loads)
		}
		siblingBacking := f.newBacking(1)
		copy(siblingBacking.data, want)
		sibling, sm := f.attach(siblingBacking)
		if len(sm.pages) != 1 || sm.pages[0].slot != m.pages[0].slot {
			t.Fatal("inherited huge page was not eagerly shared")
		}
		copy := access(t, sibling, sm, 0, true)
		if copy[0] != firstByte || copy[huge-1] != lastByte || !bytes.Equal(copy, want) {
			t.Fatal("copy lost part of huge page")
		}
		copy[huge-1] = 79
		if got := access(t, r, m, 0, false)[huge-1]; got != lastByte {
			t.Fatalf("sibling write escaped: %d", got)
		}
		// A checkpoint publishes whole pager pages, so one 2 MiB page is one page of
		// the checkpoint however many storage pages it becomes.
		if err := f.checkpoint(sibling, siblingBacking); err != nil {
			t.Fatal(err)
		}
		if got := siblingBacking.checkpointPages.Load(); got != 1 {
			t.Fatalf("the checkpoint carried %d pages, want the one huge page", got)
		}
		if got := access(t, sibling, sm, 0, false)[huge-1]; got != 79 {
			t.Fatalf("the huge checkpoint lost the last byte: %d", got)
		}
	})
}

func TestHugePageSpillAndWritebackPreserveEverySubpage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize2MiB,
			ResidentPages: 1, LogicalPages: 3, DirtyPages: 3})
		r, m, b := f.memoryRegion(3)
		want := make([][]byte, 3)
		for page := range uint64(3) {
			data := access(t, r, m, page, true)
			// Every old 4 KiB page gets its own markers, including both halves
			// and the boundary between adjacent 2 MiB pages.
			for offset := 0; offset < len(data); offset += 4096 {
				data[offset] = byte(offset/4096 + int(page)*17)
				data[offset+1] = byte((offset/4096 + int(page)*512) >> 8)
				data[offset+4095] = byte(offset/4096 + int(page)*19 + 1)
			}
			want[page] = bytes.Clone(data)
		}
		for page := range uint64(3) {
			if !bytes.Equal(access(t, r, m, page, false), want[page]) {
				t.Fatalf("spill/refault lost subpage bytes in page %d", page)
			}
		}
		f.mustCheckpoint(r, b)
		for page := range uint64(3) {
			if !bytes.Equal(b.data[page*(2<<20):(page+1)*(2<<20)], want[page]) {
				t.Fatalf("the checkpoint lost subpage bytes in page %d", page)
			}
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.Spills == 0 {
			t.Fatal("test did not exercise spill")
		}
	})
}
