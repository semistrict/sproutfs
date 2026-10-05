package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// zplan is windowPlan over the zircon core: the pages of one window a fault or
// a populate maps together, the slots it took to read pages into, and what is
// left to map. It holds no lock of a page: Zircon's pages are its objects',
// and a page the plan takes is bound to the region at once, under its root's
// lock, which keeps it out of every idle drop until the region lets it go.
type zplan struct {
	z            *zirconRegion
	start, end   uint64
	fault, store uint64
	// window and alone are windowPlan's: the plan's pages located whole once
	// located says so, and the faulting page alone before that.
	window  locations
	located bool
	alone   locations
	reading reading
	// provisional is windowPlan's provisional run.
	provisional provisionalRun
	// pages is the page the plan maps at each page of the window, by
	// page-start, and reserved the slot each page's read has, or slot -1.
	pages    []*zirconvm.VmPage
	reserved []fileSlot
	// fresh marks a page whose mapping is not installed yet, zeros a page
	// mapped to zero.
	fresh, zeros []bool
	// writable marks a page the plan maps writable: the region's own Dirty
	// page, which the guest may store into where it is.
	writable      []bool
	observedZeros bool
	installed     installedRuns
	// locked is every page the plan holds the lock of: each page it maps,
	// from the moment it takes it until the plan is unlocked, which is after
	// its commands landed, so no eviction takes a page between the two.
	locked []*zirconvm.VmPage
	// request is the READ request the faulting page's lookup sent, which
	// this plan's read answers, nil where it sent none. One the plan has
	// not answered when it is unlocked is failed, so whatever waits on it
	// looks again.
	request *zrequest
}

func (z *zirconRegion) newPlan(start, end, fault uint64) *zplan {
	p := &zplan{z: z, start: start, end: end, fault: fault, store: end,
		pages: make([]*zirconvm.VmPage, end-start), reserved: make([]fileSlot, end-start),
		fresh: make([]bool, end-start), zeros: make([]bool, end-start), writable: make([]bool, end-start)}
	for i := range p.reserved {
		p.reserved[i] = fileSlot{slot: -1}
	}
	return p
}

// plan is a plan of the window [start, end) located whole.
func (z *zirconRegion) plan(ctx context.Context, start, end, fault uint64) (*zplan, error) {
	window, err := z.region.locate(ctx, start, end)
	if err != nil {
		return nil, err
	}
	p := z.newPlan(start, end, fault)
	p.window, p.located = window, true
	return p, nil
}

// planPage is a plan of the window [start, end) that has located page alone.
func (z *zirconRegion) planPage(ctx context.Context, start, end, fault, page uint64) (*zplan, error) {
	alone, err := z.region.locate(ctx, page, page+1)
	if err != nil {
		return nil, err
	}
	p := z.newPlan(start, end, fault)
	p.alone = alone
	return p, nil
}

// locateWindow locates the whole window of a plan that has located only its
// faulting page.
func (p *zplan) locateWindow(ctx context.Context) error {
	if p.located {
		return nil
	}
	window, err := p.z.region.locate(ctx, p.start, p.end)
	if err != nil {
		return err
	}
	p.window, p.located = window, true
	return nil
}

// locationsOf is the locations a page is located in: the faulting page's own
// where the plan located it alone, and the window's otherwise.
func (p *zplan) locationsOf(page uint64) *locations {
	if p.alone.holds(page) {
		return &p.alone
	}
	if !p.located {
		panic("vmmemory: a plan asked about a page of a window it has not located")
	}
	return &p.window
}

// identity is windowPlan.identity.
func (p *zplan) identity(page uint64) (pageKey, bool) {
	return identityAt(p.locationsOf(page), page)
}

// unlock gives back every slot the plan took and did not fill.
func (p *zplan) unlock() {
	p.request.fail()
	h := p.z.region.host
	h.mu.Lock()
	for i, at := range p.reserved {
		if at.slot >= 0 {
			h.putFree(at)
			p.reserved[i] = fileSlot{slot: -1}
		}
	}
	for _, at := range p.provisional.slots {
		h.putFree(at)
	}
	p.provisional = provisionalRun{}
	for _, page := range p.locked {
		frameOf(page).mu.Unlock()
	}
	p.locked = nil
	h.signal()
	h.mu.Unlock()
}

// release takes page off the pages the plan holds the lock of, for its
// caller to hold.
func (p *zplan) release(page *zirconvm.VmPage) {
	for i, held := range p.locked {
		if held == page {
			p.locked = append(p.locked[:i], p.locked[i+1:]...)
			return
		}
	}
	panic("vmmemory: a plan gave up a page it does not hold")
}

