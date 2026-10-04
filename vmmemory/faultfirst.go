package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
)

// A fault plans its own page first. It locates that page alone, takes it —
// bound to a resident page under its identity, or a slot to read it into —
// and starts its read on a task of its own. Only then does it locate the rest
// of its window and plan it: the resident pages it maps beside its own, and,
// when it prefetches, the slots of the pages the prefetch reads. The read is
// under way while it plans, so planning the window costs the fault nothing
// while it takes less than the read.
//
// A fault used to plan its whole window before it read anything. At 4 KiB a
// window is 2,048 pages, and on GCE on 2026-10-04 that planning was 0.62 s of
// the 1.42 s a chain of 400 faults spent: a dependent 4 KiB fault from the
// cluster took 3.2 ms against 0.67 ms for the page's read alone
// (docs/measurements/gce-fault-first-2026-10-04.md). A fault that does not
// prefetch now takes no slot for its neighbours, where it took one for each
// and gave them back.
//
// A post-copy stream's fault still plans its whole window first: it reads
// the window in one read with its page, and nothing waits on it.

// WorkPlan is the work of planning a window, one unit a page located, which a
// simulation prices (sim.Config.Compute) so that a test sees a fault's
// planning take time.
const WorkPlan = "vmmemory/plan"

// rest is what a fault does with the rest of its window once its own page is
// planned.
type rest int

const (
	// restMapped maps the window's resident pages beside the faulting page
	// and reads nothing else: a fault that follows none of its memory
	// region's recent faults.
	restMapped rest = iota
	// restPrefetched maps them too, and prefetches the pages that need
	// reading: a fault that does follow one.
	restPrefetched
)

// readFirst serves the faulting page of a plan that has located that page
// alone: it takes the page, starts its read, plans the rest of the window
// while the read is under way, starts the prefetch of the rest where the fault
// prefetches, and then waits for its own read and publishes the page. What the
// plan holds when it returns is installed by the caller, the faulting page
// with the resident pages of its window.
func (p *windowPlan) readFirst(ctx context.Context, index uint64) error {
	r := p.memoryRegion
	then := restMapped
	if r.followsRecent(p.start) || sim.Bug(ctx, "pager-prefetch-every-fault") {
		then = restPrefetched
	}
	if err := p.takeFaulting(ctx, index, then); err != nil {
		return err
	}
	if sim.Bug(ctx, "pager-plan-the-window-first") {
		// The bug plans the whole window before the faulting page's read
		// starts, as every fault did.
		if err := p.planRest(ctx, index, then); err != nil {
			return err
		}
		read := p.beginFaulting(ctx, index)
		if pf := p.splitPrefetch(ctx, index); pf != nil {
			pf.begin()
		}
		return read.land(ctx, p)
	}
	read := p.beginFaulting(ctx, index)
	if err := p.planRest(ctx, index, then); err != nil {
		read.abandon()
		return err
	}
	if pf := p.splitPrefetch(ctx, index); pf != nil {
		pf.begin()
		if sim.Bug(ctx, "pager-fault-waits-for-its-prefetch") {
			// The bug installs the page only once the rest of its run is in.
			if err := r.withoutMemoryRegion(ctx, func() error {
				select {
				case <-pf.done:
					return nil
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}); err != nil {
				read.abandon()
				return err
			}
		}
	}
	return read.land(ctx, p)
}

// takeFaulting takes the faulting page into the plan before any other: bound
// to its resident identity if one exists, otherwise a slot, and as a last
// resort one an eviction frees. Waiting here is safe because the plan holds no
// other resident lock yet. A page whose bytes go in this memory region's own
// file has a place of its own there, and only it may evict for it.
//
// Where the fault will prefetch, the slot is one of a run of free slots taken
// for the whole window, at the page's place in it, so the pages the prefetch
// reads land beside it and the window is one run of slots: which of them need
// reading is not known until the window is located, and the slots of the ones
// that do not go back then (keepProvisional). A fault that reads its page
// alone takes one slot.
func (p *windowPlan) takeFaulting(ctx context.Context, index uint64, then rest) error {
	r := p.memoryRegion
	if err := p.bindShared(ctx, index, true); err != nil {
		return err
	}
	i := index - p.start
	if p.pages[i] != nil || p.zeros[i] {
		return nil
	}
	if p.own(index) {
		at, err := r.reclaimOwn(ctx, index, !p.unpublished(index))
		if err != nil {
			return err
		}
		p.reserve(index, at)
		return nil
	}
	if p.located {
		// A stream's plan has located the window: the run is the pages
		// around this one that need reading.
		p.reserveAround(index)
	} else {
		p.reserveProvisional(index, then)
	}
	if p.reserved[i].slot >= 0 {
		return nil
	}
	at, err := r.reclaim(ctx, p.file)
	if err != nil {
		return err
	}
	p.reserve(index, at)
	return nil
}

// provisionalRun is the run of free slots a fault that prefetches took for its
// window before it located it: count slots from at, for the pages from first.
// The faulting page's is reserved; the rest wait for keepProvisional, and a
// plan unlocked before they are settled gives them back.
type provisionalRun struct {
	first, faulting uint64
	at              fileSlot
	count           int
}

// slots is every slot of the run but the faulting page's, with its page.
func (run provisionalRun) slots(yield func(uint64, fileSlot) bool) {
	for k := range run.count {
		if page := run.first + uint64(k); page != run.faulting && !yield(page, run.at.plus(k)) {
			return
		}
	}
}

// reserveProvisional takes the faulting page's slot, from a run of free slots
// for its whole window where the fault prefetches. When fewer are free than
// the window, the run holds the faulting page and the pages after it first.
// Nothing is evicted; the page may remain unreserved.
func (p *windowPlan) reserveProvisional(index uint64, then rest) {
	file := p.fileOf(index)
	first, last := index, index+1
	if then == restPrefetched {
		first, last = p.start, p.end
	}
	at, count := p.memoryRegion.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	p.reserve(index, at.plus(int(index-start)))
	if count > 1 {
		p.provisional = provisionalRun{first: start, faulting: index, at: at, count: count}
	}
}

// keepProvisional settles the provisional run once the window is located:
// each slot stays reserved for its page where the prefetch will read that page
// into that file, and goes back otherwise.
func (p *windowPlan) keepProvisional() {
	run := p.provisional
	p.provisional = provisionalRun{}
	var back []fileSlot
	for page, at := range run.slots {
		if p.needsLoad(page) && p.prefetchable(page) && p.fileOf(page) == at.file {
			p.reserve(page, at)
			continue
		}
		back = append(back, at)
	}
	if len(back) == 0 {
		return
	}
	h := p.memoryRegion.host
	h.mu.Lock()
	for _, at := range back {
		h.putFree(at)
	}
	h.signal()
	h.mu.Unlock()
}

// prefetchable reports a page a prefetch may read: one that lands as a clean
// shared page, under an identity of the volume's that no other host holds and
// in a file other memory regions may read.
func (p *windowPlan) prefetchable(page uint64) bool {
	key, named := p.identity(page)
	return named && !key.zero() && !p.unpublished(page) && !p.own(page)
}

// planRest locates the whole window and takes the rest of it into the plan
// once the faulting page is in it: every page whose identity is resident,
// bound to that page, and, where the fault prefetches, free slots for the
// pages the prefetch reads, never an eviction. A fault that does not
// prefetch counts the run it leaves unread.
func (p *windowPlan) planRest(ctx context.Context, index uint64, then rest) error {
	if err := p.locateWindow(ctx); err != nil {
		return err
	}
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		if page == index || p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !p.eligible(page) {
			continue
		}
		if err := p.bindShared(ctx, page, false); err != nil {
			return err
		}
	}
	p.keepProvisional()
	wanted := func(page uint64) bool { return page != index && p.needsLoad(page) && p.prefetchable(page) }
	if then == restPrefetched {
		return p.reserveRuns(ctx, index, wanted)
	}
	for page := p.start; page < p.end; page++ {
		if wanted(page) {
			// A fault that follows none of its memory region's recent faults
			// is read alone: its neighbours are worth reading only to a guest
			// that reads forwards, and a prefetch nothing uses takes
			// processors from the faults that follow.
			h := p.memoryRegion.host
			h.mu.Lock()
			h.stats.PrefetchRandom++
			h.mu.Unlock()
			sim.Probe(ctx, ProbePrefetchRandom)
			return nil
		}
	}
	return nil
}

