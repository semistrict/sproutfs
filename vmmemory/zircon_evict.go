package vmmemory

import (
	"context"
	"errors"
	"sort"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Eviction over the zircon core: the evictor's synchronous path
// (evictor.go) over the node's page queues, with the fair share as a filter
// on its candidates, as the current core's. The split is ReclaimPage's with
// D2: a root's page is dropped and read again (ReclaimRangeForEviction's
// case), and a page of a region's layer, Dirty, AwaitingClean or Clean, has
// its bytes written to the reservations of the bindings it is the state of
// (ReclaimPageForCompression's case) and is taken out of the layer. The
// reservation, the reference, stays in the binding beside the layer: the
// frame's bytes are the pager's to move, and the layer's node keeps no
// storage of its own. Revoking every alias before the slot goes back is
// RangeChangeUpdateLocked with UnmapAndHarvest over every region in the
// alias set.

// reclaimStep is Host.reclaimStep over the zircon core: one victim for req,
// in the current core's order, and its reclamation.
func (z *zirconHost) reclaimStep(ctx context.Context, req *evictionRequest) (zirconvm.ReclaimAttempt, bool, error) {
	h := z.host
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return zirconvm.ReclaimAttempt{}, false, err
	}
	if !req.preferEviction {
		h.mu.Unlock()
		if z.takeIdle() {
			return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: zirconvm.ReclaimEvict, NumPages: 1}},
				true, nil
		}
		h.mu.Lock()
		// The slots of a prefetch still reading come next: nothing waits on
		// its pages. See prefetch.go.
		if !sim.Bug(ctx, "pager-prefetch-ignores-pressure") && z.cancelPrefetchesLocked(ctx) {
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
	page := z.peekVictimLocked(req)
	req.changed = h.changed
	// Slots reserved by a concurrent load are in no queue yet.
	if z.queuedLocked() < h.cfg.ResidentPages {
		req.busy = true
	}
	h.mu.Unlock()
	if page == nil {
		return zirconvm.ReclaimAttempt{}, false, nil
	}
	req.preferEviction = false
	return z.reclaimVictim(ctx, page)
}

// queuedLocked is how many pages of the arena the queues hold: every page of
// every object, less the pages of a temporary root, which name another's
// frame. Caller holds h.mu.
func (z *zirconHost) queuedLocked() int {
	counts := z.node.PageQueues().QueueCounts()
	return counts.Total() - counts.Wired
}

// peekVictimLocked is Host.peekVictimLocked over the node's queues: the
// least recently faulted page that leaves every protected region its pages,
// then the least recently faulted of all, then a page a cold copy pins, each
// returned locked. Caller holds h.mu.
func (z *zirconHost) peekVictimLocked(req *evictionRequest) *zirconvm.VmPage {
	h := z.host
	share := h.cfg.ResidentPages / max(len(h.memoryRegions), 1)
	candidate := func(fair bool) func(*zirconvm.VmPage) bool {
		return func(p *zirconvm.VmPage) bool {
			if isLent(p) {
				return false
			}
			f := frameOf(p)
			if fair && !z.fairLocked(f, req.region, share) {
				return false
			}
			if !f.mu.TryLock() {
				req.busy = true
				return false
			}
			if z.usableVictimLocked(f) {
				return true
			}
			f.mu.Unlock()
			return false
		}
	}
	queues := z.node.PageQueues()
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

// fairLocked is Host.fairLocked over a frame's aliases. Caller holds h.mu.
func (z *zirconHost) fairLocked(f *zframe, r *MemoryRegion, share int) bool {
	for b := range f.aliases.all() {
		if q := b.region.region; q != r && z.host.protectedLocked(q, share) {
			return false
		}
	}
	return true
}

// usableVictimLocked is usableVictimLocked over a frame: no store is
// replacing it, and no terminal region maps it. Caller holds h.mu and the
// page's lock.
func (z *zirconHost) usableVictimLocked(f *zframe) bool {
	if f.replacing != 0 || f.slot < 0 {
		return false
	}
	for b := range f.aliases.all() {
		if b.region.region.terminal.Load() != nil {
			return false
		}
	}
	return true
}

// reclaimVictim reclaims a victim peekVictimLocked returned locked, and
// unlocks it, as Host.reclaimVictim does.
func (z *zirconHost) reclaimVictim(ctx context.Context, page *zirconvm.VmPage) (zirconvm.ReclaimAttempt, bool, error) {
	defer z.unlockPage(page)
	// A cold copy the guest did not change goes back to the page it was
	// copied from rather than to the spill: see cold.go.
	given, err := z.giveBackVictim(ctx, page)
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
	if err := z.evictPage(ctx, page); err != nil {
		if errors.Is(err, errVictimHeld) {
			return zirconvm.ReclaimAttempt{Failure: zirconvm.ReclaimOther, Page: page}, true, nil
		}
		return zirconvm.ReclaimAttempt{}, false, err
	}
	return zirconvm.ReclaimAttempt{Success: zirconvm.ReclaimSuccess{Type: kind, NumPages: 1}, Page: page}, true, nil
}

// aliasesOf is the bindings that map a frame, read under h.mu.
func (z *zirconHost) aliasesOf(f *zframe) []*zbinding {
	h := z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]*zbinding, 0, f.aliases.len())
	for b := range f.aliases.all() {
		result = append(result, b)
	}
	return result
}

