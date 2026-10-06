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

// queuedLocked is how many pages of the arena the queues hold: every page of
// every object, less the pages of a temporary root, which name another's
// frame. Caller holds h.mu.
func (h *Host) queuedLocked() int {
	counts := h.node.PageQueues().QueueCounts()
	return counts.Total() - counts.Wired
}

// peekVictimLocked is Host.peekVictimLocked over the node's queues: the
// least recently faulted page that leaves every protected region its pages,
// then the least recently faulted of all, then a page a cold copy pins, each
// returned locked. Caller holds h.mu.
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

// fairLocked is Host.fairLocked over a frame's aliases. Caller holds h.mu.
func (h *Host) fairLocked(f *zframe, r *MemoryRegion, share int) bool {
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
func (h *Host) usableVictimLocked(f *zframe) bool {
	if f.replacing != 0 || f.slot < 0 {
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
func (h *Host) aliasesOf(f *zframe) []*zbinding {
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
func (r *MemoryRegion) spillTarget(b *zbinding) (spill reservation, elsewhere bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.spill, !b.dirty || b.checkpoint != nil
}

// evictPage is Host.evictPage over the zircon core: it revokes every alias
// of a locked victim before reading its bytes, writes a region's own page to
// the reservations of the bindings it is the state of, takes it out of its
// object, and gives its slot back. The slot goes back only once the spill
// has succeeded.
func (h *Host) evictPage(ctx context.Context, page *zirconvm.VmPage) error {
	f := frameOf(page)
	// The reservations the page's bytes go to are read as the alias set
	// grows: a seal taken while this runs joins the checkpoint's copy to the
	// page before it hands that copy the page's reservation, so an alias set
	// that has not grown since its reservations were read names every one the
	// bytes can be in, and one read twice is written once.
	var spills []reservation
	taken := make(map[reservation]bool)
	byRegion := make(map[*MemoryRegion][]*zbinding)
	walked := make(map[*zbinding]bool)
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
	for q, bindings := range byRegion {
		if err := q.revokeBindings(ctx, bindings); err != nil {
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
	// Out of its object, so no lookup finds it; then nothing names it, and its
	// slot goes back.
	h.removeFromObject(page)
	h.mu.Lock()
	h.stats.Evictions++
	if f.aliases.len() > 0 {
		h.displaced++
	}
	h.dropAliasesLocked(f)
	h.mu.Unlock()
	h.releaseFrame(page)
	return nil
}

// removeFromObject takes a locked page out of whichever object holds it, and
// its name out of the temporary root that lends it.
func (h *Host) removeFromObject(page *zirconvm.VmPage) {
	if link, ok := h.node.PageQueues().Backlink(page); ok {
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
		if link, ok := h.node.PageQueues().Backlink(lent); ok {
			lock := link.Cow.Lock()
			lock.Lock()
			link.Cow.RemovePageLocked(link.Offset, lent)
			lock.Unlock()
		}
	}
}

// dropAliasesLocked takes every alias off a frame that is going, which an
// eviction does: each binding names no page from here. Caller holds h.mu.
func (h *Host) dropAliasesLocked(f *zframe) {
	var bindings []*zbinding
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
