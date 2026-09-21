package vmmemory

import (
	"context"
	"errors"
	"sort"

	"github.com/semistrict/sproutfs/internal/control"
)

// windowPlan collects the pages of one page that a fault or Populate installs
// together: locked residents, slots reserved for loading, and their order.
type windowPlan struct {
	region     *Region
	start, end uint64
	fault      uint64 // the faulting page, or end for Populate
	extents    []control.Extent
	pages      []*resident // locked, indexed by page-start
	reserved   []int       // slot reserved for loading, -1 otherwise
	fresh      []bool      // page tables not yet installed
	zeros      []bool      // explicit zeros, requiring no resident or reservation
	// private marks pages loaded as this region's own dirty state, which a
	// migration destination's peer-served pages are. They are mapped writable,
	// because a dirty page the guest may store into without faulting is exactly
	// what their bindings say they are.
	private       []bool
	observedZeros bool
	locked        map[*resident]bool
	// spill names the dirty reservation the fault brought with it, or -1. A
	// page the backing serves out of another host's memory is this region's own
	// dirty state, so loading it takes a reservation, and the faulting page's
	// is taken by the waiting path before the fault holds any lock. It is
	// consumed by setting it to -1.
	spill *int
}

func (r *Region) plan(ctx context.Context, start, end, fault uint64) (*windowPlan, error) {
	ps := r.host.pageSize
	extents, err := r.backing.Locate(ctx, start*ps, (end-start)*ps)
	if err != nil {
		return nil, err
	}
	none := -1
	p := &windowPlan{region: r, start: start, end: end, fault: fault, extents: extents, pages: make([]*resident, end-start), reserved: make([]int, end-start), fresh: make([]bool, end-start), zeros: make([]bool, end-start), private: make([]bool, end-start), locked: make(map[*resident]bool), spill: &none}
	for i := range p.reserved {
		p.reserved[i] = -1
	}
	return p, nil
}

func (p *windowPlan) unlock() {
	h := p.region.host
	h.mu.Lock()
	for i, slot := range p.reserved {
		if slot >= 0 {
			// Publication takes ownership before touching the arena. These
			// reservations have never held contents or mappings.
			h.putFree(slot)
			p.reserved[i] = -1
		}
	}
	h.signal()
	h.mu.Unlock()
	pages := make([]*resident, 0, len(p.locked))
	for pg := range p.locked {
		pages = append(pages, pg)
	}
	h.unlockAll(pages)
}

// eligible reports whether a page other than the faulting one can join the
// plan: it must have no private state and must not already be resident.
func (p *windowPlan) eligible(page uint64) bool {
	b := p.region.lookupBinding(page)
	if b == nil {
		return true
	}
	if b.dirty {
		return false
	}
	h := p.region.host
	h.mu.Lock()
	resident := b.resident != nil
	h.mu.Unlock()
	return !resident
}

// identity reports the store page whose bytes this page reads, which is the
// whole of what names it: a page is published whole or not at all.
func (p *windowPlan) identity(page uint64) (pageKey, bool) {
	offset := page * p.region.host.pageSize
	first := sort.Search(len(p.extents), func(i int) bool { return p.extents[i].Offset+p.extents[i].Length > offset })
	if first >= len(p.extents) {
		return pageKey{}, false
	}
	e := p.extents[first]
	if e.Identity.Zero {
		return pageKey{id: control.Identity{Zero: true}}, true
	}
	// A page with no object is private to this region and never shared, and so
	// is one whose backing named a page other than this one.
	if e.Identity.Ref.IsZero() || e.Identity.Page != page {
		return pageKey{}, false
	}
	return pageKey{id: e.Identity}, true
}

// unpublished reports whether the window's extents say this page's bytes belong
// to no object of this volume, which for a backing that fetches from another
// host means the source still holds them: the load will take the page as this
// region's private dirty state and needs a dirty reservation for it. Only such
// a backing is asked; for every other one the answer is that the volume holds
// every page it reports.
func (p *windowPlan) unpublished(page uint64) bool {
	if !p.region.peer {
		return false
	}
	offset := page * p.region.host.pageSize
	first := sort.Search(len(p.extents), func(i int) bool { return p.extents[i].Offset+p.extents[i].Length > offset })
	if first >= len(p.extents) {
		return false
	}
	e := p.extents[first]
	return !e.Identity.Zero && e.Identity.Ref.IsZero()
}

