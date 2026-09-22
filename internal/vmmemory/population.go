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
// It bounds the runs of published identities alone. Explicit zeros are not
// counted against it: a hole is one run however many pages it covers and owns
// no arena slot, so mapping it eagerly costs one command for a whole sparse
// span. Nor are the private pages a fork point names, which nothing but this
// populate can share and which the parent's dirty budget already bounds.
//
// It is a variable only so a test can observe the bound without a region of
// production size.
var populationRuns = 128

// populationRun is the shortest run of pages Populate installs: consecutive in
// this region and resident in consecutive arena slots, which is what one
// mapping command covers. It is the read-ahead window, because a run shorter
// than one saves at most the single fault that would have mapped the same pages
// with the same single command — and only if the guest reads them at all. A
// region smaller than the read-ahead run is one window, so that is its length.
func (r *Region) populationRun() int { return max(min(r.readAheadPages, r.pageCount), 1) }

// Populate maps the pages of the region that are already resident under their
// stored identity, so a restored or forked machine starts with the pages its
// siblings loaded and takes no faults on them. It loads nothing, and it installs
// at most populationRuns runs of them. Call it once the mapping accepts commands
// and before memory users start.
func (r *Region) Populate(ctx context.Context) error {
	h := r.host
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
	budget := populationRuns
	for start := uint64(0); start < uint64(r.pageCount); {
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

// affordable drops the candidates whose run is not worth the mapping command it
// would cost, and stops once the populate's run budget is spent. A run is the
// candidates consecutive in the region and resident in consecutive arena slots,
// which is exactly what install sends as one run.
//
// A private page a seal named is never dropped. It is the fork point's: the
// parent's own dirty state, shared under a name that belongs to the point and
// that ending the seal takes back, so this populate is the only moment a child
// can map it and a fault that came later would read the bytes back out of the
// child's own first checkpoint. There is no budget to keep for it either — the
// set is exactly what the parent held dirty, which its dirty budget bounds.
//
// The slots are read once, without taking any page's lock. Nothing here decides
// what a page holds: an eviction or a publication between this reading and the
// binding below can only make install send a kept run as two, or bind one page
// fewer, which costs a command and a fault and never a page.
func (p *windowPlan) affordable(candidates []candidate, budget *int) []candidate {
	h := p.region.host
	slots := make([]int, len(candidates))
	private := make([]bool, len(candidates))
	h.mu.Lock()
	for i, item := range candidates {
		slots[i] = -1
		if pg := h.clean[item.key]; pg != nil {
			slots[i], private[i] = pg.slot, pg.private
		}
	}
	h.mu.Unlock()
	least := p.region.populationRun()
	kept := candidates[:0:0]
	for first := 0; first < len(candidates); first++ {
		if slots[first] < 0 {
			continue
		}
		last := first + 1
		for last < len(candidates) && private[last] == private[first] &&
			slots[last] == slots[last-1]+1 && candidates[last].page == candidates[last-1].page+1 {
			last++
		}
		switch {
		case private[first]:
			kept = append(kept, candidates[first:last]...)
		case last-first >= least && *budget > 0:
			*budget--
			kept = append(kept, candidates[first:last]...)
		}
		first = last - 1
	}
	return kept
}

func (p *windowPlan) bindResidents(ctx context.Context, index *residentIndex, budget *int) error {
	var candidates []candidate
	ps := p.region.host.pageSize
	for _, extent := range p.extents {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if extent.Identity.Zero {
			// A hole is one range, however large: it owns no arena slot and
			// needs no per-page identity lookup.
			first := (extent.Offset + ps - 1) / ps
			p.markZeros(max(first, p.start), min((extent.Offset+extent.Length)/ps, p.end))
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
	candidates = p.affordable(candidates, budget)
	// Every population takes resident locks in the same immutable identity
	// order. Logical page order may differ between related images; using it
	// would deadlock opposing attachments once the host-wide queue is removed.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].key == candidates[j].key {
			return candidates[i].page < candidates[j].page
		}
		return identityLess(candidates[i].key.id, candidates[j].key.id)
	})
	for _, item := range candidates {
		if err := p.bindShared(ctx, item.page, true); err != nil {
			return err
		}
	}
	return nil
}
