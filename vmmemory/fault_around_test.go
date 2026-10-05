package vmmemory_test

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A fault maps the pages around its own that are resident already, as
// Zircon's fault maps the pages around the faulting one that are present in
// its VMO (VmMapping::PageFaultLockedObject, vm/vm_mapping.cc:1334-1360). Here
// the bound is the read-ahead window, where Zircon's is its optimistic cap,
// and only a fault that reads its window maps it: a fault at random maps its
// page alone (faultfirst.go). Both cores do this.

// aroundPages is the size of every region these tests attach: four windows.
const aroundPages = 32

// aroundConfig is a pager of eight-page windows with room for every page the
// tests read. The tests turn population off, so that what a region maps is
// what its faults mapped.
func aroundConfig() vmmemory.Config {
	return vmmemory.Config{ResidentPages: 256, LogicalPages: 1024, DirtyPages: 16, ReadAheadPages: 8, PrefetchRuns: 16}
}

// mappedPages is every page a mapping maps, in order.
func mappedPages(m *mapping) []uint64 {
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	var pages []uint64
	for page := range m.pages {
		pages = append(pages, page)
	}
	slices.Sort(pages)
	return pages
}

// pagesFrom is the count pages from first, in order.
func pagesFrom(first, count uint64) []uint64 {
	var pages []uint64
	for page := first; page < first+count; page++ {
		pages = append(pages, page)
	}
	return pages
}

// aroundRegion attaches a region of aroundPages pages whose pages are the
// checkpoint source's, over a backing whose reads take time and whose
// prefetches wait for held where it is not nil.
func aroundRegion(t *testing.T, f *fixture, source control.Ref, held chan struct{}) (*vmmemory.MemoryRegion, *mapping, *slowBacking) {
	t.Helper()
	b := &slowBacking{backing: f.newBacking(aroundPages), held: held}
	b.source = source
	r, m := f.attach(b)
	return r, m, b
}

// fault faults one page and waits for the prefetch it started.
func fault(t *testing.T, f *fixture, r *vmmemory.MemoryRegion, page uint64) {
	t.Helper()
	if err := r.Fault(f.ctx, page, false); err != nil {
		t.Fatal(err)
	}
	if err := r.SettlePrefetches(f.ctx); err != nil {
		t.Fatal(err)
	}
}

// residentAlone makes each page resident alone, with nothing else of its
// window: a region of the checkpoint's first faults the last window, so that
// its fault on the page is at random and reads that page alone. The last
// window ends up resident too.
func residentAlone(t *testing.T, f *fixture, source control.Ref, pages ...uint64) {
	t.Helper()
	for _, page := range pages {
		r, _, _ := aroundRegion(t, f, source, nil)
		fault(t, f, r, aroundPages-8)
		fault(t, f, r, page)
	}
}

// A fault over a window a sibling has read maps the whole window and nothing
// of the window after it, resident too; a fault at random over a page nothing
// holds maps that page alone, as does one over its one resident page; and a
// fault whose window holds four resident pages maps those four with its own
// before the prefetch of the rest lands.
//
// Zircon's vm_mapping_page_fault_optimisation_test
// (vm/unittests/aspace_unittest.cc:1566): a fault maps every committed page
// up to its cap and no further, only its own page where nothing else is
// committed, and only the committed pages where some are.
func TestAFaultMapsTheResidentPagesOfItsWindowAndNoOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 0)
		f := newConfiguredFixture(t, aroundConfig())
		checkpoint := func(name string) control.Ref { return control.Ref{VM: vmName(t) + "-" + name, Sequence: 1} }

		// The first two windows resident: a region's first fault reads its
		// window, and its fault in the window after follows it.
		whole := checkpoint("whole")
		sibling, _, _ := aroundRegion(t, f, whole, nil)
		fault(t, f, sibling, 0)
		fault(t, f, sibling, 8)
		before := hostStats(t, f)
		r, m, _ := aroundRegion(t, f, whole, nil)
		fault(t, f, r, 0)
		if got := mappedPages(m); !slices.Equal(got, pagesFrom(0, 8)) {
			t.Fatalf("a fault over a resident window maps %v, want its window, pages 0 to 7", got)
		}
		if s := hostStats(t, f); s.Loads != before.Loads || s.ResidentPages != before.ResidentPages {
			t.Fatalf("mapping a resident window read %d times and took %d pages, want neither",
				s.Loads-before.Loads, s.ResidentPages-before.ResidentPages)
		}

		// Nothing resident: a fault at random maps its page alone.
		r, m, _ = aroundRegion(t, f, checkpoint("cold"), nil)
		fault(t, f, r, aroundPages-8)
		fault(t, f, r, 0)
		if got := mappedPages(m); !slices.Equal(got, append([]uint64{0}, pagesFrom(aroundPages-8, 8)...)) {
			t.Fatalf("a fault at random over cold pages maps %v, want page 0 alone beside the last window", got)
		}

		// Its page alone resident: a fault at random maps that one page.
		one := checkpoint("one")
		residentAlone(t, f, one, 0)
		r, m, _ = aroundRegion(t, f, one, nil)
		fault(t, f, r, aroundPages-8)
		fault(t, f, r, 0)
		if got := mappedPages(m); !slices.Equal(got, append([]uint64{0}, pagesFrom(aroundPages-8, 8)...)) {
			t.Fatalf("a fault at random over its one resident page maps %v, want page 0 alone beside the last window", got)
		}

		// Four resident pages of eight: the region's first fault maps those
		// four with its own page, and its prefetch of the rest, held, maps
		// nothing until it lands.
		four := checkpoint("four")
		residentAlone(t, f, four, 0, 1, 2, 3)
		held := make(chan struct{})
		r, m, _ = aroundRegion(t, f, four, held)
		if err := r.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := mappedPages(m); !slices.Equal(got, pagesFrom(0, 4)) {
			t.Fatalf("a fault over four resident pages of eight maps %v before its prefetch lands, want pages 0 to 3", got)
		}
		close(held)
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := mappedPages(m); !slices.Equal(got, pagesFrom(0, 8)) {
			t.Fatalf("once its prefetch landed the region maps %v, want its window, pages 0 to 7", got)
		}
	})
}

