package vmmemory

import (
	"context"
)

// stats is Host.Stats under the zircon core: the pager's counters are the
// same ones, and the idle pages are the roots' pages no region maps.
func (z *zirconHost) stats(ctx context.Context) (Stats, error) {
	if err := context.Cause(ctx); err != nil {
		return Stats{}, err
	}
	return z.host.snapshot()
}

// sharing is Host.Sharing under the zircon core: every page an object holds,
// counted under the kind of the region that made it, once for the memory and
// once for each region that maps it.
func (z *zirconHost) sharing(ctx context.Context) (SharingStats, error) {
	if err := context.Cause(ctx); err != nil {
		return SharingStats{}, err
	}
	h := z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	var stats SharingStats
	for p := range z.node.PageQueues().Pages() {
		f := frameOf(p)
		gauge := &stats.Pmem
		if f.kind == Ram {
			gauge = &stats.Ram
		}
		aliases := uint64(f.aliases.len())
		gauge.UniqueBytes += h.pageSize
		gauge.MappedBytes += aliases * h.pageSize
		if aliases > 1 {
			gauge.SavedBytes += (aliases - 1) * h.pageSize
		}
	}
	return stats, h.err
}

// stats is MemoryRegion.Stats under the zircon core: the pages the region
// maps from an object, and of those the ones another region maps too.
func (z *zirconRegion) stats(ctx context.Context) (MemoryRegionStats, error) {
	r := z.region
	if err := r.mu.RLock(ctx); err != nil {
		return MemoryRegionStats{}, err
	}
	defer r.mu.RUnlock()
	h := r.host
	stats := MemoryRegionStats{PageSize: h.pageSize}
	var bound []*zbinding
	z.mu.Lock()
	z.eachBoundLocked(0, uint64(r.pageCount), func(b *zbinding) { bound = append(bound, b) })
	z.mu.Unlock()
	h.mu.Lock()
	for _, b := range bound {
		if b.page == nil {
			continue
		}
		stats.ResidentPages++
		for alias := range frameOf(b.page).aliases.all() {
			if alias.region != z {
				stats.SharedPages++
				break
			}
		}
	}
	h.mu.Unlock()
	stats.DirtySince = z.oldestUnpublished()
	return stats, nil
}
