package vmmemory

import (
	"context"
	"sort"
	"strings"

	"github.com/semistrict/sproutfs/internal/control"
)

// populationRuns bounds the mapping runs one Populate installs before the guest
// runs, and so what a restore pays for a sibling's residency.
//
// A run costs one mapping command, and a command is what the VMM answers with
// an mmap of the arena, a UFFD registration, a write-protect and an mremap
// whose REMAP event this pager has to read back — about a fifth of a
// millisecond, spent whether or not the guest ever reads the pages. A page the
// populate leaves alone costs at most a share of one command: the fault that
// reaches it maps its whole read-ahead window from the same resident pages, one
// command for the window and only for a window the guest actually touches.
//
// Measured on 2026-09-22 on GCE, a warm restore of a 16 GiB guest beside a
// sibling holding 2,930,747 of its pages mapped them in 21,698 runs before the
// guest ran — 5.1 s of VMM start against a half-second bound — and the guest
// then took 723 faults. So an eager populate is a bet on the guest reading what
// the sibling holds, and this is what that bet may lose: 128 commands, about a
// fortieth of the restore's budget.
//
// It bounds every run a populate installs, of whatever kind. A hole is one run
// however many pages it covers, but a guest's address space is holes all through
// it rather than one: on 2026-09-23, with the resident runs already bounded, a
// warm restore still installed 14,447 runs over 2,166,194 pages of which only
// 123,056 were resident identities, so two million pages of scattered holes were
// paying a command each. A run of pages a fork point named is in the budget too,
// and has the first claim on it: nothing but this populate can share one, so it
// is worth more than a hole or a published run, but it is not worth an unbounded
// number of commands before the guest runs.
//
// It is a variable only so a test can observe the bound without a region of
// production size.
var populationRuns = 128

// populationPages bounds the pages one Populate installs, because a run's cost
// is not only its command: the kernel installs the run's pages one by one, a
// write-protected entry each, at about a microsecond a page. Measured on
// 2026-09-23 on GCE, a warm restore's populate of 839,196 pages in 128 runs
// took 1.10 s against a half-second bound for the whole restore, and a fork's
// took 2.0–2.3 s over 2.03 M pages. Sixteen thousand pages — 64 MiB at 4 KiB,
// 32 GiB at 2 MiB, where the cost is per run rather than per page — is about
// twenty milliseconds, and what it does not install the faults' windows do, a
// window per touch. A run of pages a fork point named is charged like any
// other, so an attach's cost is bounded whatever a point names.
//
// It is a variable only so a test can observe the bound without a region of
// production size.
var populationPages uint64 = 16 << 10

// populationRun is the shortest run of pages Populate installs: consecutive in
// this region and resident in consecutive arena slots, which is what one
// mapping command covers. It is the read-ahead window, because a run shorter
// than one saves at most the single fault that would have mapped the same pages
// with the same single command — and only if the guest reads them at all. A
// region smaller than the read-ahead run is one window, so that is its length.
func (r *Region) populationRun() int { return max(min(r.readAheadPages, r.pageCount), 1) }

// PopulateStats is what one attach's populate installed before the guest ran:
// the mapping commands it issued, the runs those commands covered and the pages
// in them, and how long it took. It is the region's own rather than the pager's,
// because an attach is per region and a host brings several up at once — and it
// is what says whether a restore's wait was the populate at all. A command is
// what the VMM answers with an mmap of the arena, a UFFD registration, a
// write-protect and an mremap whose REMAP event this pager reads back, so
// commands and not pages are what a populate costs.
type PopulateStats struct {
	Commands, Runs, Pages uint64
	DurationNS            int64
}

// Populated is what this region's populate came to, zero until it has run.
func (r *Region) Populated() PopulateStats {
	if stats := r.populated.Load(); stats != nil {
		return *stats
	}
	return PopulateStats{}
}

