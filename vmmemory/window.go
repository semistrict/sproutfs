package vmmemory

import (
	"context"
	"errors"
	"sort"

	"github.com/semistrict/sproutfs/control"
)

// windowPlan collects the pages of one page that a fault or Populate installs
// together: locked residents, slots reserved for loading, and their order.
type windowPlan struct {
	memoryRegion *MemoryRegion
	start, end   uint64
	fault        uint64 // the faulting page, or end for Populate
	// store names the page a store faulted on, or end. The plan reads that page
	// in and holds it locked like any other, but binds nothing to it and
	// installs no page tables for it: what this memory region will hold there is the
	// private copy the store is about to make, and a binding that took the
	// shared page first would be a second owner of that page's memory for as
	// long as the copy takes and would cost the store a revocation to undo.
	store   uint64
	extents []control.Extent
	pages   []*resident // locked, indexed by page-start
	// file is the file this window's loads go in, and reserved the slot of it
	// each page's load has, or -1.
	file     *arenaFile
	reserved []int
	fresh    []bool // page tables not yet installed
	zeros    []bool // explicit zeros, requiring no resident or reservation
	// private marks pages loaded as this memory region's own dirty state, which a
	// migration destination's peer-served pages are. They are mapped writable,
	// because a dirty page the guest may store into without faulting is exactly
	// what their bindings say they are.
	private       []bool
	observedZeros bool
	locked        map[*resident]bool
	// spill names the dirty reservation the fault brought with it, or -1. A
	// page the backing serves out of another host's memory is this memory region's own
	// dirty state, so loading it takes a reservation, and the faulting page's
	// is taken by the waiting path before the fault holds any lock. It is
	// consumed by setting it to -1.
	spill *int
	// installed is what this plan's own mapping commands came to, beside the
	// pager-wide counters they also advance: a populate adds it up over its
	// windows to say what one attach cost before its guest ran.
	installed installedRuns
}

// installedRuns is a set of mapping commands as what they cost: the commands
// themselves, the runs they covered and the pages in those runs.
type installedRuns struct{ commands, runs, pages uint64 }

func (i *installedRuns) add(other installedRuns) {
	i.commands += other.commands
	i.runs += other.runs
	i.pages += other.pages
}

func (r *MemoryRegion) plan(ctx context.Context, start, end, fault uint64) (*windowPlan, error) {
	ps := r.host.pageSize
	extents, err := r.backing.Locate(ctx, start*ps, (end-start)*ps)
	if err != nil {
		return nil, err
	}
	none := -1
	p := &windowPlan{memoryRegion: r, start: start, end: end, fault: fault, store: end, extents: extents, pages: make([]*resident, end-start), file: r.sharedFile(), reserved: make([]int, end-start), fresh: make([]bool, end-start), zeros: make([]bool, end-start), private: make([]bool, end-start), locked: make(map[*resident]bool), spill: &none}
	for i := range p.reserved {
		p.reserved[i] = -1
	}
	return p, nil
}

func (p *windowPlan) unlock() {
	h := p.memoryRegion.host
	h.mu.Lock()
	for i, slot := range p.reserved {
		if slot >= 0 {
			// Publication takes ownership before touching the arena. These
			// reservations have never held contents or mappings.
			h.putFree(fileSlot{p.file, slot})
			p.reserved[i] = -1
		}
	}
	pages := make([]*resident, 0, len(p.locked))
	for pg := range p.locked {
		pages = append(pages, pg)
		// A published page the plan loaded and a failure left unmapped is idle,
		// like any other published page nothing maps. Off the idle list, only
		// a reclaim would ever give its memory back.
		if pg.published() {
			h.idleLocked(pg)
		}
	}
	h.signal()
	h.mu.Unlock()
	h.unlockAll(pages)
}

// eligible reports whether a page other than the faulting one can join the
// plan: it must have no private state and must not already be resident.
func (p *windowPlan) eligible(page uint64) bool {
	b := p.memoryRegion.lookupBinding(page)
	if b == nil {
		return true
	}
	if b.dirty {
		return false
	}
	h := p.memoryRegion.host
	h.mu.Lock()
	resident := b.resident != nil
	h.mu.Unlock()
	return !resident
}

