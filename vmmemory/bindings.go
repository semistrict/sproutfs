package vmmemory

import "github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"

// repeated reports whether a fault on index for this access would be a repeated
// fault: this memory region already maps the page for it. See repeats.go.
func (r *MemoryRegion) repeated(index uint64, write bool) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, zero := r.lookupLocked(index)
	if zero {
		return !write
	}
	return b != nil && b.mapped && (!write || b.writable())
}

// dirtyCount reports how many pages hold private state a checkpoint has not
// taken, which is what the next seal takes.
func (r *MemoryRegion) dirtyCount() int {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return len(r.dirtySet)
}

// The bindings beside a region's layer: what Zircon keeps in page tables it
// can read back, and the pager cannot. A page has a binding once the region
// maps it or maps from it; a run of pages mapped to zero with nothing else is
// an Untracked zero interval of the same page list, which costs nothing per
// page, as the current core's compressed zero runs do (bindings.go).

// bindingLocked is the binding of one page, made where the page has none. A
// page that leaves a compressed zero run takes the run's mapping with it.
// Caller holds r.bindingsMu.
func (r *MemoryRegion) bindingLocked(index uint64) *zbinding {
	offset := index * r.host.pageSize
	if slot := r.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b := slot.Page()
		if b.inZeroRun {
			b.inZeroRun = false
			b.zero, b.mapped = true, true
		}
		return b
	}
	slot, inRun := r.beside.LookupOrAllocate(offset, zirconvm.SplitInterval)
	b := &zbinding{region: r, index: index}
	if inRun {
		b.zero, b.mapped = true, true
	}
	slot.Set(zirconvm.Page(b))
	return b
}

// lookupLocked is the binding of a page that has one, and whether a
// compressed zero run maps the page. Caller holds r.bindingsMu.
func (r *MemoryRegion) lookupLocked(index uint64) (b *zbinding, zeroRun bool) {
	offset := index * r.host.pageSize
	if slot := r.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b = slot.Page()
		return b, b.inZeroRun
	}
	return nil, r.beside.IsOffsetInZeroInterval(offset)
}

// eachBoundLocked calls visit on the binding of every page of [first, last)
// that has one, in order. Caller holds r.bindingsMu.
func (r *MemoryRegion) eachBoundLocked(first, last uint64, visit func(*zbinding)) {
	ps := r.host.pageSize
	if err := r.beside.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
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
func (r *MemoryRegion) bind(index uint64, p *zirconvm.VmPage) {
	r.bindingsMu.Lock()
	b := r.bindingLocked(index)
	r.bindingsMu.Unlock()
	h := r.host
	h.mu.Lock()
	if b.page != p {
		if b.page != nil {
			r.host.unaliasLocked(b)
		}
		r.host.aliasLocked(b, p)
	}
	h.mu.Unlock()
}

// eligible reports whether a page can join a plan: the region holds nothing
// there yet, no page, no zero and no mapping.
func (r *MemoryRegion) eligible(index uint64) bool {
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(index)
	r.bindingsMu.Unlock()
	return r.eligibleBinding(b)
}

// eligibleBinding is eligible of a page's binding, nil for one with none.
func (r *MemoryRegion) eligibleBinding(b *zbinding) bool {
	if b == nil {
		return true
	}
	h := r.host
	h.mu.Lock()
	page := b.page
	h.mu.Unlock()
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return page == nil && !b.mapped && !b.zero && !b.inZeroRun && !b.dirty && b.checkpoint == nil
}

// eligibleIn is eligible of every page of [first, last) by its binding, under
// one walk of the bindings. A page of a compressed zero run has none and is
// eligible, as in the current core: a plan finds it a hole, and its install
// finds it mapped.
func (r *MemoryRegion) eligibleIn(first, last uint64) []bool {
	result := make([]bool, last-first)
	for i := range result {
		result[i] = true
	}
	var bound []*zbinding
	r.bindingsMu.Lock()
	r.eachBoundLocked(first, last, func(b *zbinding) { bound = append(bound, b) })
	r.bindingsMu.Unlock()
	for _, b := range bound {
		result[b.index-first] = r.eligibleBinding(b)
	}
	return result
}

// heldIn is the pages of [first, last) that may not join a plan, in order.
func (r *MemoryRegion) heldIn(first, last uint64) []uint64 {
	var held []uint64
	for i, ok := range r.eligibleIn(first, last) {
		if !ok {
			held = append(held, first+uint64(i))
		}
	}
	return held
}

// mapped reports whether the region maps a page, to a page or to zero.
func (r *MemoryRegion) mapped(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, zero := r.lookupLocked(index)
	return zero || b != nil && b.mapped
}

// setMapped records whether the pages of [first, last) are mapped. A page
// recorded mapped is retained across an ambiguous answer, as the current
// core's are.
func (r *MemoryRegion) setMapped(first, last uint64, mapped bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	for page := first; page < last; page++ {
		b := r.bindingLocked(page)
		b.mapped = mapped
		r.noteSealableLocked(b)
	}
}

// mapZeros records that a plan mapped every page of [start, end) to zero, as
// MemoryRegion.mapZeros does.
func (r *MemoryRegion) mapZeros(start, end uint64) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	ps := r.host.pageSize
	var gaps [][2]uint64
	if err := r.beside.ForEveryPageAndGapInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
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
		if err := r.beside.AddZeroInterval(gap[0], gap[1], zirconvm.IntervalUntracked); err != nil {
			panic("vmmemory: adding a zero run: " + err.Error())
		}
	}
}

