package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
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
	store uint64
	// window locates the plan's pages once located says it is located. Until
	// then only the faulting page is, by alone: a fault that reads its page
	// first locates that page alone, so that its read starts before the rest
	// of its window is planned (faultfirst.go). The faulting page keeps the
	// identity it was planned under once the window is located too. alone
	// locates nothing in a plan located whole.
	window  locations
	located bool
	alone   locations
	// reading is how the fault this plan is for reads its window
	// (planFault).
	reading reading
	// provisional is the run of free slots a fault that prefetches took
	// around its own page before the window was located.
	provisional provisionalRun
	pages       []*resident // locked, indexed by page-start
	// file is the file this window's loads by identity go in, except a public
	// page's (see fileOf), and reserved the slot each page's load has, of that
	// file, of the public file or of this memory region's own, or slot -1.
	file     *arenaFile
	reserved []fileSlot
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

// plan is a plan of the window [start, end) located whole, for the faulting
// page fault, or end for none.
func (r *MemoryRegion) plan(ctx context.Context, start, end, fault uint64) (*windowPlan, error) {
	window, err := r.locate(ctx, start, end)
	if err != nil {
		return nil, err
	}
	p := r.newPlan(start, end, fault)
	p.window, p.located = window, true
	return p, nil
}

// planPage is a plan of the window [start, end) that has located page alone,
// for the faulting page fault, or end for none. locateWindow locates the rest.
func (r *MemoryRegion) planPage(ctx context.Context, start, end, fault, page uint64) (*windowPlan, error) {
	alone, err := r.locate(ctx, page, page+1)
	if err != nil {
		return nil, err
	}
	p := r.newPlan(start, end, fault)
	p.alone = alone
	return p, nil
}

func (r *MemoryRegion) newPlan(start, end, fault uint64) *windowPlan {
	none := -1
	p := &windowPlan{memoryRegion: r, start: start, end: end, fault: fault, store: end,
		pages: make([]*resident, end-start), file: r.sharedFile(), reserved: make([]fileSlot, end-start),
		fresh: make([]bool, end-start), zeros: make([]bool, end-start), private: make([]bool, end-start),
		locked: make(map[*resident]bool), spill: &none}
	for i := range p.reserved {
		p.reserved[i] = fileSlot{slot: -1}
	}
	return p
}

// locate reports the identities of the pages [first, last), each of which
// this memory region may read: one lookup of the backing for the run. It is
// the planning a simulation prices (WorkPlan), by the pages it locates.
func (r *MemoryRegion) locate(ctx context.Context, first, last uint64) (locations, error) {
	if err := sim.Work(ctx, WorkPlan, int(last-first)); err != nil {
		return locations{}, err
	}
	ps := r.host.pageSize
	extents, err := r.backing.Locate(ctx, first*ps, (last-first)*ps)
	if err != nil {
		return locations{}, err
	}
	// The pages of a run are mostly of a few checkpoints, one after another,
	// so each is checked once where its pages begin.
	var checked control.Ref
	for _, e := range extents {
		if e.Identity.Ref == checked {
			continue
		}
		if err := r.mayRead(e.Identity.Ref); err != nil {
			return locations{}, err
		}
		checked = e.Identity.Ref
	}
	return locations{first: first, pages: last - first, pageSize: ps, extents: extents}, nil
}

// locateWindow locates the whole window of a plan that has located only its
// faulting page, in one lookup. The page keeps the identity it was planned
// under.
func (p *windowPlan) locateWindow(ctx context.Context) error {
	if p.located {
		return nil
	}
	window, err := p.memoryRegion.locate(ctx, p.start, p.end)
	if err != nil {
		return err
	}
	p.window, p.located = window, true
	return nil
}

// extentOf is the extent a page's identity is read from, and false where none
// holds the page: the faulting page's own where the plan located it alone,
// and the window's otherwise, which must be located.
func (p *windowPlan) extentOf(page uint64) (control.Extent, bool) {
	if p.alone.holds(page) {
		return p.alone.extent(page)
	}
	if !p.located {
		panic(fmt.Sprintf("vmmemory: a plan asked about page %d of a window it has located only page %d of",
			page, p.alone.first))
	}
	return p.window.extent(page)
}

// locations are the identities of the pages [first, first+pages), pages of
// pageSize, as a backing located them: its extents, sorted, adjacent and each
// within one page or a hole of any length, and, from the first time a page is
// asked about, which extent holds each page. What names a page is then an
// index rather than a search of the extents, however often planning asks.
type locations struct {
	first, pages, pageSize uint64
	extents                []control.Extent
	held                   []uint32
}