// identity reports the store page whose bytes this page reads, which is the
// whole of what names it: a page is published whole or not at all.
func (p *windowPlan) identity(page uint64) (pageKey, bool) {
	offset := page * p.memoryRegion.host.pageSize
	first := sort.Search(len(p.extents), func(i int) bool { return p.extents[i].Offset+p.extents[i].Length > offset })
	if first >= len(p.extents) {
		return pageKey{}, false
	}
	e := p.extents[first]
	if e.Identity.Zero {
		return pageKey{id: control.Identity{Zero: true}}, true
	}
	// A page with no object is private to this memory region and never shared, and so
	// is one whose backing named a page other than this one.
	if e.Identity.Ref.IsZero() || e.Identity.Page != page {
		return pageKey{}, false
	}
	return pageKey{id: e.Identity}, true
}

// unpublished reports whether the window's extents say this page's bytes belong
// to no object of this volume, which for a backing that fetches from another
// host means the source still holds them: the load will take the page as this
// memory region's private dirty state and needs a dirty reservation for it. Only such
// a backing is asked; for every other one the answer is that the volume holds
// every page it reports.
func (p *windowPlan) unpublished(page uint64) bool {
	if !p.memoryRegion.peer {
		return false
	}
	offset := page * p.memoryRegion.host.pageSize
	first := sort.Search(len(p.extents), func(i int) bool { return p.extents[i].Offset+p.extents[i].Length > offset })
	if first >= len(p.extents) {
		return false
	}
	e := p.extents[first]
	return !e.Identity.Zero && e.Identity.Ref.IsZero()
}

