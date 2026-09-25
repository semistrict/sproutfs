package vmmemory_test

import (
	"context"
	"slices"
	"sort"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

type delayedLocate struct {
	vmmemory.Backing
	entered chan struct{}
	release chan struct{}
}

type unavailableLocate struct {
	vmmemory.Backing
	ready bool
}

func (b *unavailableLocate) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if !b.ready {
		return nil, errInjected
	}
	return b.Backing.Locate(ctx, offset, length)
}

func TestEmptyResidencyAttachesWithoutReadingColdMetadata(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 4, 4)
		backing := &unavailableLocate{Backing: f.newBacking(4)}
		r, m := f.attach(backing)
		if len(m.pages) != 0 {
			t.Fatal("empty residency invented eager data mappings")
		}
		backing.ready = true
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatalf("cold first fault lost contents: %d", got)
		}
	})
}

// Two attachments inherit the same pages while both are held by in-flight
// faults. Population acquires resident locks in one global identity order and
// never waits for a lock from inside a plan that already holds another, so the
// release order of the faults cannot leave either attachment stuck.
func TestConcurrentPopulationsOfHeldPagesDoNotDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 6, DirtyPages: 2, ReadAheadPages: 1})
		source, sm, _ := f.memoryRegion(2)
		access(t, source, sm, 0, false)
		access(t, source, sm, 1, false)
		entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		released := [2]bool{}
		defer func() {
			for i := range release {
				if !released[i] {
					close(release[i])
				}
			}
		}()
		sm.onResolve = func(page uint64) { close(entered[page]); <-release[page] }
		faults := make(chan error, 2)
		for page := range uint64(2) {
			go func() { faults <- source.Fault(t.Context(), page, false) }()
		}
		<-entered[0]
		<-entered[1]
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attach := func(backing vmmemory.Backing) (*mapping, <-chan error) {
			m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
			f.a.mu.Lock()
			f.a.mappings = append(f.a.mappings, m)
			f.a.mu.Unlock()
			done := make(chan struct{})
			result := make(chan error, 1)
			var r *vmmemory.MemoryRegion
			go func() {
				var err error
				r, err = f.h.Attach(ctx, ram(backing), m)
				result <- err
				close(done)
			}()
			t.Cleanup(func() {
				<-done
				clear(m.pages)
				if r != nil {
					if err := r.Detach(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			return m, result
		}
		first, firstDone := attach(f.newBacking(2))
		synctest.Wait()
		second, secondDone := attach(f.newBacking(2))
		synctest.Wait()
		// Release the second page first. An attachment that waited on a held
		// page while holding another would never see either fault complete.
		close(release[1])
		released[1] = true
		synctest.Wait()
		close(release[0])
		released[0] = true
		synctest.Wait()
		for _, done := range []<-chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("concurrent populations of held pages deadlocked")
			}
		}
		for range 2 {
			if err := <-faults; err != nil {
				t.Fatal(err)
			}
		}
		for page := range uint64(2) {
			firstPage, firstOK := first.pages[page]
			secondPage, secondOK := second.pages[page]
			if !firstOK || !secondOK || firstPage.slot != sm.pages[page].slot || secondPage.slot != sm.pages[page].slot {
				t.Fatalf("page %d did not inherit the source page", page)
			}
		}
	})
}

// carved attaches a memory region of pages whose identities match the sibling's
// everywhere but the given pages, which are this backing's own unpublished
// state: a page the sibling cannot hold, so it breaks the sibling's residency
// into runs of exactly the lengths the test asks for.
func (f *fixture) carved(pages int, private ...uint64) (*vmmemory.MemoryRegion, *mapping, *backing) {
	f.t.Helper()
	b := f.newBacking(pages)
	for _, page := range private {
		b.private[page] = true
	}
	r, m := f.attach(b)
	return r, m, b
}

