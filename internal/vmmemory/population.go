package vmmemory

import (
	"context"
	"sort"
	"strings"

	"github.com/semistrict/sproutfs/internal/control"
)

// Populate maps every page of the region that is already resident under its
// stored identity, so a restored or forked machine starts with the pages its
// siblings loaded and takes no faults on them. It loads nothing. Call it once
// the mapping accepts commands and before memory users start.
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
	for start := uint64(0); start < uint64(r.pageCount); {
		end := min(start+uint64(populationWindowBytes/PageSize), uint64(r.pageCount))
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
			if err := plan.bindResidents(ctx, index); err != nil {
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
// a metadata lookup: a located extent then selects matching frames without
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

func (p *windowPlan) bindResidents(ctx context.Context, index *residentIndex) error {
	type candidate struct {
		page uint64
		key  frame
	}
	var candidates []candidate
	ps := uint64(PageSize)
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
		candidates = append(candidates, candidate{page: page, key: frame{id: extent.Identity}})
	}
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
