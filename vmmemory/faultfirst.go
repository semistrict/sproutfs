package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
)

// A fault reads its window one of three ways, decided before it plans
// anything (planFault), and plans only what that way reads:
//
//   - A fault at random, one that follows none of its memory region's recent
//     faults (followsRecent), reads its page alone (readAlone). Its plan is
//     its page: it locates that page, takes it — bound to a resident page
//     under its identity, or a slot to read it into — and reads it. It plans
//     nothing of the rest of its window, which it neither reads nor maps.
//   - A fault that follows a recent one reads its page first and prefetches
//     the rest of its window behind it (readFirst). It locates its page
//     alone, takes it, and starts its read on a task of its own. Only then
//     does it locate the rest of its window, in one lookup, and plan it: the
//     resident pages it maps beside its own, and the slots of the pages the
//     prefetch reads, both found for the whole window at once. The read is
//     under way while it plans, so planning the window costs the fault
//     nothing while it takes less than the read.
//   - A post-copy stream's fault reads its whole window at once with its page
//     (readRun), so it locates the whole window first. Nothing waits on it.
//
// A fault used to plan its whole window, whichever it was, and at 4 KiB a
// window is 2,048 pages. Before it read anything, on GCE on 2026-10-04 that
// planning took a dependent 4 KiB fault from the cluster to 3.2 ms against
// 0.67 ms for the page's read alone
// (docs/measurements/gce-fault-first-2026-10-04.md). Planned behind the read,
// it still took 1.05 ms: locating 2,048 pages and looking each up among the
// resident pages took about 0.75 ms of processor a fault, as long as the read,
// and a fault at random used none of it but the resident pages it mapped
// (docs/measurements/gce-fault-planning-2026-10-04.md). Such a fault costs its
// guest at most one more fault in its window: the next fault there follows
// this one, and plans the window.

// WorkPlan is the work of planning a window, one unit a page located, which a
// simulation prices (sim.Config.Compute) so that a test sees a fault's
// planning take time.
const WorkPlan = "vmmemory/plan"

// reading is how a fault reads its window.
type reading int

const (
	// readAlone reads the faulting page alone and plans nothing else: a
	// fault that follows none of its memory region's recent faults.
	readAlone reading = iota
	// readFirst reads the faulting page first and prefetches the rest of
	// the window behind it: a fault that follows one.
	readFirst
	// readRun reads the whole window at once with the faulting page: a
	// post-copy stream's fault.
	readRun
)

// planFault plans the window of the page index for the faulting page fault, or
// the window's end for none, as far as the fault will read it: the whole
// window, located at once, for a fault that reads its run first; the window,
// with the faulting page located alone, for one that reads its page first; and
// the faulting page alone for one at random.
func (r *MemoryRegion) planFault(ctx context.Context, index, fault uint64) (*windowPlan, error) {
	start, end := r.window(index)
	var p *windowPlan
	var err error
	how := readAlone
	switch {
	case runFirst(ctx):
		how = readRun
		p, err = r.plan(ctx, start, end, fault)
	case r.followsRecent(start) || sim.Bug(ctx, "pager-prefetch-every-fault"):
		how = readFirst
		p, err = r.planPage(ctx, start, end, fault, index)
	case sim.Bug(ctx, "pager-plan-the-window-at-random"):
		// The bug plans the whole window of a fault at random, every page of
		// it located and looked up among the resident pages, as every fault
		// did.
		p, err = r.plan(ctx, start, end, fault)
	default:
		p, err = r.plan(ctx, index, index+1, fault)
	}
	if err != nil {
		return nil, err
	}
	p.reading = how
	return p, nil
}

// runFirst reports a fault that reads its whole run before its page is
// installed: a post-copy stream's, and every fault under the in-tree bug that
// puts the run back in front of the faulting page.
func runFirst(ctx context.Context) bool {
	return streaming(ctx) || sim.Bug(ctx, "pager-read-the-run-first")
}

// read brings the faulting page of a plan planFault made in, as the plan says
// the fault reads it. own says whether a run read first reads the pages whose
// bytes go in this memory region's own file too.
func (p *windowPlan) read(ctx context.Context, index uint64, own bool) error {
	switch p.reading {
	case readFirst:
		return p.readFirst(ctx, index)
	case readRun:
		if err := p.takeFaulting(ctx, index); err != nil {
			return err
		}
		if err := p.takeRun(ctx, index, own); err != nil {
			return err
		}
		return p.loadReserved(ctx)
	default:
		return p.readAlone(ctx, index)
	}
}

