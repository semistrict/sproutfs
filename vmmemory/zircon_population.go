package vmmemory

import (
	"context"
	"sort"
)

// bindResidents is windowPlan.bindResidents over the identity roots: the
// window's holes and its pages a root holds, grouped into the runs one
// mapping command covers each, as many of them as the budget affords, taken
// into the plan.
func (p *zplan) bindResidents(ctx context.Context, budget *populationBudget) error {
	r := p.region
	h := r.host
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
	slots, named := r.host.residentSlots(candidates)
	var kept []candidate
	for _, run := range affordRuns(groupResidentRuns(candidates, slots, named, runs), budget, uint64(r.populationRun())) {
		if run.zero {
			p.markZeros(run.first, run.last)
			continue
		}
		kept = append(kept, candidates[run.from:run.to]...)
	}
	// Every population takes the pages' locks in the same order of
	// identities, as the current core's does.
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].key == kept[j].key {
			return kept[i].page < kept[j].page
		}
		return identityLess(kept[i].key.id, kept[j].key.id)
	})
	for _, item := range kept {
		if _, err := p.take(ctx, item.page, item.key); err != nil {
			return err
		}
	}
	return nil
}

// residentSlots is the slot of the page each candidate's root holds, or slot
// -1, read once with each root's lock held for each run of candidates of one
// root, and whether a fork point lends it: the parent's own dirty state, which
// this populate is the only moment a child can map, so its runs take the
// budget first. Nothing here decides what a page holds: a page given up
// before it is taken is a command and a fault more, never a page.
func (h *Host) residentSlots(candidates []candidate) ([]fileSlot, []bool) {
	ps := h.pageSize
	slots := make([]fileSlot, len(candidates))
	named := make([]bool, len(candidates))
	for at := 0; at < len(candidates); {
		key := rootOf(candidates[at].key)
		h.mu.Lock()
		root := h.roots[key]
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
			h.mu.Lock()
			lent := root.lent != nil
			h.mu.Unlock()
			for i := at; i < at+run; i++ {
				named[i] = lent && slots[i].slot >= 0
			}
		}
		at += run
	}
	return slots, named
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

// lendsAnyLocked reports a fork point lending pages under a temporary
// identity root. Caller holds h.mu.
func (h *Host) lendsAnyLocked() bool {
	for _, root := range h.roots {
		if len(root.lentPages) > 0 {
			return true
		}
	}
	return false
}