// observeZeros records that this memory region knows about explicit zeros, which is
// what lets a sibling attachment map them eagerly without any metadata of its
// own. It is idempotent per plan.
func (p *windowPlan) observeZeros() {
	if p.observedZeros {
		return
	}
	h := p.memoryRegion.host
	h.mu.Lock()
	if !p.memoryRegion.hasZeros {
		p.memoryRegion.hasZeros = true
		h.zeroMemoryRegions++
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
	r := p.memoryRegion
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
	h := p.memoryRegion.host
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
		if found := h.probe.stable(ctx, h, pg, "bindShared"); found != "" {
			panic(found)
		}
		if page != p.store {
			h.bind(p.memoryRegion.binding(page), pg)
		}
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
	h := p.memoryRegion.host
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
	at, count := p.memoryRegion.host.allocateFree(p.file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	for k := range count {
		p.reserve(start+uint64(k), at.slot+k)
	}
}

// reserveRuns takes free slots, without evicting, for the eligible pages that
// still need loading. Runs of consecutive pages prefer consecutive slots so a
// later mapping installs them as one range. Idle pages are given up first to
// make those slots free, which is not an eviction: nothing maps them. When free
// slots cannot cover the window even so, the pages after the faulting one come
// first: access tends to continue forward.
func (p *windowPlan) reserveRuns(ctx context.Context, from uint64) error {
	h := p.memoryRegion.host
	needs := p.needsLoad
	needed := 0
	for page := p.start; page < p.end; page++ {
		if needs(page) {
			needed++
		}
	}
	if err := h.makeRoom(ctx, p.file, needed); err != nil {
		return err
	}
	spans := [][2]uint64{{p.start, p.end}}
	h.mu.Lock()
	if p.file.slots.Free() < needed {
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
			at, count := h.allocateFree(p.file, int(run))
			for k := range count {
				p.reserve(page+uint64(k), at.slot+k)
			}
			if count == 0 {
				return nil
			}
			page += run
		}
	}
	return nil
}

// loadReserved reads the reserved pages of this window with one backing read
// and publishes the resulting residents under their stored identities. The
// pages between them — the ones this memory region already holds, and the holes its
// volume has — are left out of the read rather than splitting it: a window is
// one run of a volume, and what a run costs is the volume's to decide.
func (p *windowPlan) loadReserved(ctx context.Context) error {
	h := p.memoryRegion.host
	ps := h.pageSize
	first, last := p.end, p.start
	loading := uint64(0)
	for page := p.start; page < p.end; page++ {
		if p.reserved[page-p.start] < 0 {
			continue
		}
		first, last = min(first, page), page+1
		loading++
	}
	if loading == 0 {
		return nil
	}
	wanted := make([]bool, last-first)
	for page := first; page < last; page++ {
		wanted[page-first] = p.reserved[page-p.start] >= 0
	}
	buffer := h.takeWindow(last - first)
	defer h.putWindow(buffer)
	data := *buffer
	unpublished, err := p.memoryRegion.loadRun(ctx, first, wanted, data)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.stats.Loads++
	h.stats.LoadedPages += loading
	h.mu.Unlock()
	// A backing whose pages come from another host is told which of them this
	// memory region went on to hold, so a page it served that the publish below
	// dropped stays one only that host has.
	var installed []bool
	if len(unpublished) > 0 {
		installed = make([]bool, last-first)
	}
	var failure error
	for page := first; page < last; page++ {
		at := page - first
		if !wanted[at] {
			continue
		}
		private := at < uint64(len(unpublished)) && unpublished[at]
		if failure = p.publish(ctx, page, data[at*ps:(at+1)*ps], private); failure != nil {
			break
		}
		if installed != nil {
			installed[at] = p.private[page-p.start]
		}
	}
	if installed != nil {
		p.memoryRegion.installedUnpublished(first*ps, installed)
	}
	return failure
}

// publish creates the resident for a loaded page. A concurrent load of the
// same identity may win; the duplicate slot is released and the winner used if
// its lock is free. It is never waited for: this plan already holds other
// resident locks, and a population acquires them in identity order, so waiting
// here could deadlock. The fault retries instead, holding nothing.
func (p *windowPlan) publish(ctx context.Context, page uint64, data []byte, private bool) error {
	h := p.memoryRegion.host
	i := page - p.start
	slot := p.reserved[i]
	if private {
		// The bytes are the guest's own and no checkpoint has them, so this page
		// enters the memory region as dirty state: a private page under a dirty
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
		pg, err := h.create(ctx, fileSlot{p.file, slot}, data, pageKey{}, true, p.memoryRegion.kind)
		if err != nil {
			h.releaseSpill(spill)
			return err
		}
		b := p.memoryRegion.binding(page)
		b.zero = false
		b.spillSlot = spill
		p.memoryRegion.setDirty(b, true)
		h.bind(b, pg)
		h.probe.granted(b, pg, nil)
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
	pg, err := h.create(ctx, fileSlot{p.file, slot}, data, key, false, p.memoryRegion.kind)
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
	if page != p.store {
		h.bind(p.memoryRegion.binding(page), pg)
	}
	p.pages[i] = pg
	p.locked[pg] = true
	return nil
}

// pagesOf is how many pages a set of runs covers.
func pagesOf(runs []MapRun) uint64 {
	pages := uint64(0)
	for _, run := range runs {
		pages += uint64(run.Count)
	}
	return pages
}

// install maps every planned page and populates its page tables. Consecutive
// slots and explicit zero ranges coalesce into runs, sent in bounded batches.
// It reports whether the faulting page ended resolved.
func (p *windowPlan) install(ctx context.Context) (bool, error) {
	r := p.memoryRegion
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
				p.pages[i+run] != nil && p.pages[i+run].fileSlot == pg.plus(int(run)) {
				run++
			}
			for k := range run {
				r.setMapped(r.binding(page+k), true)
				h.touch(p.pages[i+k])
				p.fresh[i+k] = false
			}
			writable = append(writable, runAt(page, pg.fileSlot, int(run)))
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
		var at fileSlot
		if pg != nil {
			at = pg.fileSlot
		}
		run := uint64(1)
		for page+run < p.end {
			next := p.pages[i+run]
			if !p.fresh[i+run] || r.mapped(page+run) || p.zeros[i+run] != zero || p.private[i+run] ||
				(!zero && (next == nil || next.fileSlot != at.plus(int(run)))) {
				break
			}
			run++
		}
		if zero {
			r.mapZeros(page, page+run) // retain possible zero mappings on an ambiguous ACK
			runs = append(runs, MapRun{Page: page, Count: int(run), Zero: true})
		} else {
			for k := range run {
				r.setMapped(r.binding(page+k), true)
				if pg := p.pages[i+k]; pg != nil {
					h.touch(pg)
				}
			}
			runs = append(runs, runAt(page, at, int(run)))
		}
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
					err = r.mapPages(ctx, run, false)
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
		p.installed.add(installedRuns{commands: uint64(commands),
			runs: uint64(mappingRuns), pages: pagesOf(runs)})
	}
	for _, run := range writable {
		if err := r.mapPages(ctx, run, true); err != nil {
			return false, r.mappingFailed(err, func() { r.unmapRuns([]MapRun{run}) })
		}
		h.mu.Lock()
		h.stats.Mappings++
		h.stats.MappingRuns++
		h.mu.Unlock()
		p.installed.add(installedRuns{commands: 1, runs: 1, pages: uint64(run.Count)})
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