// readAlone reads the faulting page of a fault at random: it takes the page,
// binds whatever else its plan located that is resident — nothing, its plan
// being its page — and reads the page where it needs reading. The read is
// the fault's own, as a run read first is: nothing is planned beside it.
func (p *windowPlan) readAlone(ctx context.Context, index uint64) error {
	if err := p.takeFaulting(ctx, index); err != nil {
		return err
	}
	if err := p.bindNeighbours(ctx, p.survey(index, false)); err != nil {
		return err
	}
	if p.reserved[index-p.start].slot >= 0 {
		// Its neighbours are worth reading only to a guest that reads
		// forwards, and a prefetch nothing uses takes processors from the
		// faults that follow.
		h := p.memoryRegion.host
		h.mu.Lock()
		h.stats.PrefetchRandom++
		h.mu.Unlock()
		sim.Probe(ctx, ProbePrefetchRandom)
	}
	return p.loadReserved(ctx)
}

// readFirst serves the faulting page of a fault that reads its page first:
// it takes the page, starts its read, plans the rest of the window while the
// read is under way, starts the prefetch of the rest, and then waits for its
// own read and publishes the page. What the plan holds when it returns is
// installed by the caller, the faulting page with the resident pages of its
// window.
func (p *windowPlan) readFirst(ctx context.Context, index uint64) error {
	r := p.memoryRegion
	if err := p.takeFaulting(ctx, index); err != nil {
		return err
	}
	if sim.Bug(ctx, "pager-plan-the-window-first") {
		// The bug plans the whole window before the faulting page's read
		// starts, as every fault did.
		into, err := p.planRest(ctx, index)
		if err != nil {
			return err
		}
		read := p.beginFaulting(ctx, index)
		if pf := p.splitPrefetch(ctx, index, into); pf != nil {
			pf.begin()
		}
		return read.land(ctx, p)
	}
	read := p.beginFaulting(ctx, index)
	into, err := p.planRest(ctx, index)
	if err != nil {
		read.abandon()
		return err
	}
	if pf := p.splitPrefetch(ctx, index, into); pf != nil {
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
// A fault that reads its run first takes free slots for the run of pages
// around this one that need reading. One that reads its page first takes its
// slot out of a run of free slots for the whole window, at the page's place
// in it, so the pages the prefetch reads land beside it and the window is one
// run of slots: which of them need reading is not known until the window is
// located, and the slots of the ones that do not go back then
// (keepProvisional). A fault that reads its page alone takes one slot.
func (p *windowPlan) takeFaulting(ctx context.Context, index uint64) error {
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
	if p.reading == readRun {
		p.reserveAround(index)
	} else {
		p.reserveProvisional(index)
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
// for its whole window where the fault reads its page first. When fewer are
// free than the window, the run holds the faulting page and the pages after it
// first. Nothing is evicted; the page may remain unreserved.
func (p *windowPlan) reserveProvisional(index uint64) {
	file := p.fileOf(index)
	first, last := index, index+1
	if p.reading == readFirst {
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
// into that file (survey), and goes back otherwise.
func (p *windowPlan) keepProvisional(into []*arenaFile) {
	run := p.provisional
	p.provisional = provisionalRun{}
	var back []fileSlot
	for page, at := range run.slots {
		if into[page-p.start] == at.file {
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

// planRest locates the whole window of a fault that reads its page first, in
// one lookup, once the faulting page is in the plan, and takes the rest of it
// into the plan: every page whose identity is resident, bound to that page,
// and free slots, never an eviction, for the pages the prefetch reads. Which
// pages are which is asked for the whole window at once. It reports the file
// each page the prefetch reads goes in (survey).
func (p *windowPlan) planRest(ctx context.Context, index uint64) ([]*arenaFile, error) {
	if err := p.locateWindow(ctx); err != nil {
		return nil, err
	}
	found := p.survey(index, true)
	if err := p.bindNeighbours(ctx, found); err != nil {
		return nil, err
	}
	p.keepProvisional(found.into)
	return found.into, p.reserveRuns(ctx, index, found.into)
}

// takeRun takes the rest of a run read first into the plan once the faulting
// page is in it: every page whose identity is resident, bound to that page,
// and free slots, never an eviction, for the pages that need reading, the
// pages after the faulting one first. own says whether the pages whose bytes
// go in this memory region's own file are reserved there too.
func (p *windowPlan) takeRun(ctx context.Context, index uint64, own bool) error {
	found := p.survey(index, false)
	if err := p.bindNeighbours(ctx, found); err != nil {
		return err
	}
	if err := p.reserveRuns(ctx, index, found.into); err != nil {
		return err
	}
	if own {
		p.reserveOwn()
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
	h.mu.Lock()
	h.readNumber++
	number := h.readNumber
	h.mu.Unlock()
	readCtx, cancel := context.WithCancelCause(sim.WithTask(ctx, fmt.Sprintf("fault-read-%d", number)))
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
