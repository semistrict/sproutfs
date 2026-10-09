package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The evictor harvests pages before it takes them. A guest reads and writes a
// page through the mapping a fault installed, and the pager sees none of it:
// it cannot read the VMM's accessed bits (decision 4 of the Zircon port), so
// fault order alone would rank a page the guest reads all the time through
// its mapping, as a DAX root's reads are, below every page written once since
// it last faulted.
//
// So each reclaim step first harvests a few of the oldest isolated pages: it
// takes their mappings away and keeps their bytes, and moves them to the
// harvested isolate queue, which the evictor takes from before the pages it
// has not harvested (PeekUnharvestedWhere). The guest's next touch of a
// harvested page faults, the fault maps it again from its frame with no read,
// and marks it accessed, which moves it out of the isolate queues. A
// harvested page the evictor reaches still isolated was not touched in the
// time the harvested pages ahead of it took to evict.
//
// That time is the lead: the evictor keeps harvestLead pages harvested ahead
// of its victims, so the guest has that many evictions to touch a page again.
// A page it touches costs one fault each time it is harvested; a page it does
// not is revoked earlier than it would have been, and not twice.

// harvestBatch is the most pages one reclaim step harvests, which bounds what
// an allocation short of a slot spends on pages it does not take.
const harvestBatch = 64

// harvestLead is how many harvested pages the evictor keeps ahead of the
// pages it takes: a quarter of the arena, and at least two, so that a victim
// was harvested a step before the step that takes it.
func (h *Host) harvestLead() int { return max(h.cfg.ResidentPages/4, 2) }

// harvest tops the harvested isolate queue up toward the lead, harvesting at
// most harvestBatch of the oldest isolated pages. A page is harvested under
// its lock, as an eviction holds its victim: every mapping of it is taken
// away, and it stays in its object with its bytes. A page whose lock is held,
// or that an eviction could not take, is left for a later step.
func (h *Host) harvest(ctx context.Context) error {
	queues := h.node.PageQueues()
	want := min(h.harvestLead()-queues.HarvestedCount(), harvestBatch)
	if want <= 0 {
		return nil
	}
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return err
	}
	found := queues.PeekUnharvestedWhere(want, func(p *zirconvm.VmPage) bool {
		if isLent(p) {
			return false
		}
		f := frameOf(p)
		if !f.mu.TryLock() {
			return false
		}
		if h.usableVictimLocked(f) {
			return true
		}
		f.mu.Unlock()
		return false
	})
	h.mu.Unlock()
	pages := make([]*zirconvm.VmPage, len(found))
	for i, link := range found {
		pages[i] = link.Page
	}
	_, err := h.harvestPages(ctx, pages)
	return err
}

// harvestPages harvests pages, each locked by the caller, and unlocks them:
// every mapping of each is taken away, and each still standard isolated moves
// to the harvested queue. It reports how many pages it took every mapping of.
func (h *Host) harvestPages(ctx context.Context, locked []*zirconvm.VmPage) (int, error) {
	if len(locked) == 0 {
		return 0, nil
	}
	queues := h.node.PageQueues()
	pages := make([]*zirconvm.VmPage, 0, len(locked))
	byRegion := make(map[*MemoryRegion][]*binding)
	// A page of a region's own layer is held live as an eviction holds it,
	// so that a detach, which gives the region's pages back itself, does not
	// run under the revocation. One that cannot be held is detaching.
	var live []*MemoryRegion
	for _, page := range locked {
		f := frameOf(page)
		if owner := f.layer; owner != nil {
			if !owner.live.TryRLock() {
				h.unlockPage(page)
				continue
			}
			live = append(live, owner)
		}
		pages = append(pages, page)
		for _, b := range h.aliasesOf(f) {
			byRegion[b.region] = append(byRegion[b.region], b)
		}
	}
	defer func() {
		for _, owner := range live {
			owner.live.RUnlock()
		}
	}()
	held := make(map[*MemoryRegion]bool)
	if !sim.Bug(ctx, "pager-harvest-without-revoking") {
		for _, q := range inAttachOrder(byRegion) {
			if err := q.revokeBindings(ctx, byRegion[q]); err != nil {
				// A region that cannot take the mapping away is terminal from
				// here, and the pages it maps are no victims: they stay where
				// they are.
				q.heldPages(ctx, err)
				held[q] = true
			}
		}
	}
	// In a controlled run another task may go on here, with the pages' mappings
	// gone and the pages still standard isolated: a fault on one, which finds
	// it held and waits, and maps it again once it is harvested.
	if err := sim.Admit(ctx, "vmmemory/harvest"); err != nil {
		for _, p := range pages {
			h.unlockPage(p)
		}
		return 0, err
	}
	revoked, harvested := 0, 0
	for _, p := range pages {
		if !h.heldByAny(frameOf(p), held) {
			revoked++
			if queues.MoveToHarvested(p) {
				harvested++
			}
		}
		h.unlockPage(p)
	}
	h.mu.Lock()
	h.stats.HarvestedPages += uint64(harvested)
	h.mu.Unlock()
	return revoked, nil
}

// heldByAny reports whether one of the regions in held maps f.
func (h *Host) heldByAny(f *frame, held map[*MemoryRegion]bool) bool {
	if len(held) == 0 {
		return false
	}
	for _, b := range h.aliasesOf(f) {
		if held[b.region] {
			return true
		}
	}
	return false
}

// harvestOwn harvests every page r maps whose lock is free, which is how a
// session answers a client that refused one of r's mappings for want of
// budget: each mapping it takes away is budget the client gets back, and the
// guest's next touch of a page maps it again from its frame, with no read. A
// page whose lock is held is in use and left alone, and so is a zero, which
// holds no page. Revoking every mapping at once leaves the client one mapping
// for the region however its pages were scattered, which no choice of fewer
// pages can promise, and a refusal is rare enough for the refaults to be the
// cheaper side. It reports how many pages it took the mappings of.
func (r *MemoryRegion) harvestOwn(ctx context.Context) (int, error) {
	h := r.host
	var locked []*zirconvm.VmPage
	taken := make(map[*frame]bool)
	h.mu.Lock()
	r.bindingsMu.Lock()
	r.eachBoundLocked(0, uint64(r.pageCount), func(b *binding) {
		if !b.mapped || b.page == nil || isLent(b.page) {
			return
		}
		// A page's lock comes before Host.mu, so it is only tried here.
		f := frameOf(b.page)
		if taken[f] || !f.mu.TryLock() {
			return
		}
		if !h.usableVictimLocked(f) {
			f.mu.Unlock()
			return
		}
		taken[f] = true
		locked = append(locked, b.page)
	})
	r.bindingsMu.Unlock()
	h.mu.Unlock()
	return h.harvestPages(ctx, locked)
}