// Populate maps the pages of the region that are already resident under their
// stored identity, so a restored or forked machine starts with the pages its
// siblings loaded and takes no faults on them. It loads nothing, and it installs
// at most populationRuns runs of them. Call it once the mapping accepts commands
// and before memory users start.
func (r *Region) Populate(ctx context.Context) error {
	h := r.host
	started := h.clock.Now()
	var installed installedRuns
	defer func() {
		r.populated.Store(&PopulateStats{Commands: installed.commands, Runs: installed.runs,
			Pages: installed.pages, DurationNS: h.clock.Since(started).Nanoseconds()})
	}()
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		r.mu.Unlock()
		return err
	}
	index := h.residentIndex(nil)
	h.mu.Lock()
	available := len(index.present) > 0 || h.zeroRegions > 0
	h.mu.Unlock()
	r.mu.Unlock()
	if !available {
		// There is no shared backing to populate. A first fault will discover
		// cold data or zeros without making attachment wait for metadata.
		return nil
	}
	// The budget is the whole region's, spent in page order. A run that straddles
	// two of the windows below is two runs to this walk and may fall under the
	// length the budget asks for; that costs a fault the populate could have
	// saved, never a page.
	//
	// The walk ends where the budget does. Each window asks the volume for the
	// identity of every page in it — four million of them for a 16 GiB guest at a
	// 4 KiB page, decoded out of the index's segments — and a window reached with
	// nothing left to spend can install no run of any kind, so every one of those
	// answers would be metadata read before the guest runs for nothing at all.
	budget := populationBudget{runs: populationRuns, pages: populationPages}
	for start := uint64(0); start < uint64(r.pageCount) && budget.left(); {
		end := min(start+max(populationWindowBytes/h.pageSize, 1), uint64(r.pageCount))
		err := func() error {
			if err := r.mu.Lock(ctx); err != nil {
				return err
			}
			defer r.mu.Unlock()
			if err := r.ready(); err != nil {
				return err
			}
			if err := h.beginIO(ctx); err != nil {
				return err
			}
			defer h.endIO()
			plan, err := r.plan(ctx, start, end, end)
			if err != nil {
				return err
			}
			defer plan.unlock()
			index = h.residentIndex(index)
			if err := plan.bindResidents(ctx, index, &budget); err != nil {
				return err
			}
			_, err = plan.install(ctx)
			installed.add(plan.installed)
			return err
		}()
		if err != nil {
			return err
		}
		start = end
	}
	return nil
}

// residentIndex is a bounded checkpoint of the shared identities present after
// a metadata lookup: a located extent then selects matching resident pages without
// probing the host index once per logical page. Binding rechecks each identity
// under its resident lock, so eviction cannot turn a candidate into stale data.
type residentIndex struct {
	version uint64
	present map[control.Identity]bool
}

func (h *Host) residentIndex(previous *residentIndex) *residentIndex {
	h.mu.Lock()
	defer h.mu.Unlock()
	if previous != nil && previous.version == h.cleanVersion {
		return previous
	}
	index := &residentIndex{version: h.cleanVersion, present: make(map[control.Identity]bool, len(h.clean))}
	for key := range h.clean {
		index.present[key.id] = true
	}
	return index
}

// identityLess is a total order over every field of an identity, so opposing
// attachments of related images acquire resident locks in one global order.
func identityLess(a, b control.Identity) bool {
	if a.Zero != b.Zero {
		return !a.Zero
	}
	if a.Ref.VM != b.Ref.VM {
		return a.Ref.VM < b.Ref.VM
	}
	if a.Ref.Sequence != b.Ref.Sequence {
		return a.Ref.Sequence < b.Ref.Sequence
	}
	if a.Volume != b.Volume {
		return strings.Compare(a.Volume, b.Volume) < 0
	}
	return a.Page < b.Page
}

// candidate is one page of a populate: the page and the resident identity it
// would bind to. They are collected in page order, which is the order the runs
// one mapping command covers are found in.
type candidate struct {
	page uint64
	key  pageKey
}

// populateRun is one stretch of pages a populate would install with a single
// mapping command: pages consecutive in the region that are all explicit zeros,
// or all bound to resident pages in consecutive arena slots. For a resident run,
// from and to bound the candidates it is made of.
type populateRun struct {
	first, last uint64
	from, to    int
	zero        bool
	// named marks a run of private pages a fork point named — the parent's own
	// dirty state, shared under a name that ending the seal takes back. This
	// populate is the only moment a child can map one, and a fault arriving later
	// reads the bytes back out of the child's own first checkpoint, so a named
	// run takes the budget before any other.
	named bool
}

