package vmmemory

// reservations are a pager's dirty reservations: the slots of its spill file,
// one per private page it admits, and what each holds. The budget is a count
// and not an allocation. A reservation is state only once it has been handed
// out: the slots below next have been, lowest first, and those given back since
// are handed out again before any new one. So a pager admitting far more dirty
// pages than its guests ever write costs memory for what they wrote, where one
// entry per admitted page cost a 4 KiB pager with a 400 GiB budget 1.4 GB of
// heap before a guest had started. Caller holds Host.mu.
type reservations struct {
	budget int
	next   int
	free   []int
	// held is what each reservation handed out so far holds: whether its slot
	// of the spill file has bytes, and the checksum they must come back with.
	// It is this process's own authority over a scratch file.
	held []spillSlot
}

// spillSlot is one reservation's slot of the spill file.
type spillSlot struct {
	sum     uint32
	written bool
}

func newReservations(budget int) *reservations { return &reservations{budget: budget} }

// available is how many more reservations may be taken.
func (r *reservations) available() int { return len(r.free) + r.budget - r.next }

// take hands out one reservation, reporting false where the budget has none.
func (r *reservations) take() (int, bool) {
	if n := len(r.free); n > 0 {
		slot := r.free[n-1]
		r.free = r.free[:n-1]
		return slot, true
	}
	if r.next == r.budget {
		return 0, false
	}
	slot := r.next
	r.next++
	r.held = append(r.held, spillSlot{})
	return slot, true
}

// put gives one back, holding nothing.
func (r *reservations) put(slot int) {
	r.held[slot] = spillSlot{}
	r.free = append(r.free, slot)
}

// record says what a spill wrote to a reservation's slot: the checksum of its
// bytes, and whether the slot is to be read back at all.
func (r *reservations) record(slot int, sum uint32, written bool) {
	r.held[slot] = spillSlot{sum: sum, written: written}
}

// holds reports whether a reservation's slot has bytes.
func (r *reservations) holds(slot int) bool { return r.held[slot].written }

// digest reports the checksum a reservation's bytes must have, and whether its
// slot has any.
func (r *reservations) digest(slot int) (uint32, bool) {
	return r.held[slot].sum, r.held[slot].written
}

// forget drops what every slot held, which truncating the spill file does.
func (r *reservations) forget() { clear(r.held) }