// mappedPages reports the pages this mapping holds, in order.
func (m *mapping) mappedPages() []uint64 {
	pages := make([]uint64, 0, len(m.pages))
	for page := range m.pages {
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	return pages
}

// pageRange is the pages [first, last), which is what a test says a populate
// installed.
func pageRange(first, last uint64) []uint64 {
	pages := make([]uint64, 0, last-first)
	for page := first; page < last; page++ {
		pages = append(pages, page)
	}
	return pages
}

// holed attaches a memory region of a volume whose given pages are explicit zeros,
// with a sibling that has faulted every page of it, so the sibling holds the
// data pages and the holes break its residency into runs. Both memory regions are
// forks of one checkpoint, so a hole is a hole in both.
func (f *fixture) holed(pages int, holes ...uint64) (*mapping, *mapping, *backing) {
	f.t.Helper()
	sibling := f.holedBacking(pages, holes)
	source, sm := f.attach(sibling)
	for page := range uint64(pages) {
		access(f.t, source, sm, page, false)
	}
	fork := f.holedBacking(pages, holes)
	_, m := f.attach(fork)
	return sm, m, fork
}

// holedBacking is one such volume: a fork of the fixture's checkpoint whose
// named pages are explicit zeros.
func (f *fixture) holedBacking(pages int, holes []uint64) *backing {
	f.t.Helper()
	b := f.newBacking(pages)
	for _, page := range holes {
		b.zero[page] = true
		clear(b.data[page*uint64(f.pageSize) : (page+1)*uint64(f.pageSize)])
	}
	return b
}

// A hole costs the same mapping command as a resident run and is worth it on the
// same terms. A guest's address space is holes all through it, not one: measured
// on GCE on 2026-09-22, a warm restore whose resident runs were already bounded
// still installed 14,447 runs over 2,166,194 pages before its guest ran, of
// which only 123,056 pages were resident identities — the rest were two million
// pages of scattered holes, one command each.
func TestPopulationSkipsHolesShorterThanTheWindowTheyWouldSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 40, LogicalPages: 128, DirtyPages: 8, ReadAheadPages: 4})
		// Six single-page holes, which leave the data in runs of four and a
		// tail of two.
		_, m, b := f.holed(32, 4, 9, 14, 19, 24, 29)
		if m.maps != 6 {
			t.Fatalf("the populate installed %d mapping runs, want the 6 data runs of a whole 4-page window", m.maps)
		}
		var want []uint64
		for _, first := range []uint64{0, 5, 10, 15, 20, 25} {
			want = append(want, pageRange(first, first+4)...)
		}
		if got := m.mappedPages(); !slices.Equal(got, want) {
			t.Fatalf("the populate mapped %v, want %v", got, want)
		}
		if b.loads != 0 {
			t.Fatalf("the populate read the backing %d times, want none", b.loads)
		}
	})
}

// Everything a populate installs comes out of the one budget, so what an attach
// costs before the guest runs is bounded whatever the volume looks like: the
// pages a fork point names have the first claim on it, because nothing but this
// populate can share them, and the rest is spent on the runs long enough to be
// worth it.
func TestPopulationSpendsOneBudgetOnHolesAndResidentRunsAlike(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 3)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 40, LogicalPages: 128, DirtyPages: 8, ReadAheadPages: 4})
		// Long holes and long data runs alternate, so every run is worth its
		// command and only the budget decides how many are installed.
		_, m, _ := f.holed(32, 4, 5, 6, 7, 16, 17, 18, 19)
		if m.maps != 3 {
			t.Fatalf("the populate installed %d mapping runs, want the 3 its budget admits", m.maps)
		}
		want := append(pageRange(0, 4), pageRange(4, 8)...)
		want = append(want, pageRange(8, 16)...)
		if got := m.mappedPages(); !slices.Equal(got, want) {
			t.Fatalf("the populate mapped %v, want %v", got, want)
		}
	})
}

// A populate run costs one mapping command whether or not the guest ever reads
// the pages it covers. A run shorter than one read-ahead window saves at most
// the one fault that would have mapped the same pages with the same single
// command, and only if the guest touches them, so it is never worth installing.
func TestPopulationSkipsRunsShorterThanTheWindowTheyWouldSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 40, LogicalPages: 96, DirtyPages: 8, ReadAheadPages: 4})
		source, sm, _ := f.memoryRegion(32)
		for page := range uint64(32) {
			access(t, source, sm, page, false)
		}
		// The sibling holds all thirty-two pages in slots of its own page
		// number. The carve leaves runs of 6, then five single pages, then 15.
		_, m, b := f.carved(32, 6, 8, 10, 12, 14, 16)
		if m.maps != 2 {
			t.Fatalf("the populate installed %d mapping runs, want the 2 runs of at least one 4-page window", m.maps)
		}
		want := append(pageRange(0, 6), pageRange(17, 32)...)
		if got := m.mappedPages(); !slices.Equal(got, want) {
			t.Fatalf("the populate mapped %v, want %v", got, want)
		}
		for _, page := range want {
			if m.pages[page].slot != int(page) {
				t.Fatalf("page %d mapped slot %d, want the sibling's slot %d", page, m.pages[page].slot, page)
			}
		}
		if b.loads != 0 {
			t.Fatalf("the populate read the backing %d times, want none", b.loads)
		}
	})
}

