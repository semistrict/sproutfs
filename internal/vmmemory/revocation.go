package vmmemory

import (
	"context"
	"errors"
	"sort"
)

const revokeBatchPages = 1024

// errVictimHeld reports an eviction that could not take one resident page away
// from every binding it is reachable from, because one of those bindings belongs
// to a region that can no longer take mapping commands. The page stays mapped
// there, so it is not this host's to reuse, and the region it could not be
// taken from is terminal from here — which is what keeps the reclaim from
// choosing that page again. It never reaches a caller: an allocation that
// meets it takes another victim.
var errVictimHeld = errors.New("vmmemory: a resident page's other holder cannot give it up")

// revocationFailed makes a failed revocation terminal, as every one of them is:
// the pages stay recorded as mapped, which is what keeps the page the guest
// may still read through reachable, and the region can no longer take mappings
// away. A revocation the client refused is terminal too, and deliberately not
// the refusal a fault is served again for: what a fault waits for is a
// revocation, and this is one that could not happen.
func (r *Region) revocationFailed(err error) error {
	if errors.Is(err, ErrMappingRefused) {
		err = errors.New("managed-memory revocation refused: " + err.Error())
	}
	return r.fail(err)
}

// revokeLocked revokes a bounded set of this region's own bindings, holding
// each one's current resident page across its revoke so no reclaim can change the
// mapping underneath it. Caller owns the region exclusively.
func (r *Region) revokeLocked(ctx context.Context, bindings []*binding) error {
	for len(bindings) > 0 {
		count := min(len(bindings), revokeBatchPages)
		var locked []*resident
		err := func() error {
			defer func() { r.host.unlockAll(locked) }()
			for _, b := range bindings[:count] {
				pg, err := r.host.current(ctx, b)
				if err != nil {
					return err
				}
				if pg != nil {
					locked = append(locked, pg)
				}
			}
			return r.revokeBindings(ctx, bindings[:count])
		}()
		if err != nil {
			return err
		}
		bindings = bindings[count:]
	}
	return nil
}

// Callers own all resident transitions (or an unmapped binding's region lock).
// Preserve possibly mapped state until the whole batch has a successful ACK.
func (r *Region) revokeBindings(ctx context.Context, bindings []*binding) error {
	batch, ok := r.mapping.(BatchRevocation)
	if !ok {
		for _, b := range bindings {
			if err := r.host.revoke(ctx, b); err != nil {
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
	commands, count, err := r.revokeBatch(ctx, batch, runs)
	if err != nil {
		return r.revocationFailed(err)
	}
	for _, b := range bindings {
		r.setMapped(b, false)
	}
	r.host.mu.Lock()
	r.host.stats.Revocations += uint64(commands)
	r.host.stats.RevokeRuns += uint64(count)
	r.host.stats.RevokedPages += uint64(pages)
	r.host.mu.Unlock()
	return nil
}

func (h *Host) revoke(ctx context.Context, b *binding) error {
	// A reclaim revokes its victim's pages under that page's lock alone and a
	// seal reads them under the region, so the two do not exclude each other:
	// the mapping state is read through the binding map, like every other
	// holder of it.
	if !b.region.isMapped(b) {
		return nil
	}
	if err := b.region.revokePage(ctx, b.index); err != nil {
		return b.region.revocationFailed(err)
	}
	b.region.setMapped(b, false)
	h.mu.Lock()
	h.stats.Revocations++
	h.stats.RevokeRuns++
	h.stats.RevokedPages++
	h.mu.Unlock()
	return nil
}
