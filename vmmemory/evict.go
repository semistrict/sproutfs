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
//   - after the step harvests (harvest.go), the least recently used page that
//     leaves every protected memory region its pages, which is the fair
//     share: a harvested page the guest has not touched since, before an
//     isolated page not harvested yet;
//   - the least recently used page of all;
//   - a page a cold copy will be compared with, last.
//
// A cold copy the guest did not change goes back to its origin rather than be
// spilled (giveback.go).
//
// The evictor's asynchronous path is not used. Nothing here evicts ahead of a
// shortage: an idle page is kept for the next VM that inherits its identity
// (see Idle pages in docs/vm-memory.md), and evicting it early would cost that
// VM a read.

// The evictor's synchronous path runs over the node's page queues, with the
// fair share as a filter on its candidates. The split is ReclaimPage's with D2
// of the port: a root's page is dropped and read again
// (ReclaimRangeForEviction's case), and a page of a region's layer, Dirty,
// AwaitingClean or Clean, has its bytes written to the reservations of the
// bindings it is the state of (ReclaimPageForCompression's case) and is taken
// out of the layer. The reservation, the reference, stays in the binding
// beside the layer: the frame's bytes are the pager's to move, and the layer's
// node keeps no storage of its own. Revoking every alias before the slot goes
// back is RangeChangeUpdateLocked with UnmapAndHarvest over every region in
// the alias set.