// What the populate leaves is left to the fault path, which maps a whole
// read-ahead window from the sibling's own pages with one command and no read.
// The bound is a fixed number of mapping runs before the guest runs, whatever
// the sibling holds.
func TestPopulationSpendsABoundedNumberOfRunsAndLeavesTheRestToFaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 2)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 40, LogicalPages: 96, DirtyPages: 8, ReadAheadPages: 4})
		source, sm, _ := f.memoryRegion(32)
		for page := range uint64(32) {
			access(t, source, sm, page, false)
		}
		// Three runs are worth installing — 8, 7 and 15 pages — and the budget
		// pays for two of them.
		r, m, b := f.carved(32, 8, 16)
		if m.maps != 2 {
			t.Fatalf("the populate installed %d mapping runs, want the 2 its budget admits", m.maps)
		}
		want := append(pageRange(0, 8), pageRange(9, 16)...)
		if got := m.mappedPages(); !slices.Equal(got, want) {
			t.Fatalf("the populate mapped %v, want %v", got, want)
		}
		// The guest's first touch of the stretch the budget did not reach costs
		// one command for the whole window, and no read: the sibling holds it.
		if err := r.Fault(t.Context(), 20, false); err != nil {
			t.Fatal(err)
		}
		if m.maps != 3 {
			t.Fatalf("the first touch beyond the populate took %d mapping runs in all, want 3", m.maps)
		}
		if got := m.mappedPages(); !slices.Equal(got, append(want, pageRange(20, 24)...)) {
			t.Fatalf("the fault mapped %v, want its whole 4-page window", got)
		}
		if b.loads != 0 {
			t.Fatalf("a warm fault read the backing %d times, want none", b.loads)
		}
	})
}

// A private page a fork point names is populated whatever its run is. The
// parent's dirty state is shared under a name that ending the seal takes back,
// so the attach is the only moment a child can map it; a fault arriving later
// would read the bytes back out of the child's own first checkpoint. Here the
// point's two pages are a run a quarter of the read-ahead window.
func TestPopulationTakesAForkPointsPagesWhateverTheirRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPagerCluster(t)
		source := c.create(t, "source", 8)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 48, DirtyPages: 8, ReadAheadPages: 8})
		r, m := f.attach(source.Volume("ram0"))
		for _, page := range []uint64{1, 2} {
			access(t, r, m, page, true)[0] = 44
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := source.ForkPoint(t.Context(),
			volume.Prepared([]byte("vmm"), map[string]volume.DirtySource{"ram0": r.Checkpoint()}))
		if err != nil {
			t.Fatal(err)
		}
		if err := point.Share(t.Context()); err != nil {
			t.Fatal(err)
		}
		vm, err := c.manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		before, _ := f.h.Stats(t.Context())
		child, cm := f.attach(vm.Volume("ram0"))
		for _, page := range []uint64{1, 2} {
			if cm.pages[page].slot != m.pages[page].slot {
				t.Fatalf("page %d mapped slot %d at attach, want the parent's own %d",
					page, cm.pages[page].slot, m.pages[page].slot)
			}
			if access(t, child, cm, page, false)[0] != 44 {
				t.Fatalf("the child lost the sealed bytes of page %d", page)
			}
		}
		after, _ := f.h.Stats(t.Context())
		if after.Loads != before.Loads {
			t.Fatalf("the child read %d pages back that the point held",
				after.LoadedPages-before.LoadedPages)
		}
		if after.IdentityHits-before.IdentityHits != 2 {
			t.Fatalf("the child mapped %d pages by identity, want the point's 2",
				after.IdentityHits-before.IdentityHits)
		}
	})
}

