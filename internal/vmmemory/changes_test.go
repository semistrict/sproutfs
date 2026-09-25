package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// A measuring pager counts, at the settle, the 4 KiB blocks of each sealed page
// the guest actually changed since the page became private: one byte is one
// block, two bytes at either end of a page are two blocks of a 2 MiB page and
// one of a 4 KiB page, and a page taken writable and never stored into is none.
func TestAMeasuringPagerCountsTheBlocksTheGuestChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8,
			MeasureChanges: true})
		a, am, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
		}
		access(t, a, am, 0, true)[0] = 99
		ends := access(t, a, am, 1, true)
		ends[0], ends[len(ends)-1] = 98, 97
		access(t, a, am, 2, true)
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.settle(a)
		want := uint64(1 + 2)
		if pageSize == 4096 {
			want = 1 + 1
		}
		stats := hostStats(t, f)
		if stats.ChangedBlocks != want || stats.MeasuredPages != 3 || stats.UnmeasuredPages != 0 {
			t.Fatalf("the pager counted %d changed blocks over %d measured and %d unmeasured pages, want %d over 3 and 0",
				stats.ChangedBlocks, stats.MeasuredPages, stats.UnmeasuredPages, want)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}

// A pager that is not measuring counts nothing.
func TestAPagerThatIsNotMeasuringCountsNoBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, _ := f.memoryRegion(4)
		access(t, a, am, 0, false)
		access(t, a, am, 0, true)[0] = 99
		if err := a.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.settle(a)
		if stats := hostStats(t, f); stats.ChangedBlocks != 0 || stats.MeasuredPages != 0 || stats.UnmeasuredPages != 0 {
			t.Fatalf("a pager that is not measuring counted %+v", stats)
		}
		if err := a.Checkpoint().Retire(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	})
}
