package vmmemory

// binding is a page's state in one memory region. It is the content of the
// page's slot in the region's page list, as a page is the content of a slot of
// Zircon's VmPageList. It holds what Zircon's slot holds in its own way — the
// resident page, explicit zero and the spill reservation — and beside them
// what Zircon has no place for: whether the page is mapped, the checkpoint's
// copy it shares, the page it was copied from, whether it is cold and whether
// write-ahead made it.
type binding struct {
	memoryRegion *MemoryRegion
	index        uint64
	// Host.mu protects the pointer. The pointed-to resident's lock protects
	// mapping state. Eviction publishes spill before clearing this pointer.
	resident *resident
	// spill is the dirty reservation this page was admitted under, or none.
	// Whether it holds the page's bytes is the spill storage's own state: a
	// seal hands a reservation to the checkpoint's copy while a reclaim may
	// already be writing the bytes it will hold, and the fact has to follow the
	// reservation rather than the binding that named it.
	spill reservation
	// checkpoint names the detached copy holding this page's sealed bytes while a
	// checkpoint ingests. Such a binding is dirty but owns neither the spill
	// reservation nor the right to store: it shares the checkpoint's page until a
	// store copies away from it. The detached copy itself is not reachable from
	// the memory region's bindings and always has checkpoint == nil.
	checkpoint *binding
	// origin is the resident page this private copy was made from, when that
	// page held a published page identity: eight bytes, and not the identity
	// itself. A write fault is not always a store, so the settle compares the
	// sealed bytes with what that page still holds and re-shares the copy onto
	// it where the two are equal. Nothing is pinned by the pointer — an origin
	// that has been evicted is simply no longer an origin — and a page copied
	// from a checkpoint's held copy, from the name a fork point lent a private
	// page, from another host's unpublished page or from zeros has none.
	origin *resident
	mapped bool
	zero   bool // explicit zero backing, independent of arena residency
	dirty  bool
	// ahead marks a private page that write-ahead made resident before any
	// store into it. A store into a writable page never faults, so it stays
	// set until the page's dirty epoch ends, and only the bytes written back
	// can tell whether the guest used it.
	ahead bool
	// cold marks a copy a store trap made of origin, which is not yet known to
	// be the guest's state and pins origin until it is: see cold.go.
	cold bool
	// inZeroRun marks a bound page that a compressed zero run maps: a page
	// whose binding is clean and holds no memory, which a plan mapped to zero.
	// The run's mapping becomes the page's own state when the page is next
	// bound, as it does for a page of the run with no binding. A slot of the
	// page list holds a binding or lies in an interval, never both, so the
	// run keeps such a page here.
	inZeroRun bool
}

// offset is the page list's offset of a page. The page list keeps Zircon's byte
// offsets; the pager counts pages, and converts here.
func (r *MemoryRegion) offset(index uint64) uint64 { return index * r.host.pageSize }

// lookupLocked is the binding of a page that has one, and whether a
// compressed zero run maps the page. Caller holds bindingsMu.
func (r *MemoryRegion) lookupLocked(index uint64) (b *binding, zeroRun bool) {
	offset := r.offset(index)
	if slot := r.pages.Lookup(offset); slot != nil && slot.IsPage() {
		b = slot.Page()
		return b, b.inZeroRun
	}
	return nil, r.pages.IsOffsetInZeroInterval(offset)
}

// setMapped and isMapped carry a page's mapping state across the one pair of
// holders that do not exclude each other: a reclaim revokes a victim's pages
// under that page's lock alone, and a seal reads them under the memory region.
func (r *MemoryRegion) setMapped(b *binding, mapped bool) {
	r.bindingsMu.Lock()
	b.mapped = mapped
	r.noteSealableLocked(b)
	r.bindingsMu.Unlock()
}

// noteSealableLocked records whether this page is one the next seal would
// write-protect: this memory region's own dirty state, held by no checkpoint, and
// mapped. The seal reads the runs of those pages rather than walking the dirty
// set, so its pause costs the commands it issues and not the pages they cover;
// every transition that changes any of the three keeps this up to date, which
// is what makes reading it O(runs). Caller holds bindingsMu.
func (r *MemoryRegion) noteSealableLocked(b *binding) {
	sealable := b.dirty && b.checkpoint == nil && b.mapped
	if r.dirtyRuns.has(b.index) == sealable {
		// Changing a run costs a join or a split, and most of these
		// transitions change nothing: a read of the runs is what tells them
		// apart.
		return
	}
	if sealable {
		r.dirtyRuns.add(b.index, b.index+1)
	} else {
		r.dirtyRuns.remove(b.index)
	}
}

func (r *MemoryRegion) isMapped(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.mapped
}

func (r *MemoryRegion) lookupBinding(index uint64) *binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b
}

// repeated reports whether a fault on index for this access would be a repeated
// fault: this memory region already maps the page for it. See repeats.go.
func (r *MemoryRegion) repeated(index uint64, write bool) bool {
	z := r.zircon

	return z.repeated(index, write)
}

// originOf reports the page a checkpoint's copy was made from, nil where it was
// made from nothing a settle may compare it with.
func (r *MemoryRegion) originOf(b *binding) *resident {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.origin
}

func (r *MemoryRegion) retireFromCheckpoint(b *binding) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	// The page is the volume's again, so where it was copied from says nothing
	// about it any more.
	b.checkpoint, b.dirty, b.origin = nil, false, nil
	delete(r.dirtyBindings, b.index)
	r.noteSealableLocked(b)
}

// heldBy reports whether the live page still shares the checkpoint's copy,
// which is what decides between publishing that copy and discarding it.
func (r *MemoryRegion) heldBy(index uint64, held *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b != nil && b.checkpoint == held
}

// dirtyCount reports how many pages hold private state a checkpoint has not
// taken, which is what the next seal takes.
func (r *MemoryRegion) dirtyCount() int {
	z := r.zircon

	return z.dirtyCount()
}
