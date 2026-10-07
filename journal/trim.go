package journal

// Trim says which of a VM's entries are still live. An entry is live while
// the VM's control record names this journal and the entry's epoch, and the
// entry's position is after the covered position the record names for it.
// covered maps each epoch the record names to its covered position; every
// entry of an epoch it does not name is dead, and so is every entry at or
// before its covered position. The tail moves on to the oldest live entry,
// and the ring before it is free once the tail hint reaches the header.
func (j *Journal) Trim(vm string, covered map[uint64]uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for epoch, h := range j.held[vm] {
		position, named := covered[epoch]
		kept := h.entries[:0]
		for _, e := range h.entries {
			if !named || e.position <= position {
				e.dead = true
				h.bytes -= e.length
				continue
			}
			kept = append(kept, e)
		}
		clear(h.entries[len(kept):])
		h.entries = kept
		if len(kept) == 0 {
			delete(j.held[vm], epoch)
		}
	}
	if len(j.held[vm]) == 0 {
		delete(j.held, vm)
	}
	if tail := j.liveTail(); tail != j.tail {
		j.tail = tail
		j.notify()
	}
}