// The bound is what an attach costs before the guest runs, whatever the volume
// is made of. This one is made of 320 runs a page each — a hole between every
// two pages a sibling holds — and every one of them is a whole read-ahead
// window, so nothing but the budget decides.
func TestPopulationInstallsNoMoreRunsThanItsBudgetHoweverManyTheVolumeHas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 320
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
			ResidentPages: 400, LogicalPages: 1024, DirtyPages: 8, ReadAheadPages: 1})
		var holes []uint64
		for page := uint64(1); page < pages; page += 2 {
			holes = append(holes, page)
		}
		_, m, _ := f.holed(pages, holes...)
		if m.maps != 128 {
			t.Fatalf("the populate installed %d mapping runs, want the 128 its budget admits", m.maps)
		}
		if got := m.mappedPages(); !slices.Equal(got, pageRange(0, 128)) {
			t.Fatalf("the populate mapped %d pages ending at %d, want the first 128 of the memory region",
				len(got), got[len(got)-1])
		}
	})
}

// The pages a fork point names have the first claim on the budget, and they are
// in it rather than outside it: an attach's cost is bounded whatever a point
// names, and a page it could not afford is one the child reads back out of its
// own first checkpoint rather than one no bound applies to.
func TestAForkPointsPagesTakeTheBudgetFirstAndAreBoundedByIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 2)
		c := newPagerCluster(t)
		source := c.create(t, "source", 16)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 32, LogicalPages: 64, DirtyPages: 8, ReadAheadPages: 8})
		r, m := f.attach(source.Volume("ram0"))
		// Three pages the guest stored into and no checkpoint has, each a run of
		// one, and a ten-page hole after them that is worth a command of its own.
		for _, page := range []uint64{1, 3, 5} {
			access(t, r, m, page, true)[0] = 44
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := source.ForkPoint(t.Context(),
			volume.Prepared([]byte("vmm"), map[string]volume.DirtySource{"ram0": r.Checkpoint()}))
		if err != nil {
			t.Fatal(err)
		}
		if pages := point.Pages("ram0"); !slices.Equal(pages, []uint64{1, 3, 5}) {
			t.Fatalf("the fork point holds %v unpublished, want pages 1, 3 and 5", pages)
		}
		if err := point.Share(t.Context()); err != nil {
			t.Fatal(err)
		}
		vm, err := c.manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		_, cm := f.attach(vm.Volume("ram0"))
		if cm.maps != 2 {
			t.Fatalf("the populate installed %d mapping runs, want the 2 its budget admits", cm.maps)
		}
		if got := cm.mappedPages(); !slices.Equal(got, []uint64{1, 3}) {
			t.Fatalf("the populate mapped %v, want the first two pages the point names", got)
		}
	})
}

// A populate whose run budget is spent stops walking. The budget bounds the
// commands it installs, but the walk that finds them goes window by window over
// the whole memory region, and every window asks the volume for the identity of every
// page in it: for a 16 GiB guest at a 4 KiB page that is four million page
// identities, decoded out of the index's segments, before the guest runs.
//
// So the walk ends where the budget does. Nothing after it can install a run of
// any kind — a hole, a resident identity or a page a fork point named — so every
// window it would still ask about is metadata read for nothing, and that is the
// part of a restore's wait this pager owns.
func TestPopulationStopsWalkingWhenItsRunBudgetIsSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationRuns(t, 2)
		// Four windows of eight pages, with a hole between every two pages the
		// sibling holds, so every window has runs worth a command and the first
		// one spends the whole budget.
		const window, windows = 8, 4
		const pages = window * windows
		vmmemory.SetPopulationWindowBytes(t, window*checkpoint.PageSize4KiB)
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
			ResidentPages: 2 * pages, LogicalPages: 4 * pages, DirtyPages: 8, ReadAheadPages: 1})
		var holes []uint64
		for page := uint64(1); page < pages; page += 2 {
			holes = append(holes, page)
		}
		sibling := f.holedBacking(pages, holes)
		source, sm := f.attach(sibling)
		for page := range uint64(pages) {
			access(t, source, sm, page, false)
		}
		fork := f.holedBacking(pages, holes)
		var located []uint64
		fork.onLocate = func(offset, _ uint64) {
			located = append(located, offset/uint64(f.pageSize))
		}
		_, m := f.attach(fork)
		if m.maps != 2 {
			t.Fatalf("the populate installed %d mapping runs, want the 2 its budget admits", m.maps)
		}
		// One window's metadata and no more: the budget was spent inside it, so
		// the three windows after it hold nothing this populate could install.
		if !slices.Equal(located, []uint64{0}) {
			t.Fatalf("the populate asked the volume about the pages at %v, want the first window's alone", located)
		}
	})
}