// observeZeros records that this region knows about explicit zeros, which is
// what lets a sibling attachment map them eagerly without any metadata of its
// own. It is idempotent per plan.
func (p *windowPlan) observeZeros() {
	if p.observedZeros {
		return
	}
	h := p.region.host
	h.mu.Lock()
	if !p.region.hasZeros {
		p.region.hasZeros = true
		h.zeroRegions++
	}
	h.mu.Unlock()
	p.observedZeros = true
}

// markZeros records a whole explicit zero extent. A hole owns no arena slot and
// no identity, so it costs one bookkeeping step per extent and one map lookup
// per binding block rather than per page.
func (p *windowPlan) markZeros(first, last uint64) {
	if first >= last {
		return
	}
	p.observeZeros()
	r := p.region
	for page := first; page < last; {
		stop := min(last, (page/bindingBlockPages+1)*bindingBlockPages)
		if !r.touchedBlock(page) {
			for i := page; i < stop; i++ {
				p.zeros[i-p.start], p.fresh[i-p.start] = true, true
			}
			page = stop
			continue
		}
		for ; page < stop; page++ {
			if p.eligible(page) {
				p.zeros[page-p.start], p.fresh[page-p.start] = true, true
			}
		}
	}
}

// bindShared binds a page to a resident with the same stored identity if there
// is one. The faulting page waits for that resident's lock only when the plan
// holds no other; read-ahead pages skip a busy resident and are simply left for
// a later fault.
func (p *windowPlan) bindShared(ctx context.Context, page uint64, wait bool) error {
	id, ok := p.identity(page)
	if !ok {
		return nil
	}
	if id.zero() {
		p.observeZeros()
		p.zeros[page-p.start] = true
		p.fresh[page-p.start] = true
		return nil
	}
	h := p.region.host
	key := id
	for {
		h.mu.Lock()
		pg := h.clean[key]
		h.mu.Unlock()
		if pg == nil {
			return nil
		}
		if p.locked[pg] {
			// An imported identity may appear more than once in this plan.
		} else if wait {
			if err := pg.mu.Lock(ctx); err != nil {
				return err
			}
		} else if !pg.mu.TryLock() {
			return nil
		}
		h.mu.Lock()
		valid := h.clean[key] == pg
		h.mu.Unlock()
		if !valid {
			h.unlock(pg)
			continue
		}
		h.probe.stable(ctx, h, pg, "bindShared")
		h.bind(p.region.binding(page), pg)
		h.mu.Lock()
		h.stats.IdentityHits++
		h.mu.Unlock()
		p.pages[page-p.start] = pg
		p.locked[pg] = true
		p.fresh[page-p.start] = true
		return nil
	}
}

func (p *windowPlan) reserve(page uint64, slot int) {
	p.reserved[page-p.start] = slot
	p.fresh[page-p.start] = true
}

// needsLoad reports whether a page has neither a resident nor a reservation
// yet and is not expected to bind to a resident identity.
func (p *windowPlan) needsLoad(page uint64) bool {
	i := page - p.start
	if p.pages[i] != nil || p.reserved[i] >= 0 || !p.eligible(page) {
		return false
	}
	id, ok := p.identity(page)
	if id.zero() {
		return false
	}
	if !ok {
		return true
	}
	h := p.region.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clean[id] == nil
}