// holds reports whether page is one these locations locate.
func (l *locations) holds(page uint64) bool { return page >= l.first && page-l.first < l.pages }

// extent is the extent that holds page, which these locations locate.
func (l *locations) extent(page uint64) (control.Extent, bool) {
	if l.held == nil {
		l.held = make([]uint32, l.pages)
		at := 0
		for k := range l.held {
			offset := (l.first + uint64(k)) * l.pageSize
			for at < len(l.extents) && l.extents[at].Offset+l.extents[at].Length <= offset {
				at++
			}
			l.held[k] = uint32(at)
		}
	}
	at := l.held[page-l.first]
	if int(at) >= len(l.extents) {
		return control.Extent{}, false
	}
	return l.extents[at], true
}

func (p *windowPlan) unlock() {
	h := p.memoryRegion.host
	h.mu.Lock()
	for i, at := range p.reserved {
		if at.slot >= 0 {
			// Publication takes ownership before touching the arena. These
			// reservations have never held contents or mappings.
			h.putFree(at)
			p.reserved[i] = fileSlot{slot: -1}
		}
	}
	// A fault that failed before it located its window holds the rest of
	// the run it took for it, which has never held anything either.
	for _, at := range p.provisional.slots {
		h.putFree(at)
	}
	p.provisional = provisionalRun{}
	pages := make([]*resident, 0, len(p.locked))
	for pg := range p.locked {
		pages = append(pages, pg)
		// A published page the plan loaded and a failure left unmapped is idle,
		// like any other published page nothing maps. Out of the don't-need queue, only
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
	h := p.memoryRegion.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return eligibleLocked(b)
}

// eligibleLocked is eligible of a page's binding, nil for a page that has
// none. Caller holds h.mu.
func eligibleLocked(b *binding) bool {
	return b == nil || !b.dirty && b.resident == nil
}

// identity reports the store page whose bytes this page reads, which is the
// whole of what names it: a page is published whole or not at all.
func (p *windowPlan) identity(page uint64) (pageKey, bool) {
	e, found := p.extentOf(page)
	if !found {
		return pageKey{}, false
	}
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
	e, found := p.extentOf(page)
	return found && !e.Identity.Zero && e.Identity.Ref.IsZero()
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
// no identity, so it costs one bookkeeping step per extent and one walk of the
// page list over it rather than a lookup per page: only a page with a binding
// can be one that may not join.
func (p *windowPlan) markZeros(first, last uint64) {
	if first >= last {
		return
	}
	p.observeZeros()
	// The pages that may not join, in order.
	var held []uint64
	if bound := p.memoryRegion.boundIn(first, last); len(bound) > 0 {
		h := p.memoryRegion.host
		h.mu.Lock()
		for _, b := range bound {
			if !eligibleLocked(b) {
				held = append(held, b.index)
			}
		}
		h.mu.Unlock()
	}
	for page := first; page < last; page++ {
		if len(held) > 0 && held[0] == page {
			held = held[1:]
			continue
		}
		p.zeros[page-p.start], p.fresh[page-p.start] = true, true
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
	for {
		h.mu.Lock()
		pg := h.clean[id]
		h.mu.Unlock()
		if pg == nil {
			return nil
		}
		if again, err := p.bindResident(ctx, page, id, pg, wait); err != nil || !again {
			return err
		}
	}
}

// bindResident binds a page to pg, the resident page its identity key named
// when the caller looked, as bindShared does. It reports again, having taken
// nothing, where pg is no longer that identity's resident page once its lock is
// held, so the caller looks again. A busy pg is left alone unless wait says to
// wait for it, and so is one this memory region cannot reach.
func (p *windowPlan) bindResident(ctx context.Context, page uint64, key pageKey, pg *resident,
	wait bool) (again bool, err error) {
	h := p.memoryRegion.host
	if p.locked[pg] {
		// An imported identity may appear more than once in this plan.
	} else if wait {
		if err := pg.mu.Lock(ctx); err != nil {
			return false, err
		}
	} else if !pg.mu.TryLock() {
		return false, nil
	}
	h.mu.Lock()
	valid := h.clean[key] == pg
	h.mu.Unlock()
	if !valid {
		h.unlock(pg)
		return true, nil
	}
	if found := h.probe.stable(ctx, h, pg, "bindShared"); found != "" {
		panic(found)
	}
	reached, err := p.memoryRegion.reach(ctx, pg, key)
	if err != nil || reached == nil {
		return false, err
	}
	pg = reached
	if page != p.store {
		h.bind(p.memoryRegion.binding(page), pg)
	}
	h.mu.Lock()
	h.stats.IdentityHits++
	h.mu.Unlock()
	p.pages[page-p.start] = pg
	p.locked[pg] = true
	p.fresh[page-p.start] = true
	return false, nil
}

// survey is what one look at the rest of a located plan finds: the pages
// whose identity is resident, which bindNeighbours binds to it, and for every
// page the file a read must bring it into, nil where no read must.
type survey struct {
	resident []neighbour
	into     []*arenaFile
}

// neighbour is one page of a plan, its identity and the resident page it
// named when the plan looked.
type neighbour struct {
	page uint64
	key  pageKey
	pg   *resident
}

// survey looks at every page of the located plan but except that the plan
// holds nothing for yet and that can join it, all at once: its bindings under
// the memory region's binding lock once, and the resident and in-flight
// identities under the host's lock once. A hole it marks as one. A page whose
// identity is resident is one to bind to that page. Every other must be read
// into the file of its identity, unless its bytes go in this memory region's
// own file or a prefetch is already reading it; where prefetched says the
// read is a prefetch's, unless too it could not land as a clean shared page,
// having no identity or being one another host still holds.
func (p *windowPlan) survey(except uint64, prefetched bool) survey {
	r := p.memoryRegion
	h := r.host
	found := survey{into: make([]*arenaFile, p.end-p.start)}
	holes := false
	bindings := r.lookupBindings(p.start, p.end)
	files := p.fileFinder()
	h.mu.Lock()
	reading := h.readingIn(p.start, p.end)
	for at, b := range bindings {
		page := p.start + uint64(at)
		if page == except || p.pages[at] != nil || p.zeros[at] || p.reserved[at].slot >= 0 || !eligibleLocked(b) {
			continue
		}
		id, named := p.identity(page)
		if named && id.zero() {
			p.zeros[at], p.fresh[at] = true, true
			holes = true
			continue
		}
		if named {
			if pg := h.clean[id]; pg != nil {
				found.resident = append(found.resident, neighbour{page: page, key: id, pg: pg})
				continue
			}
		}
		switch {
		case p.ownLocked(page):
		case !named:
			if !prefetched {
				found.into[at] = p.file
			}
		case reading.of(id):
			// A page a prefetch is reading is that prefetch's to bring in.
		case !prefetched || !p.unpublished(page):
			found.into[at] = files.of(id)
		}
	}
	h.mu.Unlock()
	if holes {
		p.observeZeros()
	}
	return found
}

// bindNeighbours is bindShared, without waiting, of the pages a survey found
// resident. A page whose identity another resident page took since is looked
// up again; one whose resident page is busy is left for a later fault.
func (p *windowPlan) bindNeighbours(ctx context.Context, found survey) error {
	for _, n := range found.resident {
		again, err := p.bindResident(ctx, n.page, n.key, n.pg, false)
		if err != nil {
			return err
		}
		if again {
			if err := p.bindShared(ctx, n.page, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *windowPlan) reserve(page uint64, at fileSlot) {
	p.reserved[page-p.start] = at
	p.fresh[page-p.start] = true
}

// fileOf is the file a load of this page by its identity goes in: the public
// file for a page of a public template, and the window's file for every other.
// A public page is never in a tenant's file, and only a public page is in the
// public file, so every VMM may be given it.
func (p *windowPlan) fileOf(page uint64) *arenaFile {
	if public := p.memoryRegion.public; public != nil {
		if id, named := p.identity(page); named && control.Public(id.id.Ref.VM) {
			return public
		}
	}
	return p.file
}

// fileFinder is fileOf for many pages, which are mostly of a few checkpoints
// one after another: whether a checkpoint is a public template's is asked once
// where its pages begin.
type fileFinder struct {
	p        *windowPlan
	vm       string
	public   bool
	answered bool
}

func (p *windowPlan) fileFinder() *fileFinder { return &fileFinder{p: p} }

// of is the file a load of a page named id goes in.
func (f *fileFinder) of(id pageKey) *arenaFile {
	public := f.p.memoryRegion.public
	if public == nil {
		return f.p.file
	}
	if !f.answered || id.id.Ref.VM != f.vm {
		f.vm, f.public, f.answered = id.id.Ref.VM, control.Public(id.id.Ref.VM), true
	}
	if f.public {
		return public
	}
	return f.p.file
}

// files is every file this window's loads by identity may go in.
func (p *windowPlan) files() []*arenaFile {
	if public := p.memoryRegion.public; public != nil {
		return []*arenaFile{p.file, public}
	}
	return []*arenaFile{p.file}
}

// reserveAround reserves free slots for the run of pages that need loading
// around the faulting page, so the run can become one mapping. When fewer
// slots are free than the run needs, the pages from the faulting one forward
// take them. Nothing is evicted; the page may remain unreserved. The run is
// of pages whose loads go in the faulting page's file.
func (p *windowPlan) reserveAround(index uint64) {
	file := p.fileOf(index)
	into := p.survey(index, false).into
	needs := func(page uint64) bool { return into[page-p.start] == file }
	first, last := index, index+1
	for first > p.start && needs(first-1) {
		first--
	}
	for last < p.end && needs(last) {
		last++
	}
	at, count := p.memoryRegion.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	for k := range count {
		p.reserve(start+uint64(k), at.plus(k))
	}
}

// reserveRuns takes free slots, without evicting, for the pages into says to
// read into a file (survey) that hold no reservation yet. Runs of consecutive
// pages prefer consecutive slots so a later mapping installs them as one range.
// Idle pages are given up first to make those slots free, which is not an
// eviction: nothing maps them. When free slots cannot cover the window even
// so, the pages after the faulting one come first: access tends to continue
// forward.
func (p *windowPlan) reserveRuns(ctx context.Context, from uint64, into []*arenaFile) error {
	for _, file := range p.files() {
		if err := p.reserveRunsIn(ctx, from, file, into); err != nil {
			return err
		}
	}
	return nil
}

// reserveRunsIn is reserveRuns for the pages whose loads go in one file.
func (p *windowPlan) reserveRunsIn(ctx context.Context, from uint64, file *arenaFile, into []*arenaFile) error {
	h := p.memoryRegion.host
	needs := func(page uint64) bool { return into[page-p.start] == file && p.reserved[page-p.start].slot < 0 }
	needed := 0
	for page := p.start; page < p.end; page++ {
		if needs(page) {
			needed++
		}
	}
	if needed == 0 {
		return nil
	}
	if err := h.makeRoom(ctx, file, needed); err != nil {
		return err
	}
	spans := [][2]uint64{{p.start, p.end}}
	h.mu.Lock()
	if h.freeLocked(file) < needed {
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
			at, count := h.allocateFree(file, int(run))
			for k := range count {
				p.reserve(page+uint64(k), at.plus(k))
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
	first, last := p.end, p.start
	loading := uint64(0)
	for page := p.start; page < p.end; page++ {
		if p.reserved[page-p.start].slot < 0 {
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
		wanted[page-first] = p.reserved[page-p.start].slot >= 0
	}
	buffer := h.takeWindow(last - first)
	defer h.putWindow(buffer)
	data := *buffer
	unpublished, err := p.memoryRegion.loadRun(ctx, first, wanted, data)
	if err != nil {
		return err
	}
	return p.publishRead(ctx, first, wanted, data, unpublished)
}

// publishRead publishes what one backing read brought in: the pages of
// [first, first+len(wanted)) that wanted marks, from data, which covers the
// run whole, as residents under their stored identities, or as this memory
// region's own dirty state where unpublished says the backing served them
// from another host.
func (p *windowPlan) publishRead(ctx context.Context, first uint64, wanted []bool, data []byte, unpublished []bool) error {
	h := p.memoryRegion.host
	ps := h.pageSize
	last := first + uint64(len(wanted))
	loading := uint64(0)
	for _, want := range wanted {
		if want {
			loading++
		}
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
	at := p.reserved[i]
	if private {
		if at.file != p.memoryRegion.privateFile() {
			// The extents named this page the volume's, and the load found it
			// another host's. Its bytes are this memory region's own, so they go
			// in its own file and never in one another region may read.
			var moved bool
			if at, moved = p.ownInstead(page); !moved {
				if page != p.fault {
					p.fresh[i] = false
					return nil
				}
				return errUnpublishedReservation
			}
		}
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
		p.reserved[i] = fileSlot{slot: -1}
		pg, err := h.create(ctx, at, data, pageKey{}, true, p.memoryRegion.kind)
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
	p.reserved[i] = fileSlot{slot: -1}
	id, named := p.identity(page)
	// Only a page loaded into a file other memory regions may read is shared
	// under its identity. One loaded into this memory region's own file is its
	// alone.
	shared := named && at.file == p.fileOf(page)
	key := pageKey{}
	if shared {
		key = id
	}
	pg, err := h.create(ctx, at, data, key, false, p.memoryRegion.kind)
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
			writable = append(writable, r.runAt(page, pg.fileSlot, int(run)))
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
			runs = append(runs, r.runAt(page, at, int(run)))
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