func (b *delayedLocate) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-b.release:
	}
	return b.Backing.Locate(ctx, offset, length)
}

func TestStalledMetadataDoesNotDelayUnrelatedWarmAttachment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 12, 4)
		source, sm, _ := f.memoryRegion(4)
		for page := range uint64(4) {
			access(t, source, sm, page, false)
		}
		slow := &delayedLocate{Backing: f.newUnrelatedBacking(4), entered: make(chan struct{}), release: make(chan struct{})}
		defer close(slow.release)
		attach := func(backing vmmemory.Backing) (*mapping, <-chan struct{}, *error) {
			m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
			f.a.mu.Lock()
			f.a.mappings = append(f.a.mappings, m)
			f.a.mu.Unlock()
			done := make(chan struct{})
			var r *vmmemory.MemoryRegion
			var err error
			go func() { r, err = f.h.Attach(t.Context(), ram(backing), m); close(done) }()
			t.Cleanup(func() {
				<-done
				f.a.mu.Lock()
				clear(m.pages)
				f.a.mu.Unlock()
				if r != nil {
					if err := r.Detach(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			return m, done, &err
		}
		_, _, _ = attach(slow)
		<-slow.entered
		warm, done, err := attach(f.newBacking(4))
		synctest.Wait()
		select {
		case <-done:
			if *err != nil {
				t.Fatal(*err)
			}
		default:
			t.Fatal("warm attachment waited for unrelated metadata")
		}
		for page := range uint64(4) {
			got, ok := warm.pages[page]
			if !ok || got.slot != sm.pages[page].slot {
				t.Fatalf("resident page %d missing at attachment completion", page)
			}
		}
	})
}

func TestAttachmentIncludesPagesLoadedDuringMetadataLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 6, 9, 4)
		source, sm, _ := f.memoryRegion(4)
		for page := range uint64(3) {
			access(t, source, sm, page, false)
		}
		seed, seedMapping := f.attach(f.newUnrelatedBacking(1))
		access(t, seed, seedMapping, 0, false)
		slow := &delayedLocate{Backing: f.newBacking(4), entered: make(chan struct{}), release: make(chan struct{})}
		m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		f.a.mappings = append(f.a.mappings, m)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var r *vmmemory.MemoryRegion
		var err error
		go func() { r, err = f.h.Attach(ctx, ram(slow), m); close(done) }()
		t.Cleanup(func() {
			<-done
			clear(m.pages)
			if r != nil {
				if err := r.Detach(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
		<-slow.entered
		access(t, source, sm, 3, false)
		close(slow.release)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			got, ok := m.pages[page]
			if !ok || got.slot != sm.pages[page].slot {
				t.Fatalf("page %d was resident before metadata returned but was not populated", page)
			}
		}
	})
}

// A run's cost is not only its command. The kernel installs the run's pages one
// by one — a write-protected entry each — and on 2026-09-23 on GCE a warm
// restore's populate of 839,196 pages in 128 runs took 1.10 s, 1.3 µs a page,
// against a half-second bound for the whole restore; a fork's took 2.0–2.3 s
// over 2.03 M pages. So the populate is bounded in pages as well as in runs,
// and a run it cannot afford in pages is left to the faults' windows like any
// other.
func TestPopulationInstallsNoMorePagesThanItsBudgetHoweverLongTheRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vmmemory.SetPopulationPages(t, 16)
		const pages = 64
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: checkpoint.PageSize4KiB,
			ResidentPages: 128, LogicalPages: 256, DirtyPages: 8, ReadAheadPages: 8})
		// The sibling read the whole memory region window by window into consecutive
		// slots, so it holds the memory region as one run: the populate installs the
		// front of it, cut to the pages it can afford, in one command.
		_, m, _ := f.holed(pages)
		if got := m.mappedPages(); !slices.Equal(got, pageRange(0, 16)) {
			t.Fatalf("the populate mapped %d pages, want the first 16 its page budget admits", len(got))
		}
		if m.maps != 1 {
			t.Fatalf("the populate installed %d mapping runs, want the one run's front", m.maps)
		}
	})
}