// hold takes the lock of a page the plan is about to take, which it holds
// until it is unlocked, and reports false where something else holds it, an
// eviction most likely, and the plan leaves the page alone. It is called
// under the page's object lock, so it never waits.
func (p *zplan) hold(page *zirconvm.VmPage) bool {
	if !frameOf(page).mu.TryLock() {
		return false
	}
	p.locked = append(p.locked, page)
	return true
}

func (p *zplan) reserve(page uint64, at fileSlot) {
	p.reserved[page-p.start] = at
	p.fresh[page-p.start] = true
}

// fileOf is windowPlan.fileOf: the public file for a page of a public
// template, the region's shared file for any other.
func (p *zplan) fileOf(page uint64) *arenaFile {
	r := p.z.region
	if r.public != nil {
		if id, named := p.identity(page); named && control.Public(id.id.Ref.VM) {
			return r.public
		}
	}
	return r.sharedFile()
}

// files is every file this window's loads by identity may go in.
func (p *zplan) files() []*arenaFile {
	r := p.z.region
	if r.public != nil {
		return []*arenaFile{r.sharedFile(), r.public}
	}
	return []*arenaFile{r.sharedFile()}
}

// own reports a page an isolated arena reads into the region's own file: one
// whose bytes no identity names. This core takes no fork point's and no
// other host's page yet.
func (p *zplan) own(page uint64) bool {
	h := p.z.region.host
	if !h.isolated() {
		return false
	}
	id, named := p.identity(page)
	if named && id.zero() {
		return false
	}
	// A page a fork point lends is read into the region's own file: its name
	// lasts only as long as the point's seal.
	return !named || h.lends(id)
}

// observeZeros is windowPlan.observeZeros.
func (p *zplan) observeZeros() {
	if p.observedZeros {
		return
	}
	r := p.z.region
	h := r.host
	h.mu.Lock()
	if !r.hasZeros {
		r.hasZeros = true
		h.zeroMemoryRegions++
	}
	h.mu.Unlock()
	p.observedZeros = true
}

// markZeros records a whole explicit zero extent, skipping the pages that may
// not join the plan.
func (p *zplan) markZeros(first, last uint64) {
	if first >= last {
		return
	}
	p.observeZeros()
	held := p.z.heldIn(first, last)
	for page := first; page < last; page++ {
		if len(held) > 0 && held[0] == page {
			held = held[1:]
			continue
		}
		p.zeros[page-p.start], p.fresh[page-p.start] = true, true
	}
}

// eligible reports whether a page other than the faulting one can join the
// plan: the region holds nothing there yet.
func (p *zplan) eligible(page uint64) bool { return p.z.eligible(page) }

// take binds page to the page its root holds under key, where it holds one,
// and reports it. It waits for the page's lock, which it takes with no
// object lock held, and binds the page only where its root still holds it
// then, under its root's lock, so no idle drop takes it between the two. A
// population takes pages in one order of identities, so two that wait on each
// other's pages cannot both be waiting.
func (p *zplan) take(ctx context.Context, page uint64, key pageKey) (*zirconvm.VmPage, error) {
	z := p.z
	h := z.region.host
	root := z.host.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	for {
		lock.Lock()
		found := root.pages.PageLocked(offset)
		lock.Unlock()
		if found == nil {
			return nil, nil
		}
		if err := z.host.lockPage(ctx, found); err != nil {
			return nil, err
		}
		lock.Lock()
		still := root.pages.PageLocked(offset) == found
		lock.Unlock()
		if !still {
			z.host.unlockPage(found)
			continue
		}
		// A page this region's process may not map where it is is moved or
		// copied where it may, and the copy is held in its place.
		reached, err := z.reach(ctx, found, key)
		if err != nil || reached == nil {
			return nil, err
		}
		found = reached
		z.host.node.PageQueues().MarkAccessed(found)
		if page != p.store {
			z.bind(page, found)
		}
		p.locked = append(p.locked, found)
		h.mu.Lock()
		h.stats.IdentityHits++
		h.mu.Unlock()
		p.pages[page-p.start] = found
		p.fresh[page-p.start] = true
		return found, nil
	}
}

// zsurvey is survey over the zircon core.
type zsurvey struct {
	into []*arenaFile
}

