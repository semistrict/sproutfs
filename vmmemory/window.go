package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
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
