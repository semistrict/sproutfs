package vmmemory

import (
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The bindings beside a region's layer: what Zircon keeps in page tables it
// can read back, and the pager cannot. A page has a binding once the region
// maps it or maps from it; a run of pages mapped to zero with nothing else is
// an Untracked zero interval of the same page list, which costs nothing per
// page, as the current core's compressed zero runs do (bindings.go).

// bindingLocked is the binding of one page, made where the page has none. A
// page that leaves a compressed zero run takes the run's mapping with it.
// Caller holds z.mu.
func (z *zirconRegion) bindingLocked(index uint64) *zbinding {
	offset := index * z.region.host.pageSize
	if slot := z.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b := slot.Page()
		if b.inZeroRun {
			b.inZeroRun = false
			b.zero, b.mapped = true, true
		}
		return b
	}
	slot, inRun := z.beside.LookupOrAllocate(offset, zirconvm.SplitInterval)
	b := &zbinding{region: z, index: index}
	if inRun {
		b.zero, b.mapped = true, true
	}
	slot.Set(zirconvm.Page(b))
	return b
}

// lookupLocked is the binding of a page that has one, and whether a
// compressed zero run maps the page. Caller holds z.mu.
func (z *zirconRegion) lookupLocked(index uint64) (b *zbinding, zeroRun bool) {
	offset := index * z.region.host.pageSize
	if slot := z.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b = slot.Page()
		return b, b.inZeroRun
	}
	return nil, z.beside.IsOffsetInZeroInterval(offset)
}

// eachBoundLocked calls visit on the binding of every page of [first, last)
// that has one, in order. Caller holds z.mu.
func (z *zirconRegion) eachBoundLocked(first, last uint64, visit func(*zbinding)) {
	ps := z.region.host.pageSize
	if err := z.beside.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
		if slot.IsPage() {
			visit(slot.Page())
		}
		return nil
	}, first*ps, last*ps); err != nil {
		panic("vmmemory: walking the bindings beside a layer: " + err.Error())
	}
}

// bind makes page index of the region map p, a page an object holds, in
// place of whatever it mapped from before. The caller holds the lock of p's
// object, so p cannot be given up between its lookup and here.
func (z *zirconRegion) bind(index uint64, p *zirconvm.VmPage) {
	z.mu.Lock()
	b := z.bindingLocked(index)
	z.mu.Unlock()
	h := z.region.host
	h.mu.Lock()
	if b.page != p {
		if b.page != nil {
			z.host.unaliasLocked(b)
		}
		z.host.aliasLocked(b, p)
	}
	h.mu.Unlock()
}

// eligible reports whether a page can join a plan: the region holds nothing
// there yet, no page, no zero and no mapping.
func (z *zirconRegion) eligible(index uint64) bool {
	z.mu.Lock()
	b, _ := z.lookupLocked(index)
	z.mu.Unlock()
	return z.eligibleBinding(b)
}

// eligibleBinding is eligible of a page's binding, nil for one with none.
func (z *zirconRegion) eligibleBinding(b *zbinding) bool {
	if b == nil {
		return true
	}
	h := z.region.host
	h.mu.Lock()
	page := b.page
	h.mu.Unlock()
	z.mu.Lock()
	defer z.mu.Unlock()
	return page == nil && !b.mapped && !b.zero && !b.inZeroRun && !b.dirty && b.checkpoint == nil
}

// eligibleIn is eligible of every page of [first, last) by its binding, under
// one walk of the bindings. A page of a compressed zero run has none and is
// eligible, as in the current core: a plan finds it a hole, and its install
// finds it mapped.
func (z *zirconRegion) eligibleIn(first, last uint64) []bool {
	result := make([]bool, last-first)
	for i := range result {
		result[i] = true
	}
	var bound []*zbinding
	z.mu.Lock()
	z.eachBoundLocked(first, last, func(b *zbinding) { bound = append(bound, b) })
	z.mu.Unlock()
	for _, b := range bound {
		result[b.index-first] = z.eligibleBinding(b)
	}
	return result
}

// heldIn is the pages of [first, last) that may not join a plan, in order.
func (z *zirconRegion) heldIn(first, last uint64) []uint64 {
	var held []uint64
	for i, ok := range z.eligibleIn(first, last) {
		if !ok {
			held = append(held, first+uint64(i))
		}
	}
	return held
}

// repeated is MemoryRegion.repeated: whether a fault on index for this access
// would be a repeated one, the region mapping the page for it already: for a
// store, its own Dirty page.
func (z *zirconRegion) repeated(index uint64, write bool) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	b, zero := z.lookupLocked(index)
	if zero {
		return !write
	}
	return b != nil && b.mapped && (!write || b.writable())
}

// mapped reports whether the region maps a page, to a page or to zero.
func (z *zirconRegion) mapped(index uint64) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	b, zero := z.lookupLocked(index)
	return zero || b != nil && b.mapped
}

// setMapped records whether the pages of [first, last) are mapped. A page
// recorded mapped is retained across an ambiguous answer, as the current
// core's are.
func (z *zirconRegion) setMapped(first, last uint64, mapped bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	for page := first; page < last; page++ {
		b := z.bindingLocked(page)
		b.mapped = mapped
		z.noteSealableLocked(b)
	}
}

// mapZeros records that a plan mapped every page of [start, end) to zero, as
// MemoryRegion.mapZeros does.
func (z *zirconRegion) mapZeros(start, end uint64) {
	z.mu.Lock()
	defer z.mu.Unlock()
	ps := z.region.host.pageSize
	var gaps [][2]uint64
	if err := z.beside.ForEveryPageAndGapInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
		if slot.IsPage() {
			slot.Page().inZeroRun = true
		}
		return nil
	}, func(start, end uint64) error {
		gaps = append(gaps, [2]uint64{start, end})
		return nil
	}, start*ps, end*ps); err != nil {
		panic("vmmemory: walking the bindings beside a layer: " + err.Error())
	}
	for _, gap := range gaps {
		if err := z.beside.AddZeroInterval(gap[0], gap[1], zirconvm.IntervalUntracked); err != nil {
			panic("vmmemory: adding a zero run: " + err.Error())
		}
	}
}

// unmapRuns takes back the record that the runs of one refused command are
// mapped, as MemoryRegion.unmapRuns does.
func (z *zirconRegion) unmapRuns(runs []MapRun) {
	z.mu.Lock()
	defer z.mu.Unlock()
	ps := z.region.host.pageSize
	for _, run := range runs {
		first, last := run.Page, run.Page+uint64(run.Count)
		if run.Zero {
			z.eachBoundLocked(first, last, func(b *zbinding) { b.inZeroRun = false })
			start, end := first*ps, last*ps
			if z.beside.IsOffsetInZeroInterval(start) {
				z.beside.LookupOrAllocate(start, zirconvm.SplitInterval)
			}
			if lastPage := end - ps; lastPage > start && z.beside.IsOffsetInZeroInterval(lastPage) {
				z.beside.LookupOrAllocate(lastPage, zirconvm.SplitInterval)
			}
			if err := z.beside.RemovePages(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
				if slot.IsInterval() {
					slot.Take()
				}
				return nil
			}, start, end); err != nil {
				panic("vmmemory: walking the bindings beside a layer: " + err.Error())
			}
			continue
		}
		z.eachBoundLocked(first, last, func(b *zbinding) {
			b.mapped = false
			z.noteSealableLocked(b)
		})
	}
}
