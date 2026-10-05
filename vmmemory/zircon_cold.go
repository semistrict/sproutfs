package vmmemory

import (
	"context"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Cold copies over the zircon core, as cold.go keeps them for the current
// core: a copy a store trap made of a root's page is not yet known to be the
// guest's state, and pins the page it was copied from in the zero-fork queue,
// outside the reclaim queues an eviction takes from while anything else can
// go, until it is compared with it.

// pin keeps origin in the arena while b's cold copy is compared with it: the
// first pin moves it to the zero-fork queue.
func (z *zirconHost) pin(origin *zirconvm.VmPage, b *zbinding) {
	h := z.host
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if f.coldCopies == nil {
		f.coldCopies = make(map[*zbinding]struct{})
	}
	first := len(f.coldCopies) == 0
	f.coldCopies[b] = struct{}{}
	if first {
		z.node.PageQueues().MoveAnonymousToAnonymousZeroFork(origin)
	}
}

// unpin is the reverse of pin: the last unpin moves origin back to the queue
// it belongs in, the don't-need queue where nothing maps it.
func (z *zirconHost) unpin(origin *zirconvm.VmPage, b *zbinding) {
	h := z.host
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if _, pinned := f.coldCopies[b]; !pinned {
		return
	}
	delete(f.coldCopies, b)
	if len(f.coldCopies) == 0 {
		z.unpinnedLocked(origin)
	}
}

// unpinnedLocked moves a page no cold copy pins any more back where it
// belongs. Caller holds h.pinMu.
func (z *zirconHost) unpinnedLocked(origin *zirconvm.VmPage) {
	if _, queued := z.node.PageQueues().Backlink(origin); !queued {
		return
	}
	if frameOf(origin).aliases.len() == 0 {
		z.node.PageQueues().MoveToReclaimDontNeed(origin)
		return
	}
	z.node.PageQueues().MoveToReclaim(origin)
}

// uncoldLocked ends b's copy being cold, if it is, as
// MemoryRegion.uncoldLocked does. Caller holds z.mu.
func (z *zirconRegion) uncoldLocked(b *zbinding) {
	if !b.cold {
		return
	}
	b.cold = false
	delete(z.coldPages, b.index)
	if b.origin != nil {
		z.host.unpin(b.origin, b)
	}
}

// leaveOutColdCopies is MemoryRegion.leaveOutColdCopies over the zircon core:
// every cold copy of the set a seal took that still holds its origin's bytes
// is left out of it. No copy of this core is cold until its store path marks
// them so.
func (z *zirconRegion) leaveOutColdCopies(_ context.Context, pending map[uint64]*zbinding) (int, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	for index, b := range z.coldPages {
		if pending[index] == b {
			panic("vmmemory: a cold copy of the zircon core reached a seal")
		}
	}
	return 0, nil
}
