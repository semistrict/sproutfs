package vmmemory

import (
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// eachBinding is MemoryRegion.eachBinding over the zircon core: every page
// with a binding, in ascending order, with the host lock and then the
// region's held, a batch at a time, so a large region holds up no page
// transition for the whole of a scan. Caller holds the region lock, which
// keeps the bindings in existence.
func (r *MemoryRegion) eachBinding(visit func(*zbinding)) {
	h := r.host
	for from, more := uint64(0), true; more; {
		h.mu.Lock()
		r.bindingsMu.Lock()
		from, more = r.eachBindingLocked(from, visit)
		r.bindingsMu.Unlock()
		h.mu.Unlock()
	}
}

// eachBindingLocked visits up to eachBindingBatch bindings from page from on,
// and reports the page to go on from and whether any is left. Caller holds
// h.mu and then r.bindingsMu.
func (r *MemoryRegion) eachBindingLocked(from uint64, visit func(*zbinding)) (next uint64, more bool) {
	ps := r.host.pageSize
	visited := 0
	if err := r.beside.ForEveryPageInRange(func(slot *zirconvm.PageOrMarker[zbinding], _ uint64) error {
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
	}, from*ps, uint64(r.pageCount)*ps); err != nil {
		panic("vmmemory: walking the bindings beside a layer: " + err.Error())
	}
	return next, more
}
