package vmmemory_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A fault reads its own page, and the rest of its read-ahead run is read
// behind it (prefetch.go). These run over a backing whose reads take virtual
// time, a fixed cost per read and a cost per page it carries, as a read of
// the cluster or the store does, so what each fault waited for is exact.

const (
	// readCost is what every read of slowBacking costs, and pageCost what
	// each page it carries adds: a page alone is 2 ms, and the seven other
	// pages of an eight-page run are 8 ms.
	readCost = time.Millisecond
	pageCost = time.Millisecond
	// pageRead is what a read of one page costs.
	pageRead = readCost + pageCost
)

// slowBacking is a fixture backing whose reads take readCost and pageCost a
// page they carry. It reads a run as one read whatever pages it leaves out,
// as a volume does (vmmemory.SparseLoader). While held is open it holds every
// prefetch's read until held closes or the read is cancelled; admit, where
// set, has a controlled run release each read once it is done.
type slowBacking struct {
	*backing
	held  chan struct{}
	admit bool
	mu    sync.Mutex
	// reads is every read, each its first page, its pages and whether a
	// prefetch made it.
	reads []slowRead
}

type slowRead struct {
	first, pages uint64
	prefetch     bool
}

var _ vmmemory.SparseLoader = (*slowBacking)(nil)

func (f *fixture) slowBacking(pages int) *slowBacking {
	return &slowBacking{backing: f.newBacking(pages)}
}

func (b *slowBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	return b.LoadPages(ctx, offset, dst, nil)
}

func (b *slowBacking) LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error {
	size := uint64(b.pageSize)
	pages := uint64(len(dst)) / size
	carried := uint64(0)
	for index := range pages {
		if wanted == nil || wanted[index] {
			carried++
		}
	}
	prefetch := checkpoint.Prefetching(ctx)
	b.mu.Lock()
	b.reads = append(b.reads, slowRead{first: offset / size, pages: carried, prefetch: prefetch})
	held := b.held
	b.mu.Unlock()
	if prefetch && held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	timer := time.NewTimer(readCost + time.Duration(carried)*pageCost)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	if b.admit {
		if err := sim.Admit(ctx, "slow-backing/read"); err != nil {
			return err
		}
	}
	b.backing.mu.Lock()
	defer b.backing.mu.Unlock()
	for index := range pages {
		if wanted == nil || wanted[index] {
			copy(dst[index*size:(index+1)*size], b.backing.data[offset+index*size:offset+(index+1)*size])
		}
	}
	return nil
}

// readsOf is every read the backing was asked for, the prefetches' or the
// faults' as prefetch says, in the order of their first pages.
func (b *slowBacking) readsOf(prefetch bool) []slowRead {
	b.mu.Lock()
	defer b.mu.Unlock()
	var reads []slowRead
	for _, read := range b.reads {
		if read.prefetch == prefetch {
			reads = append(reads, read)
		}
	}
	slices.SortFunc(reads, func(a, b slowRead) int { return int(a.first) - int(b.first) })
	return reads
}

// prefetchConfig is a pager with an eight-page read-ahead run and room for
// everything the tests read and every copy their stores make.
func prefetchConfig() vmmemory.Config {
	return vmmemory.Config{ResidentPages: 128, LogicalPages: 128, DirtyPages: 16, ReadAheadPages: 8, PrefetchRuns: 16}
}

// requirePage requires a page to be mapped and to hold the fixture's bytes for
// it, every byte of page i holding i+1.
func requirePage(t *testing.T, m *mapping, page uint64) {
	t.Helper()
	p, ok := m.mappedPage(page)
	if !ok {
		t.Fatalf("page %d is not mapped", page)
	}
	if got := m.arena.pageUnder(p.place)[0]; got != byte(page+1) {
		t.Fatalf("page %d holds %d, want %d", page, got, page+1)
	}
}

