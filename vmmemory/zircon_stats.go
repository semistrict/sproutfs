package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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
// maps, of those the ones another region maps too, and its private pages.
func (z *zirconRegion) stats(ctx context.Context) (MemoryRegionStats, error) {
	r := z.region
	if err := r.mu.RLock(ctx); err != nil {
		return MemoryRegionStats{}, err
	}
	defer r.mu.RUnlock()
	stats := MemoryRegionStats{PageSize: r.host.pageSize}
	z.eachBinding(func(b *zbinding) {
		if b.page != nil {
			stats.ResidentPages++
			for alias := range frameOf(b.page).aliases.all() {
				if alias.region != z {
					stats.SharedPages++
					break
				}
			}
		}
		if b.dirty {
			stats.PrivatePages++
		}
	})
	stats.DirtySince = z.oldestUnpublished()
	return stats, nil
}

// eachBinding is MemoryRegion.eachBinding over the zircon core: every page
// with a binding, in ascending order, with the host lock and then the
// region's held, a batch at a time, so a large region holds up no page
// transition for the whole of a scan. Caller holds the region lock, which
// keeps the bindings in existence.
func (z *zirconRegion) eachBinding(visit func(*zbinding)) {
	h := z.region.host
	for from, more := uint64(0), true; more; {
		h.mu.Lock()
		z.mu.Lock()
		from, more = z.eachBindingLocked(from, visit)
		z.mu.Unlock()
		h.mu.Unlock()
	}
}

// eachBindingLocked visits up to eachBindingBatch bindings from page from on,
// and reports the page to go on from and whether any is left. Caller holds
// h.mu and then z.mu.
func (z *zirconRegion) eachBindingLocked(from uint64, visit func(*zbinding)) (next uint64, more bool) {
	ps := z.region.host.pageSize
	visited := 0
	if err := z.beside.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
		if !slot.IsPage() {
			return nil
		}
		b := slot.Page()
		if visited == eachBindingBatch {
			next, more = b.index, true
			return zirconvm.ErrStop
		}
		visit(b)
		visited++
		return nil
	}, from*ps, uint64(z.region.pageCount)*ps); err != nil {
		panic("vmmemory: walking the bindings beside a layer: " + err.Error())
	}
	return next, more
}
