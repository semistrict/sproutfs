package vmmemory

import (
	"context"
)

// populate is MemoryRegion.Populate over the zircon core: it maps the pages
// of the region its identity roots already hold, so a restored or forked
// machine starts with the pages its siblings read. It is a commit of the
// range that reads nothing, Zircon's CommitRangeLocked restricted to the
// pages present in a root, and it spends the current core's budget
// (population.go) as the current core does.
func (z *zirconRegion) populate(ctx context.Context) error {
	r := z.region
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
	h.mu.Lock()
	available := z.host.rootPages > 0 || h.zeroMemoryRegions > 0
	h.mu.Unlock()
	r.mu.Unlock()
	if !available {
		// There is no page to populate from. A first fault will discover
		// cold data or zeros without making attachment wait for metadata.
		return nil
	}
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
			plan, err := z.plan(ctx, start, end, end)
			if err != nil {
				return err
			}
			defer plan.unlock()
			if err := plan.bindResidents(ctx, &budget); err != nil {
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

// bindResidents is windowPlan.bindResidents over the identity roots: the
// window's holes and its pages a root holds, grouped into the runs one
// mapping command covers each, as many of them as the budget affords, taken
// into the plan.
func (p *zplan) bindResidents(ctx context.Context, budget *populationBudget) error {
	z := p.z
	h := z.region.host
	ps := h.pageSize
	var candidates []candidate
	var runs []populateRun
	for _, extent := range p.window.extents {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if extent.Identity.Zero {
			// A hole is one run, however large, observed whether or not the
			// budget affords mapping it.
			first := max((extent.Offset+ps-1)/ps, p.start)
			last := min((extent.Offset+extent.Length)/ps, p.end)
			if first < last {
				p.observeZeros()
				runs = append(runs, populateRun{first: first, last: last, zero: true})
			}
			continue
		}
		if extent.Identity.Ref.IsZero() || extent.Length < ps {
			continue
		}
		page := extent.Offset / ps
		if extent.Identity.Page != page || page < p.start || page >= p.end || !p.eligible(page) {
			continue
		}
		candidates = append(candidates, candidate{page: page, key: pageKey{id: extent.Identity}})
	}
	slots := z.host.residentSlots(candidates)
	var kept []candidate
	for _, run := range affordRuns(groupResidentRuns(candidates, slots, nil, runs), budget, uint64(z.region.populationRun())) {
		if run.zero {
			p.markZeros(run.first, run.last)
			continue
		}
		kept = append(kept, candidates[run.from:run.to]...)
	}
	for _, item := range kept {
		p.take(item.page, item.key)
	}
	return nil
}

// residentSlots is the slot of the page each candidate's root holds, or slot
// -1, read once with each root's lock held for each run of candidates of one
// root. Nothing here decides what a page holds: a page given up before it is
// taken is a command and a fault more, never a page.
func (z *zirconHost) residentSlots(candidates []candidate) []fileSlot {
	ps := z.host.pageSize
	slots := make([]fileSlot, len(candidates))
	for at := 0; at < len(candidates); {
		key := rootOf(candidates[at].key)
		h := z.host
		h.mu.Lock()
		root := z.roots[key]
		h.mu.Unlock()
		run := 1
		for at+run < len(candidates) && rootOf(candidates[at+run].key) == key {
			run++
		}
		for i := at; i < at+run; i++ {
			slots[i] = fileSlot{slot: -1}
		}
		if root != nil {
			root.slotsOf(candidates[at:at+run], slots[at:at+run], ps)
		}
		at += run
	}
	return slots
}

// slotsOf sets the slot of the page the root holds for each candidate, all of
// this root, under one hold of its lock, leaving the slot of a page it does
// not hold as it is.
func (root *identityRoot) slotsOf(candidates []candidate, slots []fileSlot, pageSize uint64) {
	lock := root.pages.Lock()
	lock.Lock()
	defer lock.Unlock()
	for i, item := range candidates {
		if found := root.pages.PageLocked(item.key.id.Page * pageSize); found != nil {
			slots[i] = frameOf(found).fileSlot
		}
	}
}
