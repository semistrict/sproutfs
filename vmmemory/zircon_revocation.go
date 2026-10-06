package vmmemory

import (
	"context"
	"sort"
)

// Revocations over the zircon core, as revocation.go issues them for the
// current core: Zircon's Unmap and UnmapAndHarvest are RevokeBatch here, and
// each command is issued with the region's protection held shared, so a
// seal's write-protect commands and a revocation never overlap.

// lookupBinding is the binding of a page that has one, nil otherwise.
func (r *MemoryRegion) lookupBinding(index uint64) *zbinding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b
}

// isMapped reports whether b's mapping is installed.
func (r *MemoryRegion) isMapped(b *zbinding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.mapped
}

// setBindingMapped records whether b's mapping is installed.
func (r *MemoryRegion) setBindingMapped(b *zbinding, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b.mapped = mapped
	r.noteSealableLocked(b)
}

// revokeBindings is MemoryRegion.revokeBindings over the zircon core: the
// mapped pages of bindings, every one this region's, revoked by one command
// per run of consecutive pages where the client takes batches. An error is
// ambiguous for every run, so the pages stay recorded as mapped and the
// region is terminal.
func (r *MemoryRegion) revokeBindings(ctx context.Context, bindings []*zbinding) error {
	h := r.host
	batch, ok := r.mapping.(BatchRevocation)
	if !ok {
		for _, b := range bindings {
			if err := r.revoke(ctx, b); err != nil {
				return err
			}
		}
		return nil
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].index < bindings[j].index })
	var runs []PageRun
	pages := 0
	for _, b := range bindings {
		if !r.isMapped(b) {
			continue
		}
		pages++
		if len(runs) > 0 && runs[len(runs)-1].Page+uint64(runs[len(runs)-1].Count) == b.index {
			runs[len(runs)-1].Count++
		} else {
			runs = append(runs, PageRun{Page: b.index, Count: 1})
		}
	}
	if len(runs) == 0 {
		return nil
	}
	return r.underProtection(ctx, func() error {
		commands, count, err := r.revokeBatch(ctx, batch, runs)
		if err != nil {
			return r.revocationFailed(err)
		}
		for _, b := range bindings {
			r.setBindingMapped(b, false)
		}
		h.mu.Lock()
		h.stats.Revocations += uint64(commands)
		h.stats.RevokeRuns += uint64(count)
		h.stats.RevokedPages += uint64(pages)
		h.revokedLocked()
		h.mu.Unlock()
		return nil
	})
}

// revoke is Host.revoke over the zircon core: one page's mapping, where it is
// installed.
func (r *MemoryRegion) revoke(ctx context.Context, b *zbinding) error {
	h := r.host
	if !r.isMapped(b) {
		return nil
	}
	return r.underProtection(ctx, func() error {
		if !r.isMapped(b) {
			return nil
		}
		if err := r.revokePage(ctx, b.index); err != nil {
			return r.revocationFailed(err)
		}
		r.setBindingMapped(b, false)
		h.mu.Lock()
		h.stats.Revocations++
		h.stats.RevokeRuns++
		h.stats.RevokedPages++
		h.revokedLocked()
		h.mu.Unlock()
		return nil
	})
}
