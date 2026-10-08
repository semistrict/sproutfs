package vmmemory

import (
	"context"
	"errors"
	"sort"

	"github.com/semistrict/sproutfs/platform/sim"
)

const revokeBatchPages = 1024

// errVictimHeld reports an eviction that could not take one resident page away
// from every binding it is reachable from, because one of those bindings belongs
// to a memory region that can no longer take mapping commands. The page stays mapped
// there, so it is not this host's to reuse, and the memory region it could not be
// taken from is terminal from here — which is what keeps the reclaim from
// choosing that page again. It never reaches a caller: an allocation that
// meets it takes another victim.
var errVictimHeld = errors.New("vmmemory: a resident page's other holder cannot give it up")

// revocationFailed makes a failed revocation terminal, as every one of them is:
// the pages stay recorded as mapped, which is what keeps the page the guest
// may still read through reachable, and the memory region can no longer take mappings
// away. A revocation the client refused is terminal too, and deliberately not
// the refusal a fault is served again for: what a fault waits for is a
// revocation, and this is one that could not happen.
func (r *MemoryRegion) revocationFailed(err error) error {
	if errors.Is(err, ErrMappingRefused) {
		err = errors.New("managed-memory revocation refused: " + err.Error())
	}
	return r.fail(err)
}

// underProtection runs one revocation with the memory region's protection held
// shared, so that a seal's write-protect commands and the mappings a reclaim
// takes away cannot overlap. It is never nested and never waits for the memory region
// or for a page while it holds it.
func (r *MemoryRegion) underProtection(ctx context.Context, revoke func() error) error {
	if err := r.protectMu.RLock(ctx); err != nil {
		return err
	}
	defer r.protectMu.RUnlock()
	return revoke()
}

// A revocation records its pages unmapped once its command has landed, and
// leaves them recorded mapped where it failed, which is terminal: it is the
// other half of the mapper (mapper.go).

// Zircon's Unmap and UnmapAndHarvest are RevokeBatch here, and each command is
// issued with the region's protection held shared, so a seal's write-protect
// commands and a revocation never overlap.

// revokeBindings revokes the mapped pages of bindings, every one this
// region's, by one command per run of consecutive pages where the client takes
// batches. An error is ambiguous for every run, so the pages stay recorded as
// mapped and the region is terminal.
func (r *MemoryRegion) revokeBindings(ctx context.Context, bindings []*binding) error {
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
		// Checked once every binding is recorded: a page's guest binding and
		// the checkpoint's copy of it share an index.
		for _, run := range runs {
			r.agreeRecorded(run.Page, run.Page+uint64(run.Count))
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

// revoke takes one page's mapping away, where it is installed.
func (r *MemoryRegion) revoke(ctx context.Context, b *binding) error {
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
		r.agreeRecorded(b.index, b.index+1)
		h.mu.Lock()
		h.stats.Revocations++
		h.stats.RevokeRuns++
		h.stats.RevokedPages++
		h.revokedLocked()
		h.mu.Unlock()
		return nil
	})
}

// refusedWritable is what a store does when the command that maps its pages
// [first, last) writable was refused, the bindings already the region's own
// writable pages: a copy, a page dirtied in place, a protect trap's page, or
// fresh zeros. The guest still maps what it read there, read-only, which the
// bindings can no longer say, so those mappings are taken away and recorded
// gone, and the guest's next access faults and maps its own page. A copy's
// store does the same through its replacement. Where err is no refusal the
// mapper has ended the region, and it is returned as it is.
func (r *MemoryRegion) refusedWritable(ctx context.Context, err error, first, last uint64) error {
	if !errors.Is(err, ErrMappingRefused) || sim.Bug(ctx, "pager-keep-a-refused-store-s-old-mapping") {
		return err
	}
	var bindings []*binding
	r.bindingsMu.Lock()
	r.eachBoundLocked(first, last, func(b *binding) { bindings = append(bindings, b) })
	r.bindingsMu.Unlock()
	if revoked := r.revokeBindings(ctx, bindings); revoked != nil {
		return revoked
	}
	return err
}