// A fault maps the resident pages of its window without reading them or
// taking a page for them, and never maps past its window, whatever is
// resident there.
//
// Zircon's vm_mapping_page_fault_range_test (vm/unittests/aspace_unittest.cc:
// 1715): read faulting maps the committed pages without allocating, and a
// fault maps nothing past the range it may fault in.
func TestAFaultReadsNothingForTheResidentPagesItMapsAndStopsAtItsWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 0)
		f := newConfiguredFixture(t, aroundConfig())
		source := control.Ref{VM: vmName(t) + "-source", Sequence: 1}
		residentAlone(t, f, source, 3, 5)
		next, _, _ := aroundRegion(t, f, source, nil)
		fault(t, f, next, 8)
		before := hostStats(t, f)
		held := make(chan struct{})
		r, m, b := aroundRegion(t, f, source, held)
		if err := r.Fault(f.ctx, 4, false); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := mappedPages(m); !slices.Equal(got, []uint64{3, 4, 5}) {
			t.Fatalf("a fault on page 4 maps %v before its prefetch lands, want its own and the resident 3 and 5", got)
		}
		if reads := b.readsOf(false); len(reads) != 1 || reads[0] != (slowRead{first: 4, pages: 1}) {
			t.Fatalf("the fault read %v, want page 4 alone", reads)
		}
		close(held)
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := mappedPages(m); !slices.Equal(got, pagesFrom(0, 8)) {
			t.Fatalf("once its prefetch landed the region maps %v, want its window alone, pages 0 to 7", got)
		}
		if reads := b.readsOf(true); len(reads) != 1 || reads[0] != (slowRead{first: 0, pages: 5, prefetch: true}) {
			t.Fatalf("the prefetch read %v, want pages 0, 1, 2, 6 and 7 in one read", reads)
		}
		if s := hostStats(t, f); s.ResidentPages != before.ResidentPages+6 {
			t.Fatalf("the window took %d pages, want six: the resident 3 and 5 cost none",
				s.ResidentPages-before.ResidentPages)
		}
	})
}

// No object lock is held across a mapping command: a fault whose command the
// client has not answered holds nothing another fault of the same region, in
// another window, over pages of the same checkpoint, needs.
func TestAFaultIsServedWhileAnotherFaultsCommandIsUnanswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, aroundConfig())
		region, m, _ := aroundRegion(t, f, f.source, nil)
		answer := make(chan struct{})
		holding := make(chan struct{})
		var once sync.Once
		m.onMap = func(page uint64, _ int) {
			if page == 0 {
				once.Do(func() {
					close(holding)
					<-answer
				})
			}
		}
		first := make(chan error, 1)
		go func() { first <- region.Fault(f.ctx, 0, false) }()
		<-holding
		// The first fault's command is in flight. A fault in another window
		// is served meanwhile, from the same checkpoint.
		if err := region.Fault(f.ctx, 16, false); err != nil {
			t.Fatal(err)
		}
		requirePage(t, m, 16)
		select {
		case err := <-first:
			t.Fatalf("the first fault ended before its command was answered: %v", err)
		default:
		}
		close(answer)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		requirePage(t, m, 0)
		if err := region.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
	})
}
