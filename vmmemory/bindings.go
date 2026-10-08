package vmmemory

import "github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"

// The bindings beside a region's layer: what Zircon keeps in page tables it
// can read back, and the pager cannot. A page has a binding once the region
// maps it or maps from it; a run of pages mapped to zero with nothing else is
// an Untracked zero interval of the same page list, which costs nothing per
// page.

// binding is what a memory region keeps beside its layer for one page: whether
// its mapping is installed, and the page it maps, which Zircon keeps in page
// tables it can read back and the pager cannot; and of a page the region has
// stored into, what Zircon has no place for: the dirty reservation it was
// admitted under, the checkpoint's copy it shares, the page it was copied
// from, whether it is cold, and whether write-ahead made it.
//
// A checkpoint's copy of a page is a binding too, detached from the page list
// beside the layer: it aliases the page the guest had at the seal,
// AwaitingClean in the layer, and owns the reservation that page was admitted
// under (D1, D5). The guest's binding shares it until a store copies away from
// it.
type binding struct {
	region *MemoryRegion
	index  uint64
	// page is the page this region maps here, nil where it maps none or a
	// zero: a page of an identity root, or of the region's own layer. Guarded
	// by Host.mu, as the page's aliases are.
	page *zirconvm.VmPage
	// mapped is whether the mapping is installed, and zero whether it is a
	// zero. inZeroRun marks a bound page a compressed zero run maps: its
	// binding is clean and holds no page, and the run's mapping becomes the
	// page's own state when the page is next bound. A slot of the page list
	// holds a binding or lies in an interval, never both, so the run keeps
	// such a page here. Guarded by MemoryRegion.bindingsMu.
	mapped, zero, inZeroRun bool
	// dirty marks a page that is the region's own state and not yet its
	// volume's: Dirty in the layer, AwaitingClean while it shares the
	// checkpoint's copy, or spilled. spill is the dirty reservation it was
	// admitted under, which a seal hands to checkpoint, the copy it then
	// shares until a store copies away from it. origin is the root's page it
	// was copied from, cold marks a copy a store trap made of origin that is
	// not yet known to be the guest's state, from coldAt (Unix nanoseconds),
	// and ahead marks one write-ahead made before any store. Guarded by
	// MemoryRegion.bindingsMu. The marks sit beside the reservation, which
	// leaves them room, so that a binding, which every page a region touches
	// has, is 64 bytes.
	//
	// protected marks a dirty page a journal capture write-protected: its
	// bytes are in the journal, and the guest's next store takes a protect
	// trap that maps it writable again (journal.go). zeroed marks a page a
	// store made private from zeros, so its first capture knows what its
	// blocks held before.
	dirty                          bool
	spill                          reservation
	cold, ahead, protected, zeroed bool
	checkpoint                     *binding
	origin                         *zirconvm.VmPage
	coldAt                         int64
}

// writable reports whether the guest may store into b's page where it is: its
// own dirty state, which no checkpoint still holds and no journal capture has
// write-protected.
func (b *binding) writable() bool { return b.dirty && b.checkpoint == nil && !b.protected }

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

// bindingLocked is the binding of one page, made where the page has none. A
// page that leaves a compressed zero run takes the run's mapping with it.
// Caller holds r.bindingsMu.
func (r *MemoryRegion) bindingLocked(index uint64) *binding {
	offset := index * r.host.pageSize
	if slot := r.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b := slot.Page()
		if b.inZeroRun {
			b.inZeroRun = false
			b.zero, b.mapped = true, true
			r.recordedMapped(index, index+1, true, "left a zero run")
		}
		return b
	}
	slot, inRun := r.beside.LookupOrAllocate(offset, zirconvm.SplitInterval)
	b := &binding{region: r, index: index}
	if inRun {
		b.zero, b.mapped = true, true
		r.recordedMapped(index, index+1, true, "made in a zero run")
	}
	slot.Set(zirconvm.Page(b))
	return b
}

// lookupLocked is the binding of a page that has one, and whether a
// compressed zero run maps the page. Caller holds r.bindingsMu.
func (r *MemoryRegion) lookupLocked(index uint64) (b *binding, zeroRun bool) {
	offset := index * r.host.pageSize
	if slot := r.beside.Lookup(offset); slot != nil && slot.IsPage() {
		b = slot.Page()
		return b, b.inZeroRun
	}
	return nil, r.beside.IsOffsetInZeroInterval(offset)
}

// eachBoundLocked calls visit on the binding of every page of [first, last)
// that has one, in order. Caller holds r.bindingsMu.
func (r *MemoryRegion) eachBoundLocked(first, last uint64, visit func(*binding)) {
	ps := r.host.pageSize
	if err := r.beside.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[binding], _ uint64) error {
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
	dirty := b.dirty
	r.bindingsMu.Unlock()
	r.auditBind(index, dirty, p)
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
func (r *MemoryRegion) eligibleBinding(b *binding) bool {
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
// eligible: a plan finds it a hole, and its install finds it mapped.
func (r *MemoryRegion) eligibleIn(first, last uint64) []bool {
	result := make([]bool, last-first)
	for i := range result {
		result[i] = true
	}
	var bound []*binding
	r.bindingsMu.Lock()
	r.eachBoundLocked(first, last, func(b *binding) { bound = append(bound, b) })
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

// noteDirtyLocked puts b in the dirty set, Dirty and writable where it is,
// and starts the region's loss window where it held none. Caller holds r.bindingsMu.
func (r *MemoryRegion) noteDirtyLocked(b *binding) {
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*binding)
	}
	r.dirtySet[b.index] = b
	if r.dirtySince.IsZero() {
		r.dirtySince = r.host.clock.Now()
	}
	r.noteStoredLocked(b)
	r.noteSealableLocked(b)
}

// noteSealableLocked records whether b is a page the next seal
// write-protects: the region's own dirty state, held by no checkpoint, and
// mapped, as MemoryRegion.noteSealableLocked does. Caller holds r.bindingsMu.
func (r *MemoryRegion) noteSealableLocked(b *binding) {
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
func (r *MemoryRegion) lookupBinding(index uint64) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b
}

// isMapped reports whether b's mapping is installed.
func (r *MemoryRegion) isMapped(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.mapped
}

// harvestedReadOnly reports whether the page at index is one the guest may
// only read, bound and not mapped, because a harvest took its mapping away and
// no fault has marked it accessed since (harvest.go). A store trap on it may be
// a read KVM's worker asked for writable, so it is served as a load: the page
// is mapped read-only again, and a real store traps on that mapping and copies
// as a store into a page the guest maps does, not as a cold copy.
func (r *MemoryRegion) harvestedReadOnly(index uint64) bool {
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(index)
	readOnly := b != nil && !b.mapped && !b.writable()
	r.bindingsMu.Unlock()
	if !readOnly {
		return false
	}
	h := r.host
	h.mu.Lock()
	page := b.page
	h.mu.Unlock()
	return page != nil && h.node.PageQueues().IsHarvested(page)
}
