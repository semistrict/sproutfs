package vmmemory

import "time"

// The loss window bounds a VM's unpublished writes in time, where the dirty
// budget bounds them in bytes. Every page this region holds that no checkpoint
// covers is dated by one timestamp — the oldest of them — because that is all
// the bound needs: the checkpoint that ends the window covers the whole region
// in one pause, so nothing is learnt by dating each page apart from the rest,
// and one timestamp costs a region nothing.

// noteDirty starts the window at the first store this region holds that no
// checkpoint covers. Every later one is inside it, so the stamp is taken once
// and kept until a checkpoint carries the pages away. Caller holds bindingsMu,
// which is the lock the dirty set itself changes under: a page entering that set
// and the window opening on it are one transition.
func (r *Region) noteDirtyLocked() {
	if r.dirtySince.IsZero() {
		r.dirtySince = r.host.clock.Now()
	}
}

// takeDirtySince hands this region's window to the checkpoint that has just
// sealed its dirty set. The region keeps nothing: every page it holds
// unpublished is the checkpoint's now, and the next store into it opens a window
// of its own.
func (r *Region) takeDirtySince() time.Time {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	since := r.dirtySince
	r.dirtySince = time.Time{}
	return since
}

// restoreDirtySince gives a checkpoint's window back to the region, which is
// what an abandoned checkpoint and an unseal do: those pages are the guest's
// dirty state again, and they are exactly as old as they were. The older of the
// two stands — the guest has been storing since the seal, and neither half of
// what the region now holds may be dated by the other.
func (r *Region) restoreDirtySince(since time.Time) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.dirtySince = older(r.dirtySince, since)
}

// SetUnpublishedAge dates the pages this region has inherited from another host,
// which is what a migration destination does with the age the handoff carried:
// the window moves with the pages, so a VM handed on cannot outrun its bound by
// being handed on again. The age is relative to now on this host's own clock.
//
// The older of this and whatever the region already holds stands, so a guest
// that has been faulting since it resumed keeps the earlier of its own first
// store and what it was handed.
func (r *Region) SetUnpublishedAge(age time.Duration) {
	if age <= 0 {
		return
	}
	r.restoreDirtySince(r.host.clock.Now().Add(-age))
}

// OldestUnpublished is when the oldest write this region holds that no landed
// checkpoint covers was made, zero where it holds none. It is the region's own
// dirty set and the checkpoint still draining out of it together: a publication
// in flight has not landed, so what it carries is still unpublished, and a fork
// point's hold is the same thing for longer.
//
// It is what a host sums across the regions of one VM to answer Pressure.Oldest,
// and what it reports that VM's loss window from.
func (r *Region) OldestUnpublished() time.Time {
	r.bindingsMu.Lock()
	since := r.dirtySince
	r.bindingsMu.Unlock()
	if _, draining := r.sealState(); draining != nil {
		since = older(since, draining.dirtySince)
	}
	return since
}

// unpublishedAge is how long this region has held its oldest unpublished write,
// zero where it holds none. A region handed off reports it so the destination
// can go on measuring the same window.
func (r *Region) unpublishedAge() time.Duration {
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

// overWindow reports whether the VM this region belongs to has held a write no
// checkpoint covers for longer than the configured window. It is asked before
// every dirty reservation, so it takes no lock a fault holds and asks the
// region's owner rather than knowing anything about VMs.
func (h *Host) overWindow(r *Region) bool {
	if h.cfg.LossWindow <= 0 {
		return false
	}
	since := r.OldestUnpublished()
	if !since.IsZero() && h.clock.Since(since) > h.cfg.LossWindow {
		// This region alone is past it, so what its siblings hold cannot make
		// the answer anything else. It is the case a store held back asks in,
		// over and over, and it costs nothing to answer.
		return true
	}
	h.mu.Lock()
	oldest := h.pressure.Oldest
	h.mu.Unlock()
	if oldest == nil {
		return false
	}
	since = older(since, oldest(r))
	return !since.IsZero() && h.clock.Since(since) > h.cfg.LossWindow
}

// windowRelief reports whether a checkpoint that ends this region's window is
// coming, asking for one where none is. Only a checkpoint of this VM ends it, so
// this region is the one offered — unlike the dirty budget, which any region's
// checkpoint can relieve and which therefore offers the largest dirty set first.
//
// A seal already under way is that checkpoint. So is one already draining,
// including a fork point's hold: the hold ends at its deadline and the parent
// is checkpointed then, which is a bound a store may wait under, where the dirty
// budget gets nothing back from it at all. What is left is a VM this host cannot
// checkpoint — its loop is off, or the region belongs to no VM it runs — and
// that is a stall.
func (h *Host) windowRelief(r *Region) bool {
	if sealing, draining := r.sealState(); sealing || draining != nil {
		return true
	}
	h.mu.Lock()
	request := h.pressure.Checkpoint
	h.mu.Unlock()
	if request == nil || !request(r) {
		return false
	}
	h.mu.Lock()
	h.stats.CheckpointRequests++
	h.mu.Unlock()
	return true
}
