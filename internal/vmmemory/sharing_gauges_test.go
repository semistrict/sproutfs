package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// pageBytes is a page count in the unit the gauges report, which is what every
// expectation below is written in.
func pageBytes(pages int) uint64 { return uint64(pages * pageSize) }

func sharing(t *testing.T, f *fixture) vmmemory.SharingStats {
	t.Helper()
	stats, err := f.h.Sharing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// wantSharing checks one kind's whole gauge at once, in pages, because the three
// numbers only mean anything together: mapped less unique is the saving.
func wantSharing(t *testing.T, got vmmemory.Sharing, unique, mapped int, what string) {
	t.Helper()
	want := vmmemory.Sharing{UniqueBytes: pageBytes(unique), MappedBytes: pageBytes(mapped),
		SavedBytes: pageBytes(mapped - unique)}
	if got != want {
		t.Errorf("%s: %+v, want %+v", what, got, want)
	}
}

func wantMemoryRegion(t *testing.T, r *vmmemory.MemoryRegion, resident, private, shared int, what string) {
	t.Helper()
	stats, err := r.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ResidentBytes() != pageBytes(resident) || stats.PrivateBytes() != pageBytes(private) ||
		stats.SharedBytes() != pageBytes(shared) {
		t.Errorf("%s: resident=%d private=%d shared=%d bytes, want %d, %d and %d",
			what, stats.ResidentBytes(), stats.PrivateBytes(), stats.SharedBytes(),
			pageBytes(resident), pageBytes(private), pageBytes(shared))
	}
}

// Two memory regions that inherited the same checkpoint map one resident page each,
// which is the whole of what the pager is for: the arena holds the pages once
// and the guests map them twice, and the difference is the memory the host did
// not have to find.
func TestSharingGaugesCountEveryAliasOfAResidentPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "two memory regions of four shared pages")
		wantMemoryRegion(t, a, 4, 0, 4, "the first memory region")
		wantMemoryRegion(t, b, 4, 0, 4, "the second memory region")
	})
}

// A store is where a shared page stops being shared: the writer takes a private
// page of its own, the arena holds one page more, and neither memory region counts that
// page as shared any longer.
func TestAStoreTakesOnePageOutOfTheSharedSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
			access(t, b, bm, page, false)
		}
		access(t, a, am, 0, true)[0] = 99
		wantSharing(t, sharing(t, f).Ram, 5, 8, "one of four pages stored into")
		wantMemoryRegion(t, a, 4, 1, 3, "the memory region that stored")
		wantMemoryRegion(t, b, 4, 0, 3, "the memory region that did not")
	})
}

// The gauges are split by what the memory region is to its guest, because RAM and PMEM
// are separate arenas to plan for: a host cannot read one number and know which
// of the two is sharing anything.
func TestSharingGaugesReportRAMAndPMEMSeparately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		ram, ramMapping := f.attach(f.newBacking(2))
		pmem, pmemMapping := f.attachKind(vmmemory.Pmem, f.newUnrelatedBacking(3))
		for page := uint64(0); page < 2; page++ {
			access(t, ram, ramMapping, page, false)
		}
		for page := uint64(0); page < 3; page++ {
			access(t, pmem, pmemMapping, page, false)
		}
		stats := sharing(t, f)
		wantSharing(t, stats.Ram, 2, 2, "the RAM memory region alone")
		wantSharing(t, stats.Pmem, 3, 3, "the PMEM memory region alone")
	})
}

// Eviction is the other direction: the page leaves the arena and every memory region
// that mapped it loses it, so unique, mapped and saved all fall together rather
// than leaving a saving behind that no memory backs.
func TestEvictingASharedPageLowersEveryGauge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 32, 4)
		a, am, _ := f.memoryRegion(4)
		b, bm, _ := f.memoryRegion(4)
		for page := uint64(0); page < 4; page++ {
			access(t, a, am, page, false)
		}
		for page := uint64(0); page < 4; page++ {
			access(t, b, bm, page, false)
		}
		wantSharing(t, sharing(t, f).Ram, 4, 8, "four pages shared by two memory regions")
		// The arena is full, so the next memory region's fault takes the page both of
		// them touched first.
		c, cm := f.attach(f.newUnrelatedBacking(1))
		access(t, c, cm, 0, false)
		wantSharing(t, sharing(t, f).Ram, 4, 7, "after the shared page was evicted")
		wantMemoryRegion(t, a, 3, 0, 3, "the first memory region")
		wantMemoryRegion(t, b, 3, 0, 3, "the second memory region")
		wantMemoryRegion(t, c, 1, 0, 0, "the memory region whose fault evicted it")
	})
}
