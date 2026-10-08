package vmmemory_test

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A page the guest keeps reading through its mapping is kept over the pages it
// writes once meanwhile. Its reads never fault, which is what a DAX root's are:
// the guest reads the page through the mapping the first fault installed. An
// eviction harvests a page before it takes it, by taking the page's mappings
// away and keeping its bytes, so the guest's next read faults it back from its
// frame, and the fault marks it accessed. Once evictions are under way, the
// read page is never loaded again.
func TestAnEvictionKeepsAPageTheGuestReadsThroughItsMapping(t *testing.T) {
	for _, kind := range []vmmemory.MemoryRegionKind{vmmemory.Ram, vmmemory.Pmem} {
		t.Run(kind.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const resident, hot = 16, 0
				const pages = 8 + 10*resident
				f := newFixture(t, resident, pages, pages)
				b := f.newBacking(pages)
				// Every page but the read one holds zeros, which a store makes
				// private without a load, so only the read page is loaded.
				for page := uint64(1); page < pages; page++ {
					b.zero[page] = true
				}
				loads := 0
				b.onLoad = func(offset uint64, _ int) {
					if offset == hot {
						loads++
					}
				}
				r, m := f.attachKind(kind, b)
				accessUnder(f.ctx, t, r, m, hot, false)
				page := uint64(8)
				write := func(writes int) {
					for range writes {
						accessUnder(f.ctx, t, r, m, page, true)[0] = byte(page)
						page++
						accessUnder(f.ctx, t, r, m, hot, false)
					}
				}
				write(2 * resident)
				if s := hostStats(t, f); s.Evictions == 0 {
					t.Fatalf("evicted nothing over %d writes into %d pages", 2*resident, resident)
				}
				before := loads
				write(8 * resident)
				if loads != before {
					t.Fatalf("loaded the page the guest reads %d more times over %d writes, want none",
						loads-before, 8*resident)
				}
				if s := hostStats(t, f); s.HarvestedPages == 0 || s.SecondChances == 0 {
					t.Fatalf("harvested %d pages and gave %d second chances, want both",
						s.HarvestedPages, s.SecondChances)
				}
			})
		})
	}
}

// A store into a page a journal capture write-protected and a harvest then
// unmapped maps the page writable again, and records it mapped, so an
// eviction of it takes that mapping away before its slot goes back. The
// protect trap used to map the page without recording it: the page it served
// had always been mapped read-only, never not at all.
func TestAStoreIntoAHarvestedJournaledPageIsRevokedByItsEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 8, 8)
		r, m, _ := f.pmemRegion(2)
		f.storeAt(r, m, 0, 0, 0xee)
		if _, err := r.Capture(f.ctx, r.Unjournaled()); err != nil {
			t.Fatal(err)
		}
		if harvested, err := vmmemory.HarvestPage(f.ctx, r, 0); err != nil || !harvested {
			t.Fatalf("harvesting page 0 = %t, %v; want it harvested", harvested, err)
		}
		if p, mapped := m.mappedPage(0); mapped {
			t.Fatalf("the harvest left page 0 mapped as %+v", p)
		}
		f.storeAt(r, m, 0, 1, 0xef)
		if got := r.Unjournaled(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("unjournaled pages %v after a store into the harvested page, want [0]", got)
		}
		if went, err := vmmemory.EvictPage(f.ctx, r, 0); err != nil || !went {
			t.Fatalf("evicting page 0 = %t, %v; want it gone", went, err)
		}
		if p, mapped := m.mappedPage(0); mapped {
			t.Fatalf("the eviction left page 0 mapped as %+v", p)
		}
		if got := access(t, r, m, 0, false)[:2]; got[0] != 0xee || got[1] != 0xef {
			t.Fatalf("page 0 reads %#x after its eviction, want the 0xee and 0xef the guest stored", got)
		}
	})
}
