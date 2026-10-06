package vmmemory

import (
	"errors"
	"time"
)

// The zircon core: faults and stores over the region's layer and the identity
// roots of internal/zirconvm (plans/zircon-pager-port-2026-10-05.md, steps
// 10 to 12). It runs beside the current core, selected by Config.Core, and
// serves what the steps of the port have moved onto it. What stays the
// pager's own is shared with the current core: the arena and its files,
// isolation, placement, pressure, the connection, and the policy of which
// pages a fault reads and in what order, which moves as it is.
//
// A page is Zircon's: a VmPage whose Frame is a slot of an arena file (a
// frame). A published checkpoint's pages of one volume are an identity root,
// whose page source is the pager, and a memory region's own pages are its
// layer, whose lookup falls through to the identity root of each offset it
// holds nothing at. What Zircon has no place for stays in a binding beside the
// layer: whether the page is mapped, and which page it maps.
//
// The object locks are Zircon's: the layer's, and each root's. No object lock
// is held across a mapping command or a backing read: a fault collects its
// commands while it holds them and issues them after, as DeferredOps does, and
// the window's stripe, which the fault holds from start to end, keeps two
// faults of one window from issuing commands out of order. Lock order is the
// layer, then a root, then Host.mu, then MemoryRegion.bindingsMu. A page's own lock
// (zframe.mu), which an eviction holds across taking every mapping of the
// page away, is taken before all of them.
//
// Checkpoints are zircon_checkpoint.go's, and eviction zircon_evict.go's.

// closeRoots is what Close does before it gives back every slot: once no
// region is attached, the identity roots go, and their pages give their slots
// back as they go.
func (h *Host) closeRoots() error {
	h.mu.Lock()
	if h.logical != 0 {
		h.mu.Unlock()
		return errors.New("managed-memory regions still attached")
	}
	roots := h.roots
	h.roots = make(map[rootKey]*identityRoot)
	h.mu.Unlock()
	// Every page of a root goes back with it, and with its slot its count
	// of the roots' pages and of the idle ones (releaseFrame).
	for _, root := range roots {
		root.object.Destroy()
	}
	return nil
}

// The loss window over the zircon core: one timestamp per region, the
// oldest write it holds that no checkpoint covers, which a seal hands to its
// checkpoint and an abandoned checkpoint hands back, as losswindow.go keeps
// it for the current core.

// takeDirtySince is MemoryRegion.takeDirtySince over the zircon core.
func (r *MemoryRegion) takeDirtySince() time.Time {
	r.bindingsMu.Lock()
	since := r.dirtySince
	r.dirtySince = time.Time{}
	r.bindingsMu.Unlock()
	r.windowMu.Lock()
	r.windowAsked = false
	r.windowMu.Unlock()
	return since
}

// restoreDirtySince is MemoryRegion.restoreDirtySince over the zircon core.
func (r *MemoryRegion) restoreDirtySince(since time.Time) {
	r.bindingsMu.Lock()
	r.dirtySince = older(r.dirtySince, since)
	r.bindingsMu.Unlock()
	r.windowMu.Lock()
	r.windowAsked = false
	r.windowMu.Unlock()
}

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

// writable reports whether the guest may store into b's page where it is:
// its own dirty state, which no checkpoint still holds.
func (b *zbinding) writable() bool { return b.dirty && b.checkpoint == nil }