// reserveAround reserves free slots for the run of pages that need loading
// around the faulting page, so the run can become one mapping. When fewer
// slots are free than the run needs, the pages from the faulting one forward
// take them. Nothing is evicted; the page may remain unreserved.
func (p *windowPlan) reserveAround(index uint64) {
	first, last := index, index+1
	for first > p.start && p.needsLoad(first-1) {
		first--
	}
	for last < p.end && p.needsLoad(last) {
		last++
	}
	slot, count := p.region.host.allocateFree(int(last - first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	for k := range count {
		p.reserve(start+uint64(k), slot+k)
	}
}

// reserveRuns takes free slots, without evicting, for the eligible pages that
// still need loading. Runs of consecutive pages prefer consecutive slots so a
// later mapping installs them as one range. When free slots cannot cover the
// window, the pages after the faulting one come first: access tends to
// continue forward.
func (p *windowPlan) reserveRuns(from uint64) {
	h := p.region.host
	needs := p.needsLoad
	needed := 0
	for page := p.start; page < p.end; page++ {
		if needs(page) {
			needed++
		}
	}
	spans := [][2]uint64{{p.start, p.end}}
	h.mu.Lock()
	if h.slots.Free() < needed {
		spans = [][2]uint64{{from, p.end}, {p.start, from}}
	}
	h.mu.Unlock()
	for _, span := range spans {
		for page := span[0]; page < span[1]; {
			if !needs(page) {
				page++
				continue
			}
			run := uint64(1)
			for page+run < span[1] && needs(page+run) {
				run++
			}
			slot, count := h.allocateFree(int(run))
			for k := range count {
				p.reserve(page+uint64(k), slot+k)
			}
			if count == 0 {
				return
			}
			page += run
		}
	}
}

// loadReserved reads every run of reserved pages with one backing read and
// publishes the resulting residents under their stored identities.
func (p *windowPlan) loadReserved(ctx context.Context) error {
	h := p.region.host
	ps := int(h.pageSize)
	var buffer []byte
	for page := p.start; page < p.end; {
		i := page - p.start
		if p.reserved[i] < 0 {
			page++
			continue
		}
		run := uint64(1)
		for page+run < p.end && p.reserved[i+run] >= 0 {
			run++
		}
		if uint64(len(buffer)) < run*uint64(ps) {
			buffer = make([]byte, run*uint64(ps))
		}
		data := buffer[:run*uint64(ps)]
		unpublished, err := p.region.loadWindow(ctx, page*uint64(ps), data)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.stats.Loads++
		h.stats.LoadedPages += run
		h.mu.Unlock()
		// A backing whose pages come from another host is told which of them
		// this region went on to hold, so a page it served that the publish
		// below dropped stays one only that host has.
		var installed []bool
		if len(unpublished) > 0 {
			installed = make([]bool, run)
		}
		var failure error
		for k := range run {
			private := int(k) < len(unpublished) && unpublished[k]
			if failure = p.publish(ctx, page+k, data[int(k)*ps:int(k+1)*ps], private); failure != nil {
				break
			}
			if installed != nil {
				installed[k] = p.private[page+k-p.start]
			}
		}
		if installed != nil {
			p.region.installedUnpublished(page*uint64(ps), installed)
		}
		if failure != nil {
			return failure
		}
		page += run
	}
	return nil
}

// publish creates the resident for a loaded page. A concurrent load of the
// same identity may win; the duplicate slot is released and the winner used if
// its lock is free. It is never waited for: this plan already holds other
// resident locks, and a population acquires them in identity order, so waiting
// here could deadlock. The fault retries instead, holding nothing.
func (p *windowPlan) publish(ctx context.Context, page uint64, data []byte, private bool) error {
	h := p.region.host
	i := page - p.start
	slot := p.reserved[i]
	if private {
		// The bytes are the guest's own and no checkpoint has them, so this page
		// enters the region as dirty state: a private page under a dirty
		// reservation, which the next checkpoint publishes. The faulting page
		// brings the reservation the waiting path admitted it under, so a full
		// budget stalls the fault before it holds anything rather than failing
		// it; read-ahead takes only a reservation that is free now and leaves
		// the page for a later fault when none is.
		spill := -1
		if page == p.fault && *p.spill >= 0 {
			spill, *p.spill = *p.spill, -1
		} else {
			var err error
			if spill, err = h.tryTakeSpill(); err != nil {
				if page != p.fault && errors.Is(err, ErrCapacity) {
					p.fresh[i] = false
					return nil
				}
				if !errors.Is(err, ErrCapacity) {
					return err
				}
				// The extents did not say this page was the source's, so the
				// fault holds no reservation for it. It takes one and retries.
				return errUnpublishedReservation
			}
		}
		p.reserved[i] = -1
		pg, err := h.create(ctx, slot, data, pageKey{}, true, p.region.kind)
		if err != nil {
			h.releaseSpill(spill)
			return err
		}
		b := p.region.binding(page)
		b.zero = false
		b.spillSlot = spill
		p.region.setDirty(b, true)
		h.bind(b, pg)
		p.pages[i] = pg
		p.private[i] = true
		p.locked[pg] = true
		return nil
	}
	p.reserved[i] = -1
	id, shared := p.identity(page)
	key := pageKey{}
	if shared {
		key = id
	}
	pg, err := h.create(ctx, slot, data, key, false, p.region.kind)
	if err != nil {
		return err
	}
	if shared {
		h.mu.Lock()
		existing := h.clean[key]
		if existing == nil {
			h.clean[key] = pg
			h.cleanVersion++
		}
		h.mu.Unlock()
		if existing != nil {
			err := h.release(ctx, pg)
			h.unlock(pg)
			if err != nil {
				return err
			}
			p.fresh[i] = false
			return p.bindShared(ctx, page, false)
		}
	}
	h.bind(p.region.binding(page), pg)
	p.pages[i] = pg
	p.locked[pg] = true
	return nil
}

// install maps every planned page and populates its page tables. Consecutive
// slots and explicit zero ranges coalesce into runs, sent in bounded batches.
// It reports whether the faulting page ended resolved.
func (p *windowPlan) install(ctx context.Context) (bool, error) {
	r := p.region
	h := r.host
	var runs []MapRun
	var writable []MapRun
	for page := p.start; page < p.end; {
		i := page - p.start
		pg := p.pages[i]
		zero := p.zeros[i]
		if (pg == nil && !zero) || !p.fresh[i] {
			page++
			continue
		}
		if p.private[i] {
			// Private dirty state is mapped writable, in runs of its own: the
			// guest may store into it without faulting again, which is what its
			// binding already says.
			if r.mapped(page) {
				h.touch(pg)
				p.fresh[i] = false
				page++
				continue
			}
			run := uint64(1)
			for page+run < p.end && p.private[i+run] && p.fresh[i+run] && !r.mapped(page+run) &&
				p.pages[i+run] != nil && p.pages[i+run].slot == pg.slot+int(run) {
				run++
			}
			for k := range run {
				r.setMapped(r.binding(page+k), true)
				h.touch(p.pages[i+k])
				p.fresh[i+k] = false
			}
			writable = append(writable, MapRun{Page: page, Slot: pg.slot, Count: int(run)})
			h.mu.Lock()
			h.stats.MappedPages += run
			h.mu.Unlock()
			page += run
			continue
		}
		if r.mapped(page) {
			if pg != nil {
				h.touch(pg)
			}
			p.fresh[i] = false
			page++
			continue
		}
		slot := 0
		if pg != nil {
			slot = pg.slot
		}
		run := uint64(1)
		for page+run < p.end {
			next := p.pages[i+run]
			if !p.fresh[i+run] || r.mapped(page+run) || p.zeros[i+run] != zero || p.private[i+run] ||
				(!zero && (next == nil || next.slot != slot+int(run))) {
				break
			}
			run++
		}
		if zero {
			r.mapZeros(page, page+run) // retain possible zero mappings on an ambiguous ACK
		} else {
			for k := range run {
				r.setMapped(r.binding(page+k), true)
				if pg := p.pages[i+k]; pg != nil {
					h.touch(pg)
				}
			}
		}
		runs = append(runs, MapRun{Page: page, Slot: slot, Count: int(run), Zero: zero})
		h.mu.Lock()
		h.stats.MappedPages += run
		h.mu.Unlock()
		page += run
	}
	if len(runs) > 0 {
		commands := len(runs)
		mappingRuns := len(runs)
		if batch, ok := r.mapping.(BatchMapping); ok {
			var err error
			commands, mappingRuns, err = r.mapBatch(ctx, batch, runs)
			if err != nil {
				return false, r.mappingFailed(err, func() { r.unmapRuns(runs) })
			}
		} else {
			for i, run := range runs {
				var err error
				if run.Zero {
					err = r.mapZeroPages(ctx, run.Page, run.Count)
				} else {
					err = r.mapPages(ctx, run.Page, run.Slot, run.Count, false)
				}
				if err != nil {
					// The runs before this one are commands that landed.
					return false, r.mappingFailed(err, func() { r.unmapRuns(runs[i:]) })
				}
			}
		}
		h.mu.Lock()
		h.stats.Mappings += uint64(commands)
		h.stats.MappingRuns += uint64(mappingRuns)
		h.mu.Unlock()
	}
	for _, run := range writable {
		if err := r.mapPages(ctx, run.Page, run.Slot, run.Count, true); err != nil {
			return false, r.mappingFailed(err, func() { r.unmapRuns([]MapRun{run}) })
		}
		h.mu.Lock()
		h.stats.Mappings++
		h.stats.MappingRuns++
		h.mu.Unlock()
	}
	resolved := false
	for _, run := range runs {
		if err := r.resolvePages(ctx, run.Page, run.Count, false); err != nil {
			return false, r.fail(err)
		}
		resolved = resolved || (p.fault >= run.Page && p.fault < run.Page+uint64(run.Count))
	}
	for _, run := range writable {
		if err := r.resolvePages(ctx, run.Page, run.Count, true); err != nil {
			return false, r.fail(err)
		}
		resolved = resolved || (p.fault >= run.Page && p.fault < run.Page+uint64(run.Count))
	}
	if resolved || p.fault < p.start || p.fault >= p.end {
		return resolved, nil
	}
	// The faulting page was already mapped by an earlier attempt; its trapped
	// access still has to be completed.
	if !r.mapped(p.fault) {
		return false, nil
	}
	if err := r.resolvePages(ctx, p.fault, 1, p.private[p.fault-p.start]); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}
