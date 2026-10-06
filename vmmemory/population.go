package vmmemory

import (
	"context"
	"sort"
	"strings"

	"github.com/semistrict/sproutfs/control"
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
// It is a variable only so a test can observe the bound without a memory region of
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
// It is a variable only so a test can observe the bound without a memory region of
// production size.
var populationPages uint64 = 16 << 10

// populationRun is the shortest run of pages Populate installs: consecutive in
// this memory region and resident in consecutive arena slots, which is what one
// mapping command covers. It is the read-ahead window, because a run shorter
// than one saves at most the single fault that would have mapped the same pages
// with the same single command — and only if the guest reads them at all. A
// memory region smaller than the read-ahead run is one window, so that is its length.
func (r *MemoryRegion) populationRun() int { return max(min(r.readAheadPages, r.pageCount), 1) }

// PopulateStats is what one attach's populate installed before the guest ran:
// the mapping commands it issued, the runs those commands covered and the pages
// in them, and how long it took. It is the memory region's own rather than the pager's,
// because an attach is per memory region and a host brings several up at once — and it
// is what says whether a restore's wait was the populate at all. A command is
// what the VMM answers with an mmap of the arena, a UFFD registration, a
// write-protect and an mremap whose REMAP event this pager reads back, so
// commands and not pages are what a populate costs.
type PopulateStats struct {
	Commands, Runs, Pages uint64
	DurationNS            int64
}

// Populated is what this memory region's populate came to, zero until it has run.
func (r *MemoryRegion) Populated() PopulateStats {
	if stats := r.populated.Load(); stats != nil {
		return *stats
	}
	return PopulateStats{}
}

// Populate maps the pages of the memory region that are already resident under their
// stored identity, so a restored or forked machine starts with the pages its
// siblings loaded and takes no faults on them. It loads nothing, and it installs
// at most populationRuns runs of them. Call it once the mapping accepts commands
// and before memory users start.
func (r *MemoryRegion) Populate(ctx context.Context) error {
	z := r.zircon

	return z.populate(ctx)
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
// mapping command: pages consecutive in the memory region that are all explicit zeros,
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

// groupResidentRuns groups candidates into the runs one mapping command each
// covers, given the slot of each candidate's resident page (slot -1 for one
// with none) and whether a fork point named it, nil for none named, and
// appends them to runs. Both cores' populates group their candidates so.
func groupResidentRuns(candidates []candidate, slots []fileSlot, named []bool, runs []populateRun) []populateRun {
	isNamed := func(i int) bool { return named != nil && named[i] }
	for first := 0; first < len(candidates); first++ {
		if slots[first].slot < 0 {
			continue
		}
		last := first + 1
		for last < len(candidates) && isNamed(last) == isNamed(first) &&
			slots[last] == slots[last-1].plus(1) && candidates[last].page == candidates[last-1].page+1 {
			last++
		}
		runs = append(runs, populateRun{first: candidates[first].page, last: candidates[last-1].page + 1,
			from: first, to: last, named: isNamed(first)})
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
// often one run for most of a memory region, so refusing a long run outright would
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

// affordRuns is afford for runs no shorter than least, which is the region's
// population run.
func affordRuns(runs []populateRun, budget *populationBudget, least uint64) []populateRun {
	sort.Slice(runs, func(i, j int) bool { return runs[i].first < runs[j].first })
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
