package vmmemory

import "time"

// The loss window bounds a VM's unpublished writes in time, where the dirty
// budget bounds them in bytes. Every page this memory region holds that no checkpoint
// covers is dated by one timestamp — the oldest of them — because that is all
// the bound needs: the checkpoint that ends the window covers the whole memory region
// in one pause, so nothing is learnt by dating each page apart from the rest,
// and one timestamp costs a memory region nothing.

// noteDirty starts the window at the first store this memory region holds that no
// checkpoint covers. Every later one is inside it, so the stamp is taken once
// and kept until a checkpoint carries the pages away. Caller holds bindingsMu,
// which is the lock the dirty set itself changes under: a page entering that set
// and the window opening on it are one transition.
func (r *MemoryRegion) noteDirtyLocked() {
	if r.dirtySince.IsZero() {
		r.dirtySince = r.host.clock.Now()
	}
}

// takeDirtySince hands this memory region's window to the checkpoint that has just
// sealed its dirty set. The memory region keeps nothing: every page it holds
// unpublished is the checkpoint's now, and the next store into it opens a window
// of its own.
func (r *MemoryRegion) takeDirtySince() time.Time {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	since := r.dirtySince
	r.dirtySince, r.windowAsked = time.Time{}, false
	return since
}

// restoreDirtySince gives a checkpoint's window back to the memory region, which is
// what an abandoned checkpoint and an unseal do: those pages are the guest's
// dirty state again, and they are exactly as old as they were. The older of the
// two stands — the guest has been storing since the seal, and neither half of
// what the memory region now holds may be dated by the other.
func (r *MemoryRegion) restoreDirtySince(since time.Time) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.dirtySince, r.windowAsked = older(r.dirtySince, since), false
}

// SetUnpublishedAge dates the pages this memory region has inherited from another host,
// which is what a migration destination does with the age the handoff carried:
// the window moves with the pages, so a VM handed on cannot outrun its bound by
// being handed on again. The age is relative to now on this host's own clock.
//
// The older of this and whatever the memory region already holds stands, so a guest
// that has been faulting since it resumed keeps the earlier of its own first
// store and what it was handed.
func (r *MemoryRegion) SetUnpublishedAge(age time.Duration) {
	if age <= 0 {
		return
	}
	r.restoreDirtySince(r.host.clock.Now().Add(-age))
}

// OldestUnpublished is when the oldest write this memory region holds that no landed
// checkpoint covers was made, zero where it holds none. It is the memory region's own
// dirty set and the checkpoint still draining out of it together: a publication
// in flight has not landed, so what it carries is still unpublished, and a fork
// point's hold is the same thing for longer.
//
// It is what a host sums across the memory regions of one VM to answer Pressure.Oldest,
// and what it reports that VM's loss window from.
func (r *MemoryRegion) OldestUnpublished() time.Time {
	r.bindingsMu.Lock()
	since := r.dirtySince
	r.bindingsMu.Unlock()
	if _, draining := r.sealState(); draining != nil {
		since = older(since, draining.since())
	}
	return since
}

// unpublishedAge is how long this memory region has held its oldest unpublished write,
// zero where it holds none. A memory region handed off reports it so the destination
// can go on measuring the same window.
func (r *MemoryRegion) unpublishedAge() time.Duration {
	since := r.OldestUnpublished()
	if since.IsZero() {
		return 0
	}
	return max(r.host.clock.Since(since), 0)
}

// older is the earlier of two stamps, treating zero as no stamp at all rather
// than as the beginning of time.
func older(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	}
	return a
}

// windowAge is how long the VM this memory region belongs to has held its oldest
// write no checkpoint covers, zero where it holds none. It is asked before every
// dirty reservation, so it takes no lock a fault holds and asks the memory
// region's owner rather than knowing anything about VMs.
func (h *Host) windowAge(r *MemoryRegion) time.Duration {
	since := r.OldestUnpublished()
	if !since.IsZero() && h.clock.Since(since) > h.cfg.LossWindow {
		// This memory region alone is past the window, so what its siblings
		// hold cannot change the answer. A store held back asks in this case,
		// over and over, and it costs nothing to answer.
		return h.clock.Since(since)
	}
	h.mu.Lock()
	oldest := h.pressure.Oldest
	h.mu.Unlock()
	if oldest != nil {
		since = older(since, oldest(r))
	}
	if since.IsZero() {
		return 0
	}
	return max(h.clock.Since(since), 0)
}

// windowAnswer is what a store does about its VM's loss window.
type windowAnswer int

const (
	// windowAdmit lets the store through.
	windowAdmit windowAnswer = iota
	// windowWait holds it until a checkpoint of its VM lands.
	windowWait
	// windowStall fails it: its VM is past the window and no checkpoint of it
	// can ever be taken.
	windowStall
)

// window decides what a store into r does about its VM's loss window.
//
// A store is held only behind a checkpoint that is already sealed and
// uploading. A held store holds the vCPU that made it, inside its fault, and a
// pause needs every vCPU. So a store held while its checkpoint still needed a
// pause would wait for a pause it prevents. Past the window, then:
//
//   - a sealed publication of the VM is uploading: the store waits for it;
//   - a pause is under way, or a fork point holds the pages until its children
//     have them: the store goes through. The fork hold ends at its deadline in
//     a checkpoint that needs a pause of its own;
//   - nothing is sealed: the store goes through and asks for the checkpoint,
//     and the stores after it wait once that checkpoint has sealed;
//   - no checkpoint of the VM can be taken at all: the store stalls, and the
//     VM's owner stops it.
//
// So the guest writes past the window for at most the time one pause takes.
// Before the window runs out, the owner's own clock normally takes the
// checkpoint that ends it, so the pause comes while the guest still runs.
func (h *Host) window(r *MemoryRegion) windowAnswer {
	window := h.cfg.LossWindow
	if window <= 0 {
		return windowAdmit
	}
	age := h.windowAge(r)
	if age <= window {
		return windowAdmit
	}
	sealing, draining := r.sealState()
	if draining != nil && draining.relieves() {
		return windowWait
	}
	if sealing || draining != nil {
		return windowAdmit
	}
	if !r.askWindow() {
		return windowAdmit
	}
	h.mu.Lock()
	request := h.pressure.Checkpoint
	h.mu.Unlock()
	if request != nil && request(r) {
		h.mu.Lock()
		h.stats.CheckpointRequests++
		h.mu.Unlock()
		return windowAdmit
	}
	r.forgetWindowAsk()
	return windowStall
}

// askWindow claims this memory region's one request for the checkpoint that
// ends its window, reporting whether this caller is the one to ask.
func (r *MemoryRegion) askWindow() bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if r.windowAsked {
		return false
	}
	r.windowAsked = true
	return true
}

// forgetWindowAsk gives the request back when nobody took it, so the next
// store asks again.
func (r *MemoryRegion) forgetWindowAsk() {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.windowAsked = false
}
