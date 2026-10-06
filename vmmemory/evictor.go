package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The pager evicts with Zircon's evictor (internal/zirconvm/evictor.go). An
// allocation short of a slot calls its synchronous path for one page, and the
// evictor reclaims with reclaimStep, the pager's counterpart of Zircon's
// reclaim from the page queues: it takes one victim and reclaims it. The
// victim is, in order:
//
//   - the oldest idle page, which no memory region maps, so its slot costs no
//     revocation and no spill and nothing a guest is using;
//   - none while prefetches hold slots: they are cancelled, and the slots come
//     back as their reads end;
//   - the least recently faulted page that leaves every protected memory region
//     its pages, which is the fair share;
//   - the least recently faulted page of all;
//   - a page a cold copy will be compared with, last.
//
// The split is ReclaimPage's: an identity's clean page is dropped and read
// again, a memory region's own page is spilled (D2 of the port), and a cold
// copy the guest did not change goes back to its origin.
//
// The evictor's asynchronous path is not used. Nothing here evicts ahead of a
// shortage: an idle page is kept for the next VM that inherits its identity
// (see Idle pages in docs/vm-memory.md), and evicting it early would cost that
// VM a read.

// pagerEvictor is the evictor of the pager's resident pages.
type pagerEvictor = zirconvm.Evictor[*evictionRequest]

// evictionRequest is one allocation's eviction: whom it evicts for, and what
// the evictor's steps found, which the allocation reads when nothing was
// evicted.
type evictionRequest struct {
	// region is the memory region the slot is for, whose own pages the fair
	// share does not protect from it. It is nil for a slot of no region.
	region *MemoryRegion
	// preferEviction passes over idle pages and prefetches' slots, as
	// evictPastAFreeSlot asks, until a victim is found.
	preferEviction bool
	// changed is the host's signal as the last step left it, which the
	// allocation waits on when it found nothing.
	changed <-chan struct{}
	// busy reports a candidate whose lock was held, or slots of a load in no
	// queue yet: something to wait for rather than fail.
	busy bool
	// prefetches reports prefetches holding slots that come back as their
	// reads end.
	prefetches bool
	// file is the file the slot is for, where a free slot of it ends the
	// allocation's need: nil where the placement rule decides the slot, or
	// where the look found a free slot the budget refused.
	// freed reports that one came free after the allocation looked and before
	// the step did: the allocation takes it rather than evict.
	file  *arenaFile
	freed bool
}

// newPagerEvictor is h's evictor, enabled. It compresses: a memory region's
// own page is spilled rather than dropped.
func newPagerEvictor(h *Host) *pagerEvictor {
	e := zirconvm.NewEvictorWith(h.pageSize, h.reclaimStep, h.freePages)
	e.EnableEviction(true)
	return e
}

// freePages is how many more pages the arena may hold.
func (h *Host) freePages() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return uint64(max(h.cfg.ResidentPages-h.held, 0))
}

// evictOne evicts one page for req by the evictor's synchronous path, and
// reports whether any page was evicted while it ran, by it or beside it.
func (h *Host) evictOne(ctx context.Context, req *evictionRequest) (bool, error) {
	req.changed, req.busy, req.prefetches, req.freed = nil, false, false, false
	result, err := h.evictor.EvictSynchronous(ctx, req, h.pageSize, 0,
		zirconvm.IncludeNewest, zirconvm.NoPrint, zirconvm.Other)
	return result.Counts.Total() > 0, err
}

// evictPastAFreeSlot reports an allocation that takes a victim even where a
// free slot or an idle page would do, for one pass. That is legal and merely
// wasteful, and it is the only way an arena that is not full reaches the
// eviction paths at all: a pager sized to hold its whole guest never evicts,
// so nothing ever overlaps an eviction with a publication or a seal.
func evictPastAFreeSlot(ctx context.Context) bool {
	return sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5)
}

// reclaimStep is one step of the pager's evictor: it takes one victim for req
// and reclaims it, or reports that there is nothing to take. It always
// compresses, and every level takes a mapped page in the end, since an
// allocation short of a slot has nowhere else to get one.
func (h *Host) reclaimStep(ctx context.Context, req *evictionRequest, _ bool, _ zirconvm.EvictionLevel) (zirconvm.ReclaimAttempt, bool, error) {
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return zirconvm.ReclaimAttempt{}, false, err
	}
	if !req.preferEviction {
		h.mu.Unlock()
		if h.takeIdle() {
			return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: zirconvm.ReclaimEvict, NumPages: 1}},
				true, nil
		}
		h.mu.Lock()
		// The slots of a prefetch still reading come next: nothing waits on
		// its pages. See prefetch.go.
		if !sim.Bug(ctx, "pager-prefetch-ignores-pressure") && h.cancelPrefetchesLocked(ctx) {
			req.prefetches, req.changed = true, h.changed
			h.mu.Unlock()
			return zirconvm.ReclaimAttempt{}, false, nil
		}
	}
	// A slot that came free after the allocation looked, as a cancelled
	// prefetch's do when it settles, is taken rather than a page a guest
	// maps, as in the current core.
	if !req.preferEviction && req.file != nil && h.freeLocked(req.file) > 0 &&
		!sim.Bug(ctx, "pager-evict-past-a-freed-slot") {
		req.freed = true
		h.mu.Unlock()
		return zirconvm.ReclaimAttempt{}, false, nil
	}
	page := h.peekVictimLocked(req)
	req.changed = h.changed
	// Slots reserved by a concurrent load are in no queue yet.
	if h.queuedLocked() < h.cfg.ResidentPages {
		req.busy = true
	}
	h.mu.Unlock()
	if page == nil {
		return zirconvm.ReclaimAttempt{}, false, nil
	}
	req.preferEviction = false
	return h.reclaimVictim(ctx, page)
}

// evictionSeam runs in a reclaim between reading one victim's aliases and
// reading the reservations those aliases name, which is the one moment a seal
// can move a page's reservation to the checkpoint's copy of it without the
// reclaim seeing either state. Production leaves it nil; a test installs it to
// take a seal exactly there.
var evictionSeam func(slot int)
