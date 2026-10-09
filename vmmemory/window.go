package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// installedRuns is a set of mapping commands as what they cost: the commands
// themselves, the runs they covered and the pages in those runs.
type installedRuns struct{ commands, runs, pages uint64 }

func (i *installedRuns) add(other installedRuns) {
	i.commands += other.commands
	i.runs += other.runs
	i.pages += other.pages
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

// pagesOf is how many pages a set of runs covers.
func pagesOf(runs []MapRun) uint64 {
	pages := uint64(0)
	for _, run := range runs {
		pages += uint64(run.Count)
	}
	return pages
}

// plan is the pages of one window a fault or a populate maps together, the
// slots it took to read pages into, and what is left to map. It holds no lock
// of a page: Zircon's pages are its objects', and a page the plan takes is
// bound to the region at once, under its root's lock, which keeps it out of
// every idle drop until the region lets it go.
type plan struct {
	region       *MemoryRegion
	start, end   uint64
	fault, store uint64
	// window is the plan's pages located whole, once located says so, and
	// alone the faulting page located alone before that.
	//
	// free marks the pages of the window the region held nothing at when it
	// was located, nil where it is planned under the hold that located it. A
	// page the region holds may be its own state, which its checkpoint names
	// anew once published, and a fault may give its region up between
	// locating its window and planning it (reclaimWith): a retire there
	// leaves such a page spilled and held by nothing, and its name in window
	// is its parent's. So only a page free marks joins the plan by the name
	// located then (survey). A page the region held nothing at keeps its
	// name while the plan holds the window's stripe: only a store into it
	// makes it the region's own.
	window  locations
	located bool
	free    []bool
	alone   locations
	reading reading
	// readFrom and readTo bound the pages the plan reads, of those it plans:
	// a fault that reads its page first reads only what its stream has earned
	// (readsAhead), and maps the rest of its window only where resident.
	readFrom, readTo uint64
	// provisional is the run of slots a fault that reads its page first took
	// for its window (reserveProvisional).
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
	// its commands landed, so no eviction takes a page between the two. It
	// starts in lockedFirst, so a fault at random, which holds one page,
	// allocates nothing to hold it.
	locked      []*zirconvm.VmPage
	lockedFirst [1]*zirconvm.VmPage
	// spill is the dirty reservation the fault brought with it, for a page a
	// peer backing serves as the region's own dirty state, nil or none where
	// it brought none. private marks the pages the plan took so.
	spill   *reservation
	private []bool
	// request is the READ request the faulting page's lookup sent, which
	// this plan's read answers, nil where it sent none. One the plan has
	// not answered when it is unlocked is failed, so whatever waits on it
	// looks again.
	request *readRequest
}

func (r *MemoryRegion) newPlan(start, end, fault uint64) *plan {
	// The plan's four marks of each page are one allocation: a fault at
	// random pays for each one it makes.
	n := end - start
	marks := make([]bool, 4*n)
	p := &plan{region: r, start: start, end: end, fault: fault, store: end, readFrom: start, readTo: end,
		pages: make([]*zirconvm.VmPage, n), reserved: make([]fileSlot, n),
		fresh: marks[:n:n], zeros: marks[n : 2*n : 2*n], writable: marks[2*n : 3*n : 3*n],
		private: marks[3*n:]}
	p.locked = p.lockedFirst[:0]
	for i := range p.reserved {
		p.reserved[i] = fileSlot{slot: -1}
	}
	return p
}

// plan is a plan of the window [start, end) located whole, and of the pages
// the region held nothing at then (free). Caller holds the region, as it does
// for the whole of a fault's planning but where it gives the region up.
func (r *MemoryRegion) plan(ctx context.Context, start, end, fault uint64) (*plan, error) {
	window, err := r.locate(ctx, start, end)
	if err != nil {
		return nil, err
	}
	p := r.newPlan(start, end, fault)
	p.window, p.located = window, true
	p.free = r.eligibleIn(start, end)
	return p, nil
}

// planPage is a plan of the window [start, end) that has located page alone.
func (r *MemoryRegion) planPage(ctx context.Context, start, end, fault, page uint64) (*plan, error) {
	alone, err := r.locate(ctx, page, page+1)
	if err != nil {
		return nil, err
	}
	p := r.newPlan(start, end, fault)
	p.alone = alone
	return p, nil
}

// locateWindow locates the whole window of a plan that has located only its
// faulting page. Its callers plan the window under the hold of the region
// that located it (planRest, storeFresh), so free stays nil: what the region
// holds there when they look is what it held then.
func (p *plan) locateWindow(ctx context.Context) error {
	if p.located {
		return nil
	}
	window, err := p.region.locate(ctx, p.start, p.end)
	if err != nil {
		return err
	}
	p.window, p.located = window, true
	return nil
}

// locationsOf is the locations a page is located in: the faulting page's own
// where the plan located it alone, and the window's otherwise.
func (p *plan) locationsOf(page uint64) *locations {
	if p.alone.holds(page) {
		return &p.alone
	}
	if !p.located {
		panic("vmmemory: a plan asked about a page of a window it has not located")
	}
	return &p.window
}

// reads reports whether the plan reads page where it needs reading.
func (p *plan) reads(page uint64) bool { return page >= p.readFrom && page < p.readTo }

// identity reports the store page whose bytes this page reads, which is the
// whole of what names it: a page is published whole or not at all.
func (p *plan) identity(page uint64) (pageKey, bool) {
	return identityAt(p.locationsOf(page), page)
}

// unlock gives back every slot the plan took and did not fill.
func (p *plan) unlock() {
	p.request.fail()
	h := p.region.host
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
	found := ""
	for _, page := range p.locked {
		if f := h.probe.stable(context.Background(), h, frameOf(page), "unlock"); f != "" && found == "" {
			found = f
		}
		frameOf(page).mu.Unlock()
	}
	p.locked = nil
	h.signal()
	h.mu.Unlock()
	if found == "" {
		found = h.probe.take()
	}
	if found != "" {
		panic(found)
	}
}

// release takes page off the pages the plan holds the lock of, for its
// caller to hold.
func (p *plan) release(page *zirconvm.VmPage) {
	for i, held := range p.locked {
		if held == page {
			p.locked = append(p.locked[:i], p.locked[i+1:]...)
			return
		}
	}
	panic("vmmemory: a plan gave up a page it does not hold")
}

// holdsOwn reports whether the plan holds a page of its region's own layer:
// the faulting page, where the lookup found the region's own page there.
// Caller holds those pages, which keeps them in the layer.
func (p *plan) holdsOwn() bool {
	for _, page := range p.locked {
		if frameOf(page).layer == p.region {
			return true
		}
	}
	return false
}

// hold takes the lock of a page the plan is about to take, which it holds
// until it is unlocked, and reports false where something else holds it, an
// eviction most likely, and the plan leaves the page alone. It is called
// under the page's object lock, so it never waits.
func (p *plan) hold(page *zirconvm.VmPage) bool {
	if !frameOf(page).mu.TryLock() {
		return false
	}
	p.locked = append(p.locked, page)
	return true
}

func (p *plan) reserve(page uint64, at fileSlot) {
	p.reserved[page-p.start] = at
	p.fresh[page-p.start] = true
}

// fileOf is the file a read of this page by its identity goes in: the public
// file for a page of a public template, the region's shared file for any
// other. A public page is never in a tenant's file, and only a public page is
// in the public file, so every VMM may be given it.
func (p *plan) fileOf(page uint64) *arenaFile {
	r := p.region
	if r.public != nil {
		if id, named := p.identity(page); named && control.Public(id.id.Ref.VM) {
			return r.public
		}
	}
	return r.sharedFile()
}

// files is every file this window's loads by identity may go in.
func (p *plan) files() []*arenaFile {
	r := p.region
	if r.public != nil {
		return []*arenaFile{r.sharedFile(), r.public}
	}
	return []*arenaFile{r.sharedFile()}
}

// own reports a page an isolated arena reads into the region's own file: one
// whose bytes no identity names. The pager takes no fork point's and no other
// host's page here yet.
func (p *plan) own(page uint64) bool {
	h := p.region.host
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

// observeZeros records that this memory region knows about explicit zeros,
// which is what lets a sibling attachment map them eagerly without any
// metadata of its own. It is idempotent per plan.
func (p *plan) observeZeros() {
	if p.observedZeros {
		return
	}
	r := p.region
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
func (p *plan) markZeros(first, last uint64) {
	if first >= last {
		return
	}
	p.observeZeros()
	held := p.region.heldIn(first, last)
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
func (p *plan) eligible(page uint64) bool { return p.region.eligible(page) }

// take binds page to the page its root holds under key, where it holds one,
// and reports it. It waits for the page's lock, which it takes with no
// object lock held, and binds the page only where its root still holds it
// once that lock is held: between the root's two looks the page may be given
// up, and another put in its place, but no idle drop, eviction or move takes
// a page whose lock is held, so after the second look it stays until the plan
// is unlocked. A population takes pages in one order of identities, so two
// that wait on each other's pages cannot both be waiting.
func (p *plan) take(ctx context.Context, page uint64, key pageKey) (*zirconvm.VmPage, error) {
	r := p.region
	h := r.host
	root := r.host.root(rootOf(key))
	offset := key.id.Page * h.pageSize
	lock := root.pages.Lock()
	for {
		lock.Lock()
		found := root.pages.PageLocked(offset)
		lock.Unlock()
		if found == nil {
			return nil, nil
		}
		// In a controlled run another task may go on here, with the root's
		// lock and the page's both free: an idle drop of this page, say.
		if err := sim.Admit(ctx, "vmmemory/populate-take"); err != nil {
			return nil, err
		}
		if err := r.host.lockPage(ctx, found); err != nil {
			return nil, err
		}
		lock.Lock()
		still := root.pages.PageLocked(offset) == found
		lock.Unlock()
		if !still {
			r.host.unlockPage(found)
			continue
		}
		// A page this region's process may not map where it is is moved or
		// copied where it may, and the copy is held in its place.
		reached, err := r.reach(ctx, found, key)
		if err != nil || reached == nil {
			return nil, err
		}
		found = reached
		r.host.node.PageQueues().MarkAccessed(found)
		if page != p.store {
			r.bind(page, found)
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

// survey is what one look at the rest of a located plan finds: for every page
// the file a read must bring it into, nil where no read must.
type survey struct {
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
//
// It looks at the region's bindings, each root and the reads under way under
// holds of their own locks, one after another. The plan holds the window's
// stripe and the region, so no page of the window the region held nothing at
// becomes its between them: only a fault of the window, a populate or a
// checkpoint would bind one. A page it held may go to an eviction, which
// leaves it to its own fault. What other regions do meanwhile, a read that
// supplies a page to its root, a prefetch that lands or starts, an idle
// drop, only makes a look out of date: a page the root
// held is taken only under its lock, and only where the plan can hold it; a
// page a read brings in since is read twice, and the later supply keeps the
// earlier page (supplyRun, landRun); a page whose read ended since is left to
// its own fault; and a prefetch looks at the reads under way again under the
// hold that sends its own (splitPrefetch).
func (p *plan) survey(ctx context.Context, except uint64, prefetched bool) survey {
	r := p.region
	found := survey{into: make([]*arenaFile, p.end-p.start)}
	// A page may join where the region holds nothing there now, and held
	// nothing there when the window was located: the name of one it held may
	// be out of date (plan.free).
	eligible := r.eligibleIn(p.start, p.end)
	if p.free != nil && !sim.Bug(ctx, "pager-survey-by-a-stale-name") {
		for i, free := range p.free {
			eligible[i] = eligible[i] && free
		}
	}
	holes := false
	reading := r.host.readingIn(p.start, p.end)
	files := &fileFinder{p: p}
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
			if !prefetched && !p.own(page) && p.reads(page) {
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
		taken, left := p.takeRootRun(ctx, page, last, eligible)
		for q := page; q < last; q++ {
			i := q - p.start
			if taken[q-page] || left[q-page] || p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !eligible[i] {
				continue
			}
			key, _ := p.identity(q)
			if !p.reads(q) || reading.of(key) {
				// A page past what the fault reads ahead is its own fault's
				// to read, and one a prefetch is reading that prefetch's.
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
// reports which it took, and which it left: those the root holds where this
// region's process may not map them, and those something else holds. They are
// their own faults' to reach.
func (p *plan) takeRootRun(ctx context.Context, first, last uint64, eligible []bool) (taken, left []bool) {
	r := p.region
	h := r.host
	key, _ := p.identity(first)
	root := r.host.root(rootOf(key))
	marks := make([]bool, 2*(last-first))
	taken, left = marks[:last-first:last-first], marks[last-first:]
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
		if !r.reachable(found) {
			left[page-first] = true
			continue
		}
		if !p.hold(found) {
			// Something else holds the page: a prefetch that landed it and
			// has not given it up yet, a fault, an eviction. It is left to
			// its own fault, as an unreachable one is, and never read again.
			left[page-first] = !sim.Bug(ctx, "pager-read-a-held-page-again")
			continue
		}
		// Mapping a page around a fault is no access of it, as Zircon's fault
		// does not count one (DisableMarkAccessed, vm/vm_mapping.cc): a page
		// the guest goes on to use shows it to the harvest.
		if sim.Bug(ctx, "pager-mark-pages-around-accessed") {
			r.host.node.PageQueues().MarkAccessed(found)
		}
		r.bind(page, found)
		p.pages[i], p.fresh[i], taken[page-first] = found, true, true
		hits++
	}
	lock.Unlock()
	if hits > 0 {
		// A count, which decides nothing.
		h.mu.Lock()
		h.stats.IdentityHits += hits
		h.mu.Unlock()
	}
	return taken, left
}

// fileFinder is fileFinder over a plan.
type fileFinder struct {
	p        *plan
	vm       string
	public   bool
	answered bool
}

func (f *fileFinder) of(id pageKey) *arenaFile {
	r := f.p.region
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

// reserveAround reserves free slots for the run of pages that need reading
// around the faulting page, so the run can become one mapping. When fewer
// slots are free than the run needs, the pages from the faulting one forward
// take them. Nothing is evicted; the page may remain unreserved. The run is of
// pages whose reads go in the faulting page's file.
func (p *plan) reserveAround(ctx context.Context, index uint64) {
	file := p.fileOf(index)
	into := p.survey(ctx, index, false).into
	needs := func(page uint64) bool { return into[page-p.start] == file }
	first, last := index, index+1
	for first > p.start && needs(first-1) {
		first--
	}
	for last < p.end && needs(last) {
		last++
	}
	at, count := p.region.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	for k := range count {
		p.reserve(start+uint64(k), at.plus(k))
	}
}

// reserveProvisional takes the faulting page's slot, from a run of free slots
// for its whole window where the fault reads its page first. When fewer are
// free than the window, the run holds the faulting page and the pages after it
// first. Nothing is evicted; the page may remain unreserved.
func (p *plan) reserveProvisional(index uint64) {
	file := p.fileOf(index)
	first, last := index, index+1
	if p.reading == readFirst {
		first, last = p.readFrom, p.readTo
	}
	at, count := p.region.host.allocateFree(file, int(last-first))
	if count == 0 {
		return
	}
	start := max(first, min(index, last-uint64(count)))
	p.reserve(index, at.plus(int(index-start)))
	if count > 1 {
		p.provisional = provisionalRun{first: start, faulting: index, at: at, count: count}
	}
}

// keepProvisional settles the provisional run once the window is located: each
// slot stays reserved for its page where the prefetch will read that page into
// that file (survey), and goes back otherwise.
func (p *plan) keepProvisional(into []*arenaFile) {
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
	h := p.region.host
	h.mu.Lock()
	for _, at := range back {
		h.putFree(at)
	}
	h.signal()
	h.mu.Unlock()
}

// reserveRuns takes free slots, without evicting, for the pages into says to
// read into a file (survey) that hold no reservation yet. Runs of consecutive
// pages prefer consecutive slots so a later mapping installs them as one
// range. Idle pages are given up first to make those slots free, which is not
// an eviction: nothing maps them. When free slots cannot cover the window even
// so, the pages after the faulting one come first: access tends to continue
// forward.
func (p *plan) reserveRuns(ctx context.Context, from uint64, into []*arenaFile) error {
	for _, file := range p.files() {
		if err := p.reserveRunsIn(ctx, from, file, into); err != nil {
			return err
		}
	}
	return nil
}

// reserveRunsIn is reserveRuns for the pages whose reads go in one file.
func (p *plan) reserveRunsIn(ctx context.Context, from uint64, file *arenaFile, into []*arenaFile) error {
	h := p.region.host
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
	if err := p.region.host.makeRoom(ctx, file, needed); err != nil {
		return err
	}
	// The slots made free here are anyone's until they are taken, under
	// holds of h.mu of their own: another allocation may take them first.
	// Then fewer runs are reserved, and the pages left are their own faults'
	// to read. In a controlled run another task may go on here.
	if err := sim.Admit(ctx, "vmmemory/reserve-runs"); err != nil {
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
// window it reads there, without evicting. Whether a page may join is looked
// at again for each page, and a place is taken only where it is free under
// the same hold of h.mu. A page read here goes to the region's own layer by
// what the backing holds when it is read, so a name located before the
// region was given up decides nothing here.
func (p *plan) reserveOwn() {
	r := p.region
	h := r.host
	for page := p.start; page < p.end; page++ {
		i := page - p.start
		if p.pages[i] != nil || p.zeros[i] || p.reserved[i].slot >= 0 || !p.own(page) || !p.eligible(page) ||
			!p.reads(page) {
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
// (readRun) and supplies them.
func (p *plan) loadReserved(ctx context.Context) error {
	h := p.region.host
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
	if p.holdsOwn() && !sim.Bug(ctx, "pager-read-holding-its-own-page") {
		// A retire or an unseal holds the region exclusively and waits for
		// the lock of the region's own page the checkpoint shares, and a read
		// gives the region up and takes it again: with such a page held
		// across the read, each would wait for the other. The plan reads
		// nothing more; the pages it reserved are left to their own faults,
		// and their slots go back when it is unlocked.
		return nil
	}
	wanted := make([]bool, last-first)
	keys := make([]pageKey, last-first)
	for page := first; page < last; page++ {
		wanted[page-first] = p.reserved[page-p.start].slot >= 0
		if id, ok := p.identity(page); ok {
			keys[page-first] = id
		}
	}
	buffer := h.takeWindow(last - first)
	defer h.putWindow(buffer)
	data := *buffer
	r := p.region
	// The read answers the READ request the faulting page's lookup sent,
	// where the run holds that page: its supply below resolves it, and a
	// failed read fails it when the plan is unlocked.
	//
	// The region is given up across the read, so a seal, a retire, an unseal
	// or a populate of it may run whole meanwhile. None changes what the plan
	// acts on after: a reserved page is one the region held nothing at, which
	// keeps its name while the plan holds the window's stripe; a page the plan
	// took is bound and locked, so no eviction or idle drop takes it, and none
	// is the region's own, which a retire would wait for; the supply binds
	// each page to whatever its object holds then (supplyRun);
	// install maps no page the region maps already, and decides whether the
	// region's own page is writable once the region is held again.
	var unpublished []bool
	err := r.withoutMemoryRegion(ctx, func() error {
		// In a controlled run another task may go on here, with the region
		// given up.
		if err := sim.Admit(ctx, "vmmemory/run-read"); err != nil {
			return err
		}
		var err error
		unpublished, err = r.readRun(ctx, first, wanted, keys, data, &h.loadLatency)
		return err
	})
	if err != nil {
		return err
	}
	return p.publishRead(ctx, first, wanted, data, unpublished)
}

// unpublished reports whether the window's extents say this page's bytes
// belong to no object of its volume, which for a backing that fetches from
// another host means that host still holds them.
func (p *plan) unpublished(page uint64) bool {
	if !p.region.peer {
		return false
	}
	e, found := p.locationsOf(page).extent(page)
	return found && !e.Identity.Zero && e.Identity.Ref.IsZero()
}

// publishRead supplies what one backing read brought in: the pages of [first,
// first+len(wanted)) that wanted marks, from data, which covers the run
// whole. A page with an identity goes to its root, and is bound to whichever
// page the root then holds; one with none goes to the region's own layer.
// A run of consecutive pages of one root is one supply.
//
// A page a peer backing reported another host's is the region's own dirty
// state, which the backing is told the region went on to hold.
func (p *plan) publishRead(ctx context.Context, first uint64, wanted []bool, data []byte, unpublished []bool) error {
	r := p.region
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
	var installed []bool
	if len(unpublished) > 0 {
		installed = make([]bool, last-first)
		defer func() { r.installedUnpublished(first*ps, installed) }()
	}
	for page := first; page < last; {
		at := page - first
		if !wanted[at] {
			page++
			continue
		}
		if at < uint64(len(unpublished)) && unpublished[at] {
			if err := p.publishPrivate(ctx, page, data[at*ps:(at+1)*ps]); err != nil {
				return err
			}
			installed[at] = p.private[page-p.start]
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
			frame, err := r.host.newFrame(ctx, slot, data[offset:offset+ps], r.kind)
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
func (p *plan) supplyRun(ctx context.Context, page uint64, id pageKey, shared bool, frames []*zirconvm.VmPage) error {
	r := p.region
	h := r.host
	ps := h.pageSize
	object, pages := r.layer, r.pages
	if shared {
		root := r.host.root(rootOf(id))
		object, pages = root.object, root.pages
	} else {
		for _, frame := range frames {
			frameOf(frame).layer = r
		}
	}
	if err := r.host.supply(ctx, object, page, frames); err != nil {
		return err
	}
	last := page + uint64(len(frames))
	if p.fault >= page && p.fault < last {
		// The faulting page's own read request is answered by its supply.
		p.request.answer(nil)
	}
	// The supply gave the object's lock back, and the binding below takes it
	// again. Between the two another region may look at the root, and finds
	// a frame read here held, which it leaves to its own fault; a prefetch
	// that lands the same pages finds them supplied, and drops its own; and
	// no idle drop takes a frame whose lock the plan holds. In a controlled
	// run another task may go on here. A cancelled one still binds what it
	// supplied, so the plan gives every frame's lock back.
	admitted := sim.Admit(ctx, "vmmemory/supplied")
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
				r.host.adoptLocked(found)
				h.mu.Unlock()
			}
		} else if r.reachable(found) && p.hold(found) {
			hits++
		} else {
			p.fresh[i] = false
			continue
		}
		r.host.node.PageQueues().MarkAccessed(found)
		// A store's own page is bound too, unmapped: it is what the store
		// copies from, and the binding keeps it from an idle drop until the
		// copy replaces it.
		p.pages[i], p.fresh[i] = found, true
		r.bind(q, found)
	}
	lock.Unlock()
	if hits > 0 {
		// A count, which decides nothing.
		h.mu.Lock()
		h.stats.IdentityHits += hits
		h.mu.Unlock()
	}
	return admitted
}

// install maps every planned page and resolves the faulting one. Consecutive
// slots and explicit zero ranges coalesce into runs, sent in bounded batches.
// The commands are issued with no object lock held: a plan holds none, its
// pages being bound to the region already. The region's own Dirty pages are
// mapped writable, in runs of their own.
//
// Whether the region maps a page is looked up, recorded and commanded under
// holds of bindingsMu of their own, one after another. The plan holds the
// window's stripe and the region throughout, so no fault, populate or
// checkpoint of the region maps or unmaps a page of the window between them,
// and no eviction or idle drop takes a page the plan holds. h.mu is taken
// only to count.
func (p *plan) install(ctx context.Context) (bool, error) {
	r := p.region
	h := r.host
	var runs, writable []MapRun
	// Whether the region's own page is mapped writable is decided now, with
	// the page held: a seal taken while the plan read gave the region up
	// makes it the checkpoint's, read-only.
	for i, pg := range p.pages {
		if pg != nil && frameOf(pg).layer == r {
			p.writable[i] = r.writable(p.start + uint64(i))
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
		if r.mapped(page) {
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
			if !p.fresh[i+run] || page+run == p.store || r.mapped(page+run) || p.zeros[i+run] != zero ||
				p.writable[i+run] != p.writable[i] ||
				(!zero && (next == nil || frameOf(next).fileSlot != at.plus(int(run)))) {
				break
			}
			run++
		}
		switch {
		case zero:
			runs = append(runs, MapRun{Page: page, Count: int(run), Zero: true})
		case p.writable[i]:
			writable = append(writable, r.runAt(page, at, int(run)))
		default:
			runs = append(runs, r.runAt(page, at, int(run)))
		}
		h.mu.Lock()
		h.stats.MappedPages += run
		h.mu.Unlock()
		page += run
	}
	// Each run is recorded mapped as its command lands (mapper.go), the
	// read-only and zero runs first: a refusal leaves the rest unsent and
	// unrecorded.
	if len(runs) > 0 {
		commands, mappingRuns, err := r.mapReadOnly(ctx, runs)
		if err != nil {
			return false, err
		}
		h.mu.Lock()
		h.stats.Mappings += uint64(commands)
		h.stats.MappingRuns += uint64(mappingRuns)
		h.mu.Unlock()
		p.installed.add(installedRuns{commands: uint64(commands), runs: uint64(mappingRuns), pages: pagesOf(runs)})
	}
	for _, run := range writable {
		if err := r.mapRun(ctx, run, true); err != nil {
			return false, err
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
	if !r.mapped(p.fault) {
		return false, nil
	}
	if err := r.resolvePages(ctx, p.fault, 1, p.writable[p.fault-p.start]); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}
