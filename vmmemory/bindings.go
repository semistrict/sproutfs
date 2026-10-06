package vmmemory

// repeated reports whether a fault on index for this access would be a repeated
// fault: this memory region already maps the page for it. See repeats.go.
func (r *MemoryRegion) repeated(index uint64, write bool) bool {
	z := r.zircon

	return z.repeated(index, write)
}

// dirtyCount reports how many pages hold private state a checkpoint has not
// taken, which is what the next seal takes.
func (r *MemoryRegion) dirtyCount() int {
	z := r.zircon

	return z.dirtyCount()
}