// survey looks at every page of the located plan but except that the plan
// holds nothing for yet and that can join it. A hole it marks as one. A page
// its root holds it takes, with the root's lock held once for each run of
// pages of one root, which is Zircon's fault-around of the pages present
// (IfExistPages). Every other page must be read into the file of its
// identity, unless its bytes go in the region's own file or a prefetch is
// reading it already; where prefetched says the read is a prefetch's, a page
// with no identity is left to its own fault.
func (p *zplan) survey(except uint64, prefetched bool) zsurvey {
	z := p.z
	found := zsurvey{into: make([]*arenaFile, p.end-p.start)}
	eligible := z.eligibleIn(p.start, p.end)
	holes := false
	reading := z.host.readingIn(p.start, p.end)
	files := &zfileFinder{p: p}
	for page := p.start; page < p.end; {
		at := page - p.start
		if page == except || p.pages[at] != nil || p.zeros[at] || p.reserved[at].slot >= 0 || !eligible[at] {
			page++
			continue
		}
		id, named := p.identity(page)
		if named && id.zero() {
			p.zeros[at], p.fresh[at] = true, true
			holes = true
			page++
			continue
		}
		if !named {
			if !prefetched && !p.own(page) {
				found.into[at] = p.fileOf(page)
			}
			page++
			continue
		}
		// The run of pages of this root from here, which one hold of its lock
		// looks up.
		root := rootOf(id)
		last := page + 1
		for last < p.end && last != except {
			next, ok := p.identity(last)
			if !ok || next.zero() || rootOf(next) != root {
				break
			}
			last++
		}
		taken, elsewhere := p.takeRootRun(page, last, eligible)
		for q := page; q < last; q++ {
			i := q - p.start
			if taken[q-page] || elsewhere[q-page] || p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !eligible[i] {
				continue
			}
			key, _ := p.identity(q)
			if reading.of(key) {
				// A page a prefetch is reading is that prefetch's to bring in.
				continue
			}
			found.into[i] = files.of(key)
		}
		page = last
	}
	if holes {
		p.observeZeros()
	}
	return found
}

// takeRootRun takes every page of [first, last), all of one root, that the root
// holds and that may join the plan, under one hold of the root's lock, and
// reports which it took, and which the root holds where this region's process
// may not map them: those are their own faults' to reach.
func (p *zplan) takeRootRun(first, last uint64, eligible []bool) (taken, elsewhere []bool) {
	z := p.z
	h := z.region.host
	key, _ := p.identity(first)
	root := z.host.root(rootOf(key))
	taken = make([]bool, last-first)
	elsewhere = make([]bool, last-first)
	hits := uint64(0)
	lock := root.pages.Lock()
	lock.Lock()
	for page := first; page < last; page++ {
		i := page - p.start
		if p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !eligible[i] {
			continue
		}
		found := root.pages.PageLocked(page * h.pageSize)
		if found == nil {
			continue
		}
		if !z.reachable(found) {
			elsewhere[page-first] = true
			continue
		}
		if !p.hold(found) {
			continue
		}
		z.host.node.PageQueues().MarkAccessed(found)
		z.bind(page, found)
		p.pages[i], p.fresh[i], taken[page-first] = found, true, true
		hits++
	}
	lock.Unlock()
	if hits > 0 {
		h.mu.Lock()
		h.stats.IdentityHits += hits
		h.mu.Unlock()
	}
	return taken, elsewhere
}

// zfileFinder is fileFinder over a zplan.
type zfileFinder struct {
	p        *zplan
	vm       string
	public   bool
	answered bool
}

func (f *zfileFinder) of(id pageKey) *arenaFile {
	r := f.p.z.region
	if r.public == nil {
		return r.sharedFile()
	}
	if !f.answered || id.id.Ref.VM != f.vm {
		f.vm, f.public, f.answered = id.id.Ref.VM, control.Public(id.id.Ref.VM), true
	}
	if f.public {
		return r.public
	}
	return r.sharedFile()
}

// reserveAround is windowPlan.reserveAround.
func (p *zplan) reserveAround(index uint64) {
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
	at, count := p.z.region.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	for k := range count {
		p.reserve(start+uint64(k), at.plus(k))
	}
}

// reserveProvisional is windowPlan.reserveProvisional.
func (p *zplan) reserveProvisional(index uint64) {
	file := p.fileOf(index)
	first, last := index, index+1
	if p.reading == readFirst {
		first, last = p.start, p.end
	}
	at, count := p.z.region.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	p.reserve(index, at.plus(int(index-start)))
	if count > 1 {
		p.provisional = provisionalRun{first: start, faulting: index, at: at, count: count}
	}
}