// unmapRuns takes back the record that the runs of one refused command are
// mapped, as MemoryRegion.unmapRuns does.
func (r *MemoryRegion) unmapRuns(runs []MapRun) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	ps := r.host.pageSize
	for _, run := range runs {
		first, last := run.Page, run.Page+uint64(run.Count)
		if run.Zero {
			r.eachBoundLocked(first, last, func(b *zbinding) { b.inZeroRun = false })
			start, end := first*ps, last*ps
			if r.beside.IsOffsetInZeroInterval(start) {
				r.beside.LookupOrAllocate(start, zirconvm.SplitInterval)
			}
			if lastPage := end - ps; lastPage > start && r.beside.IsOffsetInZeroInterval(lastPage) {
				r.beside.LookupOrAllocate(lastPage, zirconvm.SplitInterval)
			}
			if err := r.beside.RemovePages(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
				if slot.IsInterval() {
					slot.Take()
				}
				return nil
			}, start, end); err != nil {
				panic("vmmemory: walking the bindings beside a layer: " + err.Error())
			}
			continue
		}
		r.eachBoundLocked(first, last, func(b *zbinding) {
			b.mapped = false
			r.noteSealableLocked(b)
		})
	}
}

// zbinding is what a memory region keeps beside its layer for one page:
// whether its mapping is installed, and the page it maps, which Zircon keeps
// in page tables it can read back and the pager cannot; and of a page the
// region has stored into, what Zircon has no place for: the dirty reservation
// it was admitted under, the checkpoint's copy it shares, the page it was
// copied from, whether it is cold, and whether write-ahead made it.
//
// A checkpoint's copy of a page is a zbinding too, detached from the page
// list beside the layer, as the current core's is: it aliases the page the
// guest had at the seal, AwaitingClean in the layer, and owns the reservation
// that page was admitted under (D1, D5). The guest's binding shares it until
// a store copies away from it.
type zbinding struct {
	region *MemoryRegion
	index  uint64
	// page is the page this region maps here, nil where it maps none or a
	// zero: a page of an identity root, or of the region's own layer. Guarded
	// by Host.mu, as the page's aliases are.
	page *zirconvm.VmPage
	// mapped is whether the mapping is installed, and zero whether it is a
	// zero. inZeroRun marks a bound page a compressed zero run maps, as the
	// current core's binding does. Guarded by MemoryRegion.bindingsMu.
	mapped, zero, inZeroRun bool
	// dirty marks a page that is the region's own state and not yet its
	// volume's: Dirty in the layer, AwaitingClean while it shares the
	// checkpoint's copy, or spilled. spill is the dirty reservation it was
	// admitted under, which a seal hands to checkpoint, the copy it then
	// shares until a store copies away from it. origin is the root's page it
	// was copied from, cold marks a copy a store trap made of origin that is
	// not yet known to be the guest's state, from coldAt (Unix nanoseconds),
	// and ahead marks one write-ahead made before any store. Guarded by
	// MemoryRegion.bindingsMu. The marks sit beside the reservation, which leaves
	// them room, so that a binding, which every page a region touches has, is
	// 64 bytes.
	dirty       bool
	spill       reservation
	cold, ahead bool
	checkpoint  *zbinding
	origin      *zirconvm.VmPage
	coldAt      int64
}

// writable reports whether the guest may store into b's page where it is:
// its own dirty state, which no checkpoint still holds.
func (b *zbinding) writable() bool { return b.dirty && b.checkpoint == nil }

// noteDirtyLocked puts b in the dirty set, Dirty and writable where it is,
// and starts the region's loss window where it held none. Caller holds r.bindingsMu.
func (r *MemoryRegion) noteDirtyLocked(b *zbinding) {
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*zbinding)
	}
	r.dirtySet[b.index] = b
	if r.dirtySince.IsZero() {
		r.dirtySince = r.host.clock.Now()
	}
	r.noteSealableLocked(b)
}

// noteSealableLocked records whether b is a page the next seal
// write-protects: the region's own dirty state, held by no checkpoint, and
// mapped, as MemoryRegion.noteSealableLocked does. Caller holds r.bindingsMu.
func (r *MemoryRegion) noteSealableLocked(b *zbinding) {
	sealable := b.writable() && b.mapped
	if r.dirtyRuns.has(b.index) == sealable {
		return
	}
	if sealable {
		r.dirtyRuns.add(b.index, b.index+1)
	} else {
		r.dirtyRuns.remove(b.index)
	}
}

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