// A guest that follows pointers knows its next address only once the page it
// is reading is in. So a chain of faults pays each fault's whole wait, one
// after another. Each hop here is in a window of its own, none in the window
// after an earlier hop's, and each costs exactly one page's read. The first
// hop of a memory region prefetches the rest of its run behind it, as a boot
// does; the rest follow none of the faults before them and read their page
// alone, so no prefetch takes the processors their reads need. Read the run
// first, as the pager did, and each hop costs the whole run, 9 ms. A store
// into a page the memory region holds nothing for, which is how every cold
// fault reaches the pager on x86-64, is the same.
func TestADependentChainOfFaultsWaitsForOnePageAHop(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newConfiguredFixture(t, prefetchConfig())
				b := f.slowBacking(128)
				r, m := f.attach(b)
				// Windows 6, 0, 10, 2, 14, 4, 12 and 8.
				hops := []uint64{51, 3, 83, 19, 115, 35, 99, 67}
				began := time.Now()
				for _, page := range hops {
					started := time.Now()
					if err := r.Fault(f.ctx, page, write); err != nil {
						t.Fatal(err)
					}
					if took := time.Since(started); took != pageRead {
						t.Fatalf("the hop to page %d took %v, want %v: one page's read", page, took, pageRead)
					}
					requirePage(t, m, page)
				}
				if took := time.Since(began); took != time.Duration(len(hops))*pageRead {
					t.Fatalf("the chain of %d hops took %v, want %v", len(hops), took, time.Duration(len(hops))*pageRead)
				}
				if err := r.SettlePrefetches(f.ctx); err != nil {
					t.Fatal(err)
				}
				s := hostStats(t, f)
				hopCount := uint64(len(hops))
				if s.Faults != hopCount || s.Loads != hopCount+1 || s.LoadedPages != hopCount+7 ||
					s.Prefetches != 1 || s.PrefetchedPages != 7 || s.PrefetchMapped != 7 ||
					s.PrefetchRandom != hopCount-1 || s.PrefetchWaits != 0 {
					t.Fatalf("faults %d, loads %d of %d pages, prefetches %d of %d pages mapping %d, runs left unread %d, "+
						"waits %d; want %d, %d of %d, 1 of 7 mapping 7, %d, 0",
						s.Faults, s.Loads, s.LoadedPages, s.Prefetches, s.PrefetchedPages, s.PrefetchMapped,
						s.PrefetchRandom, s.PrefetchWaits, hopCount, hopCount+1, hopCount+7, hopCount-1)
				}
				for _, read := range b.readsOf(false) {
					if read.pages != 1 {
						t.Fatalf("a fault read %d pages from page %d, want its own alone", read.pages, read.first)
					}
				}
				// The first hop's run is in, mapped by its prefetch.
				for page := uint64(48); page < 56; page++ {
					requirePage(t, m, page)
				}
			})
		})
	}
}

// A guest reading its memory forwards still reads it in runs. Its first fault
// in a run reads that page; its second finds the rest of the run in flight and
// waits for that read rather than reading its page again, and once the run is
// in maps all of it, so a run costs two faults and two reads, and no page is
// read twice. The page's read overlaps the run's, so a run costs the run's
// read, 8 ms, where reading the run first cost 9.
func TestAGuestReadingForwardsStillReadsItsMemoryInRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := f.slowBacking(24)
		r, m := f.attach(b)
		began := time.Now()
		for page := range uint64(24) {
			if _, ok := m.mappedPage(page); !ok {
				if err := r.Fault(f.ctx, page, false); err != nil {
					t.Fatal(err)
				}
			}
			requirePage(t, m, page)
		}
		if took, want := time.Since(began), 3*(readCost+7*pageCost); took != want {
			t.Fatalf("reading three runs forwards took %v, want %v: each run's own read", took, want)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		s := hostStats(t, f)
		if s.Faults != 6 || s.Loads != 6 || s.LoadedPages != 24 || s.Prefetches != 3 || s.PrefetchWaits != 3 {
			t.Fatalf("faults %d, loads %d of %d pages, prefetches %d, waits %d; want 6, 6 of 24, 3, 3",
				s.Faults, s.Loads, s.LoadedPages, s.Prefetches, s.PrefetchWaits)
		}
		if reads := b.readsOf(true); len(reads) != 3 || reads[0] != (slowRead{1, 7, true}) ||
			reads[1] != (slowRead{9, 7, true}) || reads[2] != (slowRead{17, 7, true}) {
			t.Fatalf("the prefetches read %v, want the seven pages after each run's first", reads)
		}
	})
}