// keepProvisional is windowPlan.keepProvisional.
func (p *zplan) keepProvisional(into []*arenaFile) {
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
	h := p.z.region.host
	h.mu.Lock()
	for _, at := range back {
		h.putFree(at)
	}
	h.signal()
	h.mu.Unlock()
}

// reserveRuns is windowPlan.reserveRuns.
func (p *zplan) reserveRuns(ctx context.Context, from uint64, into []*arenaFile) error {
	for _, file := range p.files() {
		if err := p.reserveRunsIn(ctx, from, file, into); err != nil {
			return err
		}
	}
	return nil
}

// reserveRunsIn is windowPlan.reserveRunsIn.
func (p *zplan) reserveRunsIn(ctx context.Context, from uint64, file *arenaFile, into []*arenaFile) error {
	h := p.z.region.host
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
	if err := p.z.host.makeRoom(ctx, file, needed); err != nil {
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

// reserveOwn takes places in the region's own file for the pages of the
// window it reads there, without evicting.
func (p *zplan) reserveOwn() {
	r := p.z.region
	h := r.host
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		if p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !p.own(page) || !p.eligible(page) {
			continue
		}
		h.mu.Lock()
		for _, at := range r.ownPlaces(page, true) {
			if taken := h.takeOwnLocked(r, page, at); taken.slot >= 0 {
				p.reserve(page, taken)
				break
			}
		}
		h.mu.Unlock()
	}
}

// loadReserved reads the reserved pages of the window with one backing read
// and supplies them, as windowPlan.loadReserved does.
func (p *zplan) loadReserved(ctx context.Context) error {
	h := p.z.region.host
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
	r := p.z.region
	// The read answers the READ request the faulting page's lookup sent,
	// where the run holds that page: its supply below resolves it, and a
	// failed read fails it when the plan is unlocked.
	err := r.withoutMemoryRegion(ctx, func() error {
		_, err := r.readRun(ctx, first, wanted, data, &h.loadLatency)
		return err
	})
	if err != nil {
		return err
	}
	return p.publishRead(ctx, first, wanted, data)
}

// publishRead supplies what one backing read brought in: the pages of [first,
// first+len(wanted)) that wanted marks, from data, which covers the run
// whole. A page with an identity goes to its root, and is bound to whichever
// page the root then holds; one with none goes to the region's own layer.
// A run of consecutive pages of one root is one supply.
func (p *zplan) publishRead(ctx context.Context, first uint64, wanted []bool, data []byte) error {
	z := p.z
	r := z.region
	h := r.host
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
	for page := first; page < last; {
		at := page - first
		if !wanted[at] {
			page++
			continue
		}
		id, named := p.identity(page)
		shared := named && p.reserved[page-p.start].file == p.fileOf(page)
		run := uint64(1)
		if shared {
			for page+run < last && wanted[at+run] {
				next, ok := p.identity(page + run)
				if !ok || next.zero() || rootOf(next) != rootOf(id) ||
					p.reserved[page+run-p.start].file != p.reserved[page-p.start].file {
					break
				}
				run++
			}
		}
		frames := make([]*zirconvm.VmPage, 0, run)
		var failure error
		for q := page; q < page+run; q++ {
			i := q - p.start
			slot := p.reserved[i]
			p.reserved[i] = fileSlot{slot: -1}
			offset := (q - first) * ps
			frame, err := z.host.newFrame(ctx, slot, data[offset:offset+ps], r.kind)
			if err != nil {
				// newFrame gave this slot back, and the slots of the rest of
				// the run go back with the plan.
				failure = err
				break
			}
			frames = append(frames, frame)
		}
		if len(frames) > 0 {
			if err := p.supplyRun(ctx, page, id, shared, frames); err != nil {
				return errors.Join(failure, err)
			}
		}
		if failure != nil {
			return failure
		}
		page += run
	}
	return nil
}

// supplyRun supplies the frames read for the pages from page on: to their
// root where shared, and to the region's own layer otherwise. Each page is
// then bound to the page its object holds, which is the one read here unless
// another read supplied its own first.
func (p *zplan) supplyRun(ctx context.Context, page uint64, id pageKey, shared bool, frames []*zirconvm.VmPage) error {
	z := p.z
	h := z.region.host
	ps := h.pageSize
	object, pages := z.layer, z.pages
	if shared {
		root := z.host.root(rootOf(id))
		object, pages = root.object, root.pages
	} else {
		for _, frame := range frames {
			frameOf(frame).layer = z
		}
	}
	if err := z.host.supply(ctx, object, page, frames); err != nil {
		return err
	}
	last := page + uint64(len(frames))
	if p.fault >= page && p.fault < last {
		// The faulting page's own read request is answered by its supply.
		p.request.answer(nil)
	}
	// Each page is bound to what its object holds now, under the object's
	// lock: the frame read here, or the page another read supplied first.
	hits := uint64(0)
	lock := pages.Lock()
	lock.Lock()
	for q := page; q < last; q++ {
		i := q - p.start
		found := pages.PageLocked(q * ps)
		if found == nil {
			// Given up already, by a drop of idle pages; a later fault reads
			// it.
			p.fresh[i] = false
			continue
		}
		if found == frames[q-page] {
			// The frame read here, which the plan holds from its making.
			p.locked = append(p.locked, found)
			if shared {
				h.mu.Lock()
				z.host.adoptLocked(found)
				h.mu.Unlock()
			}
		} else if z.reachable(found) && p.hold(found) {
			hits++
		} else {
			p.fresh[i] = false
			continue
		}
		z.host.node.PageQueues().MarkAccessed(found)
		// A store's own page is bound too, unmapped: it is what the store
		// copies from, and the binding keeps it from an idle drop until the
		// copy replaces it.
		p.pages[i], p.fresh[i] = found, true
		z.bind(q, found)
	}
	lock.Unlock()
	if hits > 0 {
		h.mu.Lock()
		h.stats.IdentityHits += hits
		h.mu.Unlock()
	}
	return nil
}

// install maps every planned page and resolves the faulting one, as
// windowPlan.install does. The commands are issued with no object lock held:
// a plan holds none, its pages being bound to the region already. The
// region's own Dirty pages are mapped writable, in runs of their own.
func (p *zplan) install(ctx context.Context) (bool, error) {
	z := p.z
	r := z.region
	h := r.host
	var runs, writable []MapRun
	// Whether the region's own page is mapped writable is decided now, with
	// the page held: a seal taken while the plan read gave the region up
	// makes it the checkpoint's, read-only.
	for i, pg := range p.pages {
		if pg != nil && frameOf(pg).layer == z {
			p.writable[i] = z.writable(p.start + uint64(i))
		}
	}
	for page := p.start; page < p.end; {
		i := page - p.start
		pg := p.pages[i]
		zero := p.zeros[i]
		if (pg == nil && !zero) || !p.fresh[i] || page == p.store {
			page++
			continue
		}
		if z.mapped(page) {
			p.fresh[i] = false
			page++
			continue
		}
		var at fileSlot
		if pg != nil {
			at = frameOf(pg).fileSlot
		}
		run := uint64(1)
		for page+run < p.end {
			next := p.pages[i+run]
			if !p.fresh[i+run] || page+run == p.store || z.mapped(page+run) || p.zeros[i+run] != zero ||
				p.writable[i+run] != p.writable[i] ||
				(!zero && (next == nil || frameOf(next).fileSlot != at.plus(int(run)))) {
				break
			}
			run++
		}
		switch {
		case zero:
			z.mapZeros(page, page+run)
			runs = append(runs, MapRun{Page: page, Count: int(run), Zero: true})
		case p.writable[i]:
			z.setMapped(page, page+run, true)
			writable = append(writable, r.runAt(page, at, int(run)))
		default:
			z.setMapped(page, page+run, true)
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
				return false, r.mappingFailed(err, func() { z.unmapRuns(runs) })
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
					return false, r.mappingFailed(err, func() { z.unmapRuns(runs[i:]) })
				}
			}
		}
		h.mu.Lock()
		h.stats.Mappings += uint64(commands)
		h.stats.MappingRuns += uint64(mappingRuns)
		h.mu.Unlock()
		p.installed.add(installedRuns{commands: uint64(commands), runs: uint64(mappingRuns), pages: pagesOf(runs)})
	}
	for _, run := range writable {
		if err := r.mapPages(ctx, run, true); err != nil {
			return false, r.mappingFailed(err, func() { z.unmapRuns([]MapRun{run}) })
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
	// The faulting page was mapped by an earlier attempt; its trapped access
	// still has to be completed.
	if !z.mapped(p.fault) {
		return false, nil
	}
	if err := r.resolvePages(ctx, p.fault, 1, p.writable[p.fault-p.start]); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}