// faultRead is the faulting page's backing read, under way on a task of its
// own while the fault plans the rest of its window.
type faultRead struct {
	host        *Host
	page        uint64
	buffer      *[]byte
	unpublished []bool
	err         error
	done        chan struct{}
	cancel      context.CancelCauseFunc
}

// beginFaulting starts the read of the faulting page where the plan reserved a
// slot for it, and returns nil where the page needs no read: it is bound to a
// resident page, or reads as zeros.
func (p *windowPlan) beginFaulting(ctx context.Context, index uint64) *faultRead {
	if p.reserved[index-p.start].slot < 0 {
		return nil
	}
	r := p.memoryRegion
	h := r.host
	// The read's task is named by its page, under the task of the fault:
	// nothing another task does changes its name, so a controlled run
	// orders it the same way whatever order its faults began in.
	readCtx, cancel := context.WithCancelCause(sim.WithTask(ctx, fmt.Sprintf("fault-read-%d", index)))
	read := &faultRead{host: h, page: index, buffer: h.takeWindow(1), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(read.done)
		// In a controlled run the read begins when the run chooses, not
		// beside whatever the fault plans next.
		if read.err = sim.Admit(readCtx, "vmmemory/fault-read"); read.err != nil {
			return
		}
		read.unpublished, read.err = r.readRun(readCtx, index, []bool{true}, *read.buffer, &h.loadLatency)
	}()
	return read
}

// land waits for the read, with the memory region given up as a backing read
// gives it up, and publishes the page into the slot the plan reserved for it.
// A nil read has nothing to land.
func (read *faultRead) land(ctx context.Context, p *windowPlan) error {
	if read == nil {
		return nil
	}
	defer read.cancel(nil)
	defer read.host.putWindow(read.buffer)
	r := p.memoryRegion
	err := r.withoutMemoryRegion(ctx, func() error {
		select {
		case <-read.done:
		case <-ctx.Done():
			read.cancel(context.Cause(ctx))
			<-read.done
			return context.Cause(ctx)
		}
		// The read's goroutine ended at an instant of its own; in a controlled
		// run the fault goes on when the run chooses.
		if err := sim.Admit(ctx, "vmmemory/fault-read-landed"); err != nil {
			return err
		}
		return read.err
	})
	if err != nil {
		return err
	}
	return p.publishRead(ctx, read.page, []bool{true}, *read.buffer, read.unpublished)
}

// abandon ends a read the fault no longer wants and waits for it, so its
// buffer is free to go back.
func (read *faultRead) abandon() {
	if read == nil {
		return
	}
	read.cancel(errReadAbandoned)
	<-read.done
	read.host.putWindow(read.buffer)
}

// errReadAbandoned is what a faulting page's read is cancelled with when the
// fault failed to plan the rest of its window.
var errReadAbandoned = errors.New("vmmemory: the fault reading this page failed before its read landed")