// Nothing waits on a prefetch but a fault on a page that prefetch is reading.
// With every prefetch held, faults on other runs, reads and stores, each cost
// exactly their own page's read.
func TestAFaultNeverWaitsForAPrefetchOfOtherPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := f.slowBacking(32)
		b.held = make(chan struct{})
		time.AfterFunc(time.Hour, func() { close(b.held) })
		r, m := f.attach(b)
		for _, access := range []struct {
			page  uint64
			write bool
		}{{0, false}, {8, false}, {16, true}, {27, false}} {
			started := time.Now()
			if err := r.Fault(f.ctx, access.page, access.write); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(started); took != pageRead {
				t.Fatalf("a fault on page %d beside held prefetches took %v, want %v", access.page, took, pageRead)
			}
			requirePage(t, m, access.page)
		}
		// The host's settle waits for every prefetch of every memory region,
		// each held until the hour is up.
		began := time.Now()
		if err := f.h.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(began); waited != time.Hour-4*pageRead+readCost+7*pageCost {
			t.Fatalf("the host settled its prefetches in %v, want the hold and then a run's read", waited)
		}
		for page := range uint64(32) {
			requirePage(t, m, page)
		}
	})
}

// A prefetch takes only slots that are free and never evicts. With the arena
// full of pages the guest maps, a fault in another memory region evicts for
// its own page and reads nothing else.
func TestAPrefetchTakesOnlyFreeSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 4})
		a, am := f.attach(f.slowBacking(8))
		for _, page := range []uint64{0, 4} {
			accessUnder(f.ctx, t, a, am, page, false)
		}
		other := f.slowBacking(8)
		other.source = control.Ref{VM: other.owner + "-unrelated", Sequence: 1}
		o, om := f.attach(other)
		started := time.Now()
		if err := o.Fault(f.ctx, 1, false); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(started); took != pageRead {
			t.Fatalf("the fault took %v, want %v", took, pageRead)
		}
		if err := o.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		s := hostStats(t, f)
		if s.Evictions != 1 || s.Prefetches != 2 || len(other.readsOf(true)) != 0 {
			t.Fatalf("evictions %d, prefetches %d, the other image's prefetch reads %v; want 1, the first image's 2, none",
				s.Evictions, s.Prefetches, other.readsOf(true))
		}
		for page := range uint64(4) {
			if _, mapped := om.mappedPage(page); mapped != (page == 1) {
				t.Fatalf("page %d of the other image is mapped = %t, want only the faulting page", page, mapped)
			}
		}
	})
}

// Memory pressure drops a prefetch. With a prefetch's read held and every
// other slot a page the guest maps, a fault that needs a slot cancels the
// prefetch and takes one of its slots instead of evicting a page the guest
// is using. It waits for nothing but its own page's read.
func TestAnAllocationCancelsAPrefetchRatherThanEvict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 8, LogicalPages: 32, DirtyPages: 8,
			ReadAheadPages: 4, PrefetchRuns: 1})
		b := f.slowBacking(24)
		b.held = make(chan struct{})
		time.AfterFunc(time.Hour, func() { close(b.held) })
		r, m := f.attach(b)
		// The first fault's prefetch holds three slots and is the one in
		// flight; the runs of the next three are left unread for the bound,
		// and the fifth finds a slot for its own page alone.
		for _, page := range []uint64{0, 4, 8, 12, 16} {
			if err := r.Fault(f.ctx, page, false); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		if err := r.Fault(f.ctx, 20, false); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(started); took != pageRead {
			t.Fatalf("the fault short of a slot took %v, want %v: its own page's read", took, pageRead)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		s := hostStats(t, f)
		// The cancelled prefetch's three slots came back, the fault took one,
		// and its own prefetch, the one in flight now, the other two.
		if s.Evictions != 0 || s.PrefetchCancelled != 1 || s.PrefetchRefused != 3 || s.PrefetchDropped != 3 ||
			s.PrefetchedPages != 2 {
			t.Fatalf("evictions %d, cancelled %d, refused %d, dropped %d, landed %d; want 0, 1, 3, 3, 2",
				s.Evictions, s.PrefetchCancelled, s.PrefetchRefused, s.PrefetchDropped, s.PrefetchedPages)
		}
		for _, page := range []uint64{0, 4, 8, 12, 16, 20} {
			requirePage(t, m, page)
		}
		if probes := sim.RuntimeFrom(f.ctx).Probes(); probes[vmmemory.ProbePrefetchCancelled] != 1 ||
			probes[vmmemory.ProbePrefetchRefused] != 3 {
			t.Fatalf("probes %v, want the prefetch cancelled once and refused three times", probes)
		}
	})
}