// The pages are ordered for reclaim by Zircon's page queues
// (internal/zirconvm/pagequeues.go):
//
//   - Every page a memory region may map is in a reclaim queue, or in an
//     isolate queue once it has aged out of them. The queues age one
//     generation for each fault served (AgeOnAccess), so an eviction walks
//     them in the order a fault last touched each page. The pager sees no
//     access through a page table it has filled, so the evictor harvests a
//     page before it takes it, and the access after faults (harvest.go).
//   - An idle page, which no memory region maps, is in the don't-need queue,
//     which Zircon's peek takes first. The evictor takes its victims by
//     peeking these queues.
//   - A page a cold copy will be compared with is in the zero-fork queue. In
//     Zircon that queue holds the pages a write fault copied from the zero page,
//     outside the reclaim queues until the scanner has compared them with zero.
//     Here the comparison's other side is a page of the arena rather than the
//     zero page, and it is that page which must wait outside the reclaim
//     queues, so it is the page queued there; an eviction takes it only when
//     nothing else can go. See cold.go.
//
// A page's queue is decided by whether it is idle and whether it is pinned,
// which are both changed under Host.pinMu, so its moves between the queues are
// made under it too. Host.mu is taken before pinMu, and the queues' own locks
// after both.

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
		// h.mu is given up for the idle drop, which takes it itself. Nothing
		// read under the hold before is used after it: the rest of the step
		// looks at the prefetches, the free slots and the queues afresh. A
		// host that went terminal meanwhile still goes on to a victim, as a
		// step begun just before would, and the allocation's next look
		// reports it.
		h.mu.Unlock()
		if h.takeIdle() {
			return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: zirconvm.ReclaimEvict, NumPages: 1}},
				true, nil
		}
		// In a controlled run another task may go on here, between the idle
		// drop that found nothing and the victim: a prefetch that lands idle
		// pages, or a fault that maps the page this step would take.
		if err := sim.Admit(ctx, "vmmemory/reclaim-step"); err != nil {
			return zirconvm.ReclaimAttempt{}, false, err
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
	if h.freedLocked(ctx, req) {
		h.mu.Unlock()
		return zirconvm.ReclaimAttempt{}, false, nil
	}
	// The step takes a page a guest maps from here, so it harvests first, with
	// h.mu given up: what it reads after is read afresh, a slot that came free
	// meanwhile too. See harvest.go.
	h.mu.Unlock()
	if err := h.harvest(ctx); err != nil {
		return zirconvm.ReclaimAttempt{}, false, err
	}
	h.mu.Lock()
	if h.freedLocked(ctx, req) {
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

// freedLocked reports a slot that came free after the allocation looked, as a
// cancelled prefetch's do when it settles, and marks req to take it rather
// than a page a guest maps. Caller holds h.mu.
func (h *Host) freedLocked(ctx context.Context, req *evictionRequest) bool {
	if req.preferEviction || req.file == nil || h.freeLocked(req.file) == 0 ||
		sim.Bug(ctx, "pager-evict-past-a-freed-slot") {
		return false
	}
	req.freed = true
	return true
}

// evictionSeam runs in a reclaim between reading one victim's aliases and
// reading the reservations those aliases name, which is the one moment a seal
// can move a page's reservation to the checkpoint's copy of it without the
// reclaim seeing either state. Production leaves it nil; a test installs it to
// take a seal exactly there.
var evictionSeam func(slot int)

// queuedLocked is how many pages of the arena the queues hold: every page of
// every object, less the pages of a temporary root, which name another's
// frame. Caller holds h.mu.
func (h *Host) queuedLocked() int {
	counts := h.node.PageQueues().QueueCounts()
	return counts.Total() - counts.Wired
}

// peekVictimLocked is the victim the node's queues give: the least recently
// used page that leaves every protected region its pages, then the least
// recently used of all, then a page a cold copy pins, each returned locked.
// The isolate queues give a harvested page before one not harvested yet.
// Caller holds h.mu.
func (h *Host) peekVictimLocked(req *evictionRequest) *zirconvm.VmPage {
	share := h.cfg.ResidentPages / max(len(h.memoryRegions), 1)
	candidate := func(fair bool) func(*zirconvm.VmPage) bool {
		return func(p *zirconvm.VmPage) bool {
			if isLent(p) {
				return false
			}
			f := frameOf(p)
			if fair && !h.fairLocked(f, req.region, share) {
				return false
			}
			if !f.mu.TryLock() {
				req.busy = true
				return false
			}
			if h.usableVictimLocked(f) {
				return true
			}
			f.mu.Unlock()
			return false
		}
	}
	queues := h.node.PageQueues()
	for _, fair := range []bool{true, false} {
		if victim, ok := queues.PeekIsolateWhere(0, candidate(fair)); ok {
			return victim.Page
		}
	}
	if victim, ok := queues.PeekAnonymousZeroForkWhere(candidate(false)); ok {
		return victim.Page
	}
	return nil
}

// fairLocked reports whether evicting f leaves every protected memory region
// other than r its pages: no alias of f is a protected region's. Caller holds
// h.mu.
func (h *Host) fairLocked(f *frame, r *MemoryRegion, share int) bool {
	for b := range f.aliases.all() {
		if q := b.region; q != r && h.protectedLocked(q, share) {
			return false
		}
	}
	return true
}

// usableVictimLocked is usableVictimLocked over a frame: no store is
// replacing it, and no terminal region maps it. Caller holds h.mu and the
// page's lock.
func (h *Host) usableVictimLocked(f *frame) bool {
	if f.replacing != 0 || f.slot < 0 {
		return false
	}
	if f.layer != nil && f.layer.detaching.Load() {
		// Its region is detaching, and gives the page back itself.
		return false
	}
	for b := range f.aliases.all() {
		if b.region.terminal.Load() != nil {
			return false
		}
	}
	return true
}

// reclaimVictim reclaims a victim peekVictimLocked returned locked, and
// unlocks it, as Host.reclaimVictim does.
func (h *Host) reclaimVictim(ctx context.Context, page *zirconvm.VmPage) (zirconvm.ReclaimAttempt, bool, error) {
	defer h.unlockPage(page)
	// A cold copy the guest did not change goes back to the page it was
	// copied from rather than to the spill: see cold.go.
	given, err := h.giveBackVictim(ctx, page)
	if err != nil {
		return zirconvm.ReclaimAttempt{}, false, err
	}
	if given {
		return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: zirconvm.ReclaimCompress, NumPages: 1},
			Page: page}, true, nil
	}
	kind := zirconvm.ReclaimEvict
	if frameOf(page).layer != nil {
		kind = zirconvm.ReclaimCompress
	}
	if err := h.evictPage(ctx, page); err != nil {
		if errors.Is(err, errVictimHeld) {
			return zirconvm.ReclaimAttempt{Failure: zirconvm.ReclaimOther, Page: page}, true, nil
		}
		return zirconvm.ReclaimAttempt{}, false, err
	}
	return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: kind, NumPages: 1}, Page: page}, true, nil
}

// aliasesOf is the bindings that map a frame, read under h.mu.
func (h *Host) aliasesOf(f *frame) []*binding {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]*binding, 0, f.aliases.len())
	for b := range f.aliases.all() {
		result = append(result, b)
	}
	return result
}

// spillTarget is the reservation b's bytes go to, and whether a page that
// names none has them held elsewhere: by the checkpoint's copy, or because the
// page is not b's region's own state at all.
func (r *MemoryRegion) spillTarget(b *binding) (spill reservation, elsewhere bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.spill, !b.dirty || b.checkpoint != nil
}

