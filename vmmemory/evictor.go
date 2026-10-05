package vmmemory

import (
	"context"
	"errors"
	"sort"

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
	req.changed, req.busy, req.prefetches = nil, false, false
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
	if z := h.zircon; z != nil {
		return z.reclaimStep(ctx, req)
	}
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return zirconvm.ReclaimAttempt{}, false, err
	}
	if !req.preferEviction {
		if pg := h.takeIdleLocked(); pg != nil {
			h.mu.Unlock()
			if err := h.dropIdle(ctx, pg); err != nil {
				return zirconvm.ReclaimAttempt{}, false, err
			}
			return reclaimed(pg, zirconvm.ReclaimEvict), true, nil
		}
		// The slots of a prefetch still reading come next: nothing waits on
		// its pages, and a page a guest maps is one it is using. The
		// prefetches are cancelled, and their slots come back as their reads
		// end. See prefetch.go.
		if !sim.Bug(ctx, "pager-prefetch-ignores-pressure") && h.cancelPrefetchesLocked(ctx) {
			req.prefetches, req.changed = true, h.changed
			h.mu.Unlock()
			return zirconvm.ReclaimAttempt{}, false, nil
		}
	}
	pg := h.peekVictimLocked(req)
	req.changed = h.changed
	// Slots reserved by a concurrent load are in no queue yet.
	if h.queuedLocked() < h.cfg.ResidentPages {
		req.busy = true
	}
	h.mu.Unlock()
	if pg == nil {
		return zirconvm.ReclaimAttempt{}, false, nil
	}
	req.preferEviction = false
	return h.reclaimVictim(ctx, pg)
}

// reclaimed is the attempt that reclaimed pg as kind.
func reclaimed(pg *resident, kind zirconvm.ReclaimType) zirconvm.ReclaimAttempt {
	return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: kind, NumPages: 1}, Page: pg}
}

// peekVictimLocked locks and returns the page to evict for req, or nil where
// none can be taken now. A page the fair share protects is passed over while
// another can go, and so is a page a cold copy will be compared with: it is
// taken last, which ends those copies being cold. An arena whose every page
// is one of those, or the one page a store is copying from, has nothing else
// to give. Such a page waits in the zero-fork queue, outside the reclaim
// queues. See cold.go. Caller holds h.mu.
func (h *Host) peekVictimLocked(req *evictionRequest) *resident {
	share := h.cfg.ResidentPages / max(len(h.memoryRegions), 1)
	candidate := func(fair bool) func(*resident) bool {
		return func(pg *resident) bool {
			if fair && !h.fairLocked(pg, req.region, share) {
				return false
			}
			if !pg.mu.TryLock() {
				req.busy = true
				return false
			}
			if usableVictimLocked(pg) {
				return true
			}
			pg.mu.Unlock()
			return false
		}
	}
	// Every reclaim queue is peeked, the active ones too: the evictor must
	// find a page however recently every page was faulted.
	for _, fair := range []bool{true, false} {
		if victim, ok := h.queues.PeekIsolateWhere(0, candidate(fair)); ok {
			return victim.Page
		}
	}
	if victim, ok := h.queues.PeekAnonymousZeroForkWhere(candidate(false)); ok {
		return victim.Page
	}
	return nil
}

// usableVictimLocked reports whether a page whose lock is held may be
// evicted. A page a store is replacing is one the guest still reads through a
// mapping that names this offset, and the command that stops it naming it has
// not landed. It is not this reclaim's to take; the store gives it up itself
// once its mapping is in. A page a terminal memory region maps cannot be
// taken from it: see evictPage. Caller holds h.mu.
func usableVictimLocked(pg *resident) bool {
	if pg.replacing != 0 {
		return false
	}
	for b := range pg.aliases.all() {
		if b.memoryRegion.terminal.Load() != nil {
			return false
		}
	}
	return true
}

// reclaimVictim reclaims a victim peekVictimLocked returned locked, and
// unlocks it. A victim another memory region will not give up is not this
// allocation's failure: the next step passes over it, because that memory
// region is terminal from here, and takes another page. The arena is finite,
// so every such step removes one page from what the steps consider, and an
// arena made entirely of them reports ErrCapacity rather than spinning.
func (h *Host) reclaimVictim(ctx context.Context, pg *resident) (zirconvm.ReclaimAttempt, bool, error) {
	defer h.unlockAll([]*resident{pg})
	// A cold copy the guest did not change goes back to the page it was
	// copied from rather than to the spill: see cold.go. It is compressed, as
	// Zircon compresses a page of zeros to a marker.
	given, err := h.giveBackVictim(ctx, pg)
	if err != nil {
		return zirconvm.ReclaimAttempt{}, false, err
	}
	if given {
		return reclaimed(pg, zirconvm.ReclaimCompress), true, nil
	}
	kind := zirconvm.ReclaimEvict
	if pg.private {
		kind = zirconvm.ReclaimCompress
	}
	if err := h.evictPage(ctx, pg); err != nil {
		if errors.Is(err, errVictimHeld) {
			return zirconvm.ReclaimAttempt{Failure: zirconvm.ReclaimOther, Page: pg}, true, nil
		}
		return zirconvm.ReclaimAttempt{}, false, err
	}
	return reclaimed(pg, kind), true, nil
}