// A page a migration's source turns out still to hold is the guest's own state
// and not the identity its volume names, so a prefetch drops it: only a fault
// may take it, as the memory region's dirty state. The rest of the run lands.
func TestAPrefetchLeavesAPageTheSourceHoldsToItsFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		peer := &peerBacking{backing: f.newBacking(8), hidden: map[uint64]byte{2: 99}}
		r, m := f.attach(peer)
		if err := r.Fault(f.ctx, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if _, mapped := m.mappedPage(2); mapped {
			t.Fatal("the prefetch mapped a page the source holds")
		}
		for _, page := range []uint64{0, 1, 3, 4, 5, 6, 7} {
			requirePage(t, m, page)
		}
		if got := accessUnder(f.ctx, t, r, m, 2, false)[0]; got != 99 {
			t.Fatalf("the page the source holds reads %d, want the 99 it served", got)
		}
		s := hostStats(t, f)
		if s.PrefetchDropped != 1 || s.DirtyPages != 1 || !peer.holds(2) {
			t.Fatalf("dropped %d, dirty %d, the source told the page was taken %t; want 1, 1, true",
				s.PrefetchDropped, s.DirtyPages, peer.holds(2))
		}
		if probes := sim.RuntimeFrom(f.ctx).Probes(); probes[vmmemory.ProbePrefetchHeld] != 1 {
			t.Fatalf("probes %v, want the held page dropped once", probes)
		}
	})
}

// A post-copy stream's faults read whole runs: nothing waits on them.
func TestAStreamsFaultReadsItsWholeRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := f.slowBacking(8)
		r, m := f.attach(b)
		started := time.Now()
		if err := r.Fault(vmmemory.WithStream(f.ctx), 3, false); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(started); took != readCost+8*pageCost {
			t.Fatalf("the stream's fault took %v, want its run's read", took)
		}
		for page := range uint64(8) {
			requirePage(t, m, page)
		}
		if s := hostStats(t, f); s.Prefetches != 0 || s.Loads != 1 {
			t.Fatalf("prefetches %d, loads %d; want none and one", s.Prefetches, s.Loads)
		}
	})
}

// A detach cancels its memory region's prefetches and waits for them to end
// before it takes anything away, because they read the region's backing,
// which is the caller's to close once the detach returns. It does not wait
// for a held read to finish: the prefetch is cancelled, its slots go back and
// nothing it read is left behind.
func TestADetachCancelsItsPrefetches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := f.slowBacking(16)
		b.held = make(chan struct{})
		time.AfterFunc(time.Hour, func() { close(b.held) })
		r, m := f.attach(b)
		for _, page := range []uint64{0, 8} {
			if err := r.Fault(f.ctx, page, false); err != nil {
				t.Fatal(err)
			}
		}
		m.arena.mu.Lock()
		clear(m.pages)
		m.arena.mu.Unlock()
		began := time.Now()
		if err := r.Detach(f.ctx); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(began); took != 0 {
			t.Fatalf("the detach took %v, want no wait for the held prefetches", took)
		}
		s := hostStats(t, f)
		if s.Prefetches != 2 || s.PrefetchDropped != 14 || s.PrefetchedPages != 0 || s.ResidentPages != 2 ||
			s.IdlePages != 2 {
			t.Fatalf("prefetches %d, dropped %d, landed %d, resident %d, idle %d; want 2, 14, 0, 2, 2",
				s.Prefetches, s.PrefetchDropped, s.PrefetchedPages, s.ResidentPages, s.IdlePages)
		}
	})
}