// residentRuns groups the candidates into the runs one mapping command each
// covers, and appends them to runs.
//
// The slots are read once, without taking any page's lock. Nothing here decides
// what a page holds: an eviction or a publication between this reading and the
// binding below can only make install send a kept run as two, or bind one page
// fewer, which costs a command and a fault and never a page.
func (p *windowPlan) residentRuns(candidates []candidate, runs []populateRun) []populateRun {
	h := p.region.host
	slots := make([]int, len(candidates))
	named := make([]bool, len(candidates))
	h.mu.Lock()
	for i, item := range candidates {
		slots[i] = -1
		if pg := h.clean[item.key]; pg != nil {
			slots[i], named[i] = pg.slot, pg.private
		}
	}
	h.mu.Unlock()
	for first := 0; first < len(candidates); first++ {
		if slots[first] < 0 {
			continue
		}
		last := first + 1
		for last < len(candidates) && named[last] == named[first] &&
			slots[last] == slots[last-1]+1 && candidates[last].page == candidates[last-1].page+1 {
			last++
		}
		runs = append(runs, populateRun{first: candidates[first].page, last: candidates[last-1].page + 1,
			from: first, to: last, named: named[first]})
		first = last - 1
	}
	return runs
}

// afford spends the populate's run budget on the runs worth a mapping command,
// and reports them in page order. The runs a fork point named go first, whatever
// their length; every other run must cover at least one read-ahead window,
// because a shorter one saves at most the single fault that would have mapped
// the same pages with the same single command. Within each of those two the
// budget is spent in page order.
// populationBudget is what one Populate may still spend: commands, and the
// pages the kernel installs for them.
type populationBudget struct {
	runs  int
	pages uint64
}

func (b *populationBudget) left() bool { return b.runs > 0 && b.pages > 0 }

// spend takes a run out of the budget, cut down to the pages left when it is
// longer than they are, and reports what was afforded. A sibling's residency is
// often one run for most of a region, so refusing a long run outright would
// leave the budget unspent; the front of it is worth the same command, and the
// fault that reaches the rest maps its window from the same pages.
func (b *populationBudget) spend(run populateRun) (populateRun, bool) {
	if !b.left() {
		return run, false
	}
	if run.last-run.first > b.pages {
		run.last = run.first + b.pages
		if !run.zero {
			// A resident run's candidates are one per page, in page order.
			run.to = run.from + int(b.pages)
		}
	}
	b.runs--
	b.pages -= run.last - run.first
	return run, true
}

func (p *windowPlan) afford(runs []populateRun, budget *populationBudget) []populateRun {
	sort.Slice(runs, func(i, j int) bool { return runs[i].first < runs[j].first })
	least := uint64(p.region.populationRun())
	kept := runs[:0:0]
	for _, named := range [2]bool{true, false} {
		for _, run := range runs {
			if run.named != named || !budget.left() {
				continue
			}
			if !named && run.last-run.first < least {
				continue
			}
			run, ok := budget.spend(run)
			if !ok {
				continue
			}
			kept = append(kept, run)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].first < kept[j].first })
	return kept
}

func (p *windowPlan) bindResidents(ctx context.Context, index *residentIndex, budget *populationBudget) error {
	var candidates []candidate
	var runs []populateRun
	ps := p.region.host.pageSize
	for _, extent := range p.extents {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if extent.Identity.Zero {
			// A hole is one run, however large: it owns no arena slot and needs
			// no per-page identity lookup. It is observed whether or not this
			// populate can afford to map it, because that is what lets a sibling
			// attachment find holes without reading metadata of its own.
			first := max((extent.Offset+ps-1)/ps, p.start)
			last := min((extent.Offset+extent.Length)/ps, p.end)
			if first < last {
				p.observeZeros()
				runs = append(runs, populateRun{first: first, last: last, zero: true})
			}
			continue
		}
		if extent.Identity.Ref.IsZero() || extent.Length < ps || !index.present[extent.Identity] {
			continue
		}
		page := extent.Offset / ps
		if extent.Identity.Page != page || page < p.start || page >= p.end || !p.eligible(page) {
			continue
		}
		candidates = append(candidates, candidate{page: page, key: pageKey{id: extent.Identity}})
	}
	var kept []candidate
	for _, run := range p.afford(p.residentRuns(candidates, runs), budget) {
		if run.zero {
			p.markZeros(run.first, run.last)
			continue
		}
		kept = append(kept, candidates[run.from:run.to]...)
	}
	// Every population takes resident locks in the same immutable identity
	// order. Logical page order may differ between related images; using it
	// would deadlock opposing attachments once the host-wide queue is removed.
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].key == kept[j].key {
			return kept[i].page < kept[j].page
		}
		return identityLess(kept[i].key.id, kept[j].key.id)
	})
	for _, item := range kept {
		if err := p.bindShared(ctx, item.page, true); err != nil {
			return err
		}
	}
	return nil
}
