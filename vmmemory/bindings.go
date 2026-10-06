package vmmemory

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