// evictionSeam runs in a reclaim between reading one victim's aliases and
// reading the reservations those aliases name, which is the one moment a seal
// can move a page's reservation to the checkpoint's copy of it without the
// reclaim seeing either state. Production leaves it nil; a test installs it to
// take a seal exactly there.
var evictionSeam func(slot int)

// evictPage evicts a locked victim: it revokes every alias before reading its
// bytes, writes a memory region's own page to its reservation in the spill,
// and releases its slot. A scratch write provides live read-after-write
// backing; no Sync is needed because only the volume quorum acknowledges
// durability. The slot is not released unless the spill has succeeded.
func (h *Host) evictPage(ctx context.Context, pg *resident) error {
	// The reservation the page's bytes go to is read once, here: a seal taken
	// while this runs hands the page's reservation to the checkpoint's copy of
	// it and joins that copy to the page, so a reservation can be named by the
	// page before this walk and by the copy after it, and is written once
	// either way.
	var spills []reservation
	taken := make(map[reservation]bool)
	byMemoryRegion := make(map[*MemoryRegion][]*binding)
	// The seal joins the checkpoint's copy to the page before it hands that
	// copy the page's reservation, so an alias set that has not grown since
	// its reservations were read names every reservation the page's bytes can
	// be in. One that has grown is walked again: the alias the reservation
	// moved to is in it, and a reservation this walk already read is not read
	// again, because the bytes are the same bytes whichever alias owns them by
	// the time they are written.
	walked := make(map[*binding]bool)
	for grown := true; grown; {
		grown = false
		aliases := h.aliases(pg)
		if evictionSeam != nil {
			evictionSeam(pg.slot)
		}
		for _, b := range aliases {
			if walked[b] {
				continue
			}
			walked[b], grown = true, true
			byMemoryRegion[b.memoryRegion] = append(byMemoryRegion[b.memoryRegion], b)
			if b.memoryRegion.Checkpoint() != nil {
				// The memory region is sealed: a publication is reading its pages
				// while this eviction punches one of them. Nothing may lose
				// bytes here, and nothing reaches it without arena pressure
				// at exactly the wrong moment.
				sim.Probe(ctx, ProbeEvictionDuringPublication)
			}
			if !pg.private {
				continue
			}
			spill, elsewhere := b.spillTarget()
			if spill.none() {
				// A page sharing a checkpoint's copy owns no reservation of its own:
				// the checkpoint's copy is the alias that spills those bytes, and so
				// does a machine that inherited the name a seal gave the page, whose
				// page is not its own state at all. Any other private page without one
				// would lose them here.
				if !elsewhere {
					return errors.New("private page has no spill reservation")
				}
				continue
			}
			if taken[spill] {
				continue
			}
			taken[spill] = true
			spills = append(spills, spill)
		}
	}
	for r, bindings := range byMemoryRegion {
		if err := r.revokeBindings(ctx, bindings); err != nil {
			// A memory region this page is also reachable from cannot take the mapping
			// away, which is what a machine whose memory session has stopped
			// answering looks like from here. The page therefore stays mapped
			// there and is not this host's to reuse — but that is a fact about
			// that memory region, which the failed revocation has just made terminal,
			// and not about whoever is evicting. A fan-out's children share
			// every page they inherited, so returning this to the caller ends
			// one machine for another machine's death and then the next for
			// that one's. The caller takes another victim instead; this page
			// is excluded from every later step by the memory region it could not be
			// taken from.
			r.heldPages(ctx, err)
			return errors.Join(errVictimHeld, err)
		}
	}
	// The storage writes each run of consecutive reservations in one write, so
	// the page goes to it in the order of its reservations.
	sort.Slice(spills, func(i, j int) bool { return spills[i].ref.Value() < spills[j].ref.Value() })
	if len(spills) > 0 {
		ps := int(h.pageSize)
		data := make([]byte, len(spills)*ps)
		refs := make([]zirconvm.ReferenceValue, len(spills))
		for i, spill := range spills {
			if err := pg.file.Read(ctx, pg.slot, data[i*ps:(i+1)*ps]); err != nil {
				return err
			}
			refs[i] = spill.ref
		}
		writes, err := h.spill.StoreReserved(ctx, refs, data)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.stats.SpillWrites += uint64(writes)
		h.stats.SpillWriteBytes += uint64(len(data))
		h.stats.Spills += uint64(len(spills))
		h.mu.Unlock()
	}
	if err := h.release(ctx, pg); err != nil {
		return err
	}
	h.mu.Lock()
	h.stats.Evictions++
	if pg.aliases.len() > 0 {
		h.displaced++
	}
	unaliasAllLocked(pg)
	h.mu.Unlock()
	return nil
}