// evictPage evicts a locked victim: it revokes every alias before reading its
// bytes, writes a region's own page to the reservations of the bindings it is
// the state of, takes it out of its object, and gives its slot back. The slot
// goes back only once the spill has succeeded.
func (h *Host) evictPage(ctx context.Context, page *zirconvm.VmPage) error {
	f := frameOf(page)
	// A page of a region's own layer has its bytes written to that region's
	// reservations, which a detach gives back, and it is taken out of that
	// region's layer, which a detach destroys. So the region is held live,
	// shared, from before its reservations are read until the page is gone,
	// as a give-back holds it. One that cannot be held without waiting is
	// detaching, or about to, and gives the page back itself: the victim is
	// held. The page's lock keeps its layer from changing meanwhile.
	if owner := f.layer; owner != nil && !sim.Bug(ctx, "pager-detach-under-an-eviction") {
		if !owner.live.TryRLock() {
			return errVictimHeld
		}
		defer owner.live.RUnlock()
	}
	// The reservations the page's bytes go to are read as the alias set
	// grows: a seal taken while this runs joins the checkpoint's copy to the
	// page before it hands that copy the page's reservation, so an alias set
	// that has not grown since its reservations were read names every one the
	// bytes can be in, and one read twice is written once.
	var spills []reservation
	taken := make(map[reservation]bool)
	byRegion := make(map[*MemoryRegion][]*binding)
	walked := make(map[*binding]bool)
	for grown := true; grown; {
		grown = false
		aliases := h.aliasesOf(f)
		if evictionSeam != nil {
			evictionSeam(f.slot)
		}
		for _, b := range aliases {
			if walked[b] {
				continue
			}
			walked[b], grown = true, true
			byRegion[b.region] = append(byRegion[b.region], b)
			if b.region.Checkpoint() != nil {
				// A publication is reading this region's pages while this
				// eviction takes one of them.
				sim.Probe(ctx, ProbeEvictionDuringPublication)
			}
			if f.layer == nil {
				// A root's page: its bytes are its identity's, read again.
				continue
			}
			spill, elsewhere := b.region.spillTarget(b)
			if spill.none() {
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
	for _, q := range inAttachOrder(byRegion) {
		if err := q.revokeBindings(ctx, byRegion[q]); err != nil {
			// A region that cannot take the mapping away is terminal from
			// here, and this page is excluded from every later step by it.
			q.heldPages(ctx, err)
			return errors.Join(errVictimHeld, err)
		}
	}
	sort.Slice(spills, func(i, j int) bool { return spills[i].ref.Value() < spills[j].ref.Value() })
	if len(spills) > 0 {
		ps := int(h.pageSize)
		data := make([]byte, len(spills)*ps)
		refs := make([]zirconvm.ReferenceValue, len(spills))
		for i, spill := range spills {
			if err := f.file.Read(ctx, f.slot, data[i*ps:(i+1)*ps]); err != nil {
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
	// In a controlled run another task may go on here, with the page's
	// mappings gone and its bytes written away but the page still in its
	// object: a fault on it, which finds it held and waits, or a seal of a
	// region that maps it, which joins the checkpoint's copy to it.
	if err := sim.Admit(ctx, "vmmemory/evict-remove"); err != nil {
		return err
	}
	// Out of its object, so no lookup finds it; then nothing names it, and its
	// slot goes back. The aliases are taken off under a hold of h.mu of their
	// own, whatever they are by then. The page's lock, held throughout, keeps
	// out every path that names a page but a seal's walk, which names it to
	// the checkpoint's copy of a binding that names it, and hands that copy
	// the binding's reservation, which the bytes went to: so the copy dropped
	// here with the rest finds its bytes there. Once they are off, nothing names
	// the page, and releaseFrame's look finds none.
	h.removeFromObject(page)
	h.mu.Lock()
	h.stats.Evictions++
	if f.aliases.len() > 0 {
		h.displaced++
	}
	h.dropAliasesLocked(f)
	h.mu.Unlock()
	if evictedSeam != nil {
		evictedSeam(f.slot)
	}
	h.releaseFrame(page)
	return nil
}

// evictedSeam runs once an eviction has taken every alias off its page and
// before it gives the page's slot back, where nothing maps the page and its
// slot is still taken. Production leaves it nil.
var evictedSeam func(slot int)

// removeFromObject takes a locked page out of whichever object holds it, and
// its name out of the temporary root that lends it.
//
// Each object's lock and h.mu are held one after another, not together. The
// page's lock, which the caller holds, keeps it from being lent anew or moved
// between them. The name it is lent under is read and cleared under h.mu, as
// the end of the fork point's seal reads the names it lent, and either may
// then remove it: RemovePageLocked looks again under the root's lock, so
// whichever comes second removes nothing.
func (h *Host) removeFromObject(page *zirconvm.VmPage) {
	if link, ok := h.node.PageQueues().Backlink(page); ok {
		lock := link.Cow.Lock()
		lock.Lock()
		link.Cow.RemovePageLocked(link.Offset, page)
		lock.Unlock()
	}
	h.dropLentName(page)
}

// dropAliasesLocked takes every alias off a frame that is going, which an
// eviction does: each binding names no page from here. Caller holds h.mu.
func (h *Host) dropAliasesLocked(f *frame) {
	var bindings []*binding
	for b := range f.aliases.all() {
		bindings = append(bindings, b)
	}
	for _, b := range bindings {
		f.aliases.remove(b)
		if !f.mappedBy(b.region) {
			b.region.resident--
		}
		if b.page != nil && frameOf(b.page) == f {
			b.page = nil
		}
	}
}