// spillTarget is binding.spillTarget over the zircon core: the reservation
// b's bytes go to, and whether a page that names none has them held
// elsewhere: by the checkpoint's copy, or because the page is not b's
// region's own state at all.
func (z *zirconRegion) spillTarget(b *zbinding) (spill reservation, elsewhere bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return b.spill, !b.dirty || b.checkpoint != nil
}

// evictPage is Host.evictPage over the zircon core: it revokes every alias
// of a locked victim before reading its bytes, writes a region's own page to
// the reservations of the bindings it is the state of, takes it out of its
// object, and gives its slot back. The slot goes back only once the spill
// has succeeded.
func (z *zirconHost) evictPage(ctx context.Context, page *zirconvm.VmPage) error {
	h := z.host
	f := frameOf(page)
	// The reservations the page's bytes go to are read as the alias set
	// grows: a seal taken while this runs joins the checkpoint's copy to the
	// page before it hands that copy the page's reservation, so an alias set
	// that has not grown since its reservations were read names every one the
	// bytes can be in, and one read twice is written once.
	var spills []reservation
	taken := make(map[reservation]bool)
	byRegion := make(map[*zirconRegion][]*zbinding)
	walked := make(map[*zbinding]bool)
	for grown := true; grown; {
		grown = false
		aliases := z.aliasesOf(f)
		if evictionSeam != nil {
			evictionSeam(f.slot)
		}
		for _, b := range aliases {
			if walked[b] {
				continue
			}
			walked[b], grown = true, true
			byRegion[b.region] = append(byRegion[b.region], b)
			if b.region.region.Checkpoint() != nil {
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
	for q, bindings := range byRegion {
		if err := q.revokeBindings(ctx, bindings); err != nil {
			// A region that cannot take the mapping away is terminal from
			// here, and this page is excluded from every later step by it.
			q.region.heldPages(ctx, err)
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
	// Out of its object, so no lookup finds it; then nothing names it, and its
	// slot goes back.
	z.removeFromObject(page)
	h.mu.Lock()
	h.stats.Evictions++
	if f.aliases.len() > 0 {
		h.displaced++
	}
	z.dropAliasesLocked(f)
	h.mu.Unlock()
	z.releaseFrame(page)
	return nil
}

// removeFromObject takes a locked page out of whichever object holds it, and
// its name out of the temporary root that lends it.
func (z *zirconHost) removeFromObject(page *zirconvm.VmPage) {
	h := z.host
	if link, ok := z.node.PageQueues().Backlink(page); ok {
		lock := link.Cow.Lock()
		lock.Lock()
		link.Cow.RemovePageLocked(link.Offset, page)
		lock.Unlock()
	}
	h.mu.Lock()
	lent := frameOf(page).lent
	frameOf(page).lent = nil
	h.mu.Unlock()
	if lent != nil {
		if link, ok := z.node.PageQueues().Backlink(lent); ok {
			lock := link.Cow.Lock()
			lock.Lock()
			link.Cow.RemovePageLocked(link.Offset, lent)
			lock.Unlock()
		}
	}
}

// dropAliasesLocked takes every alias off a frame that is going, which an
// eviction does: each binding names no page from here. Caller holds h.mu.
func (z *zirconHost) dropAliasesLocked(f *zframe) {
	var bindings []*zbinding
	for b := range f.aliases.all() {
		bindings = append(bindings, b)
	}
	for _, b := range bindings {
		f.aliases.remove(b)
		if !f.mappedBy(b.region) {
			b.region.region.resident--
		}
		if b.page != nil && frameOf(b.page) == f {
			b.page = nil
		}
	}
}
