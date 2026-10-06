package vmmemory

import (
	"context"
	"maps"
	"slices"
	"time"
)

// Why a copy is cold before it is dirty.
//
// A write fault on a page the guest does not map is a store trap, and it is not
// always a store. On x86-64 KVM finishes a guest's cold read from a worker,
// async_pf_execute, that asks for the page writable, and the pager cannot tell
// that fault from a store while it waits: the worker does not finish until the
// page is writable, so the pager copies. A guest reading what it inherited
// makes these copies by the thousand. Kept as ordinary dirty pages, they fill
// the arena, are spilled, and are uploaded by the next checkpoint of a disk or
// fetched from the parent by every child of a fork, although the guest never
// changed a byte of them.
//
// So a copy a store trap makes of a published page is cold: private and
// writable, but not yet known to be the guest's state. It becomes an ordinary
// dirty page only when a comparison with the page it was copied from finds the
// guest changed it. Until then:
//
//   - Its origin is pinned. An eviction takes the page a cold copy was made
//     from only when nothing else can go. Without it, the copy is compared
//     with the bytes its volume holds for the page, which is a backing read:
//     see volumeHolds.
//   - An eviction that picks the copy itself gives it back rather than spill
//     it, once it is coldCopyAge old and where it can take the locks for that
//     without waiting: see giveBackVictim. A copy it could not give back is
//     spilled, and its session and the seal read it back to compare it.
//   - Its session gives it back soon after it is made: see giveBackColdCopies.
//   - A seal compares every cold copy in the set it took and leaves out each one
//     that still holds its origin's bytes: see leaveOutColdCopies. So no
//     checkpoint, whether a capture, a disk's interval checkpoint or a fork
//     point, holds a cold copy the guest did not change, and none is uploaded
//     or fetched.
//
// A move of the origin moves its pins with it. An origin that goes anyway
// leaves its copies cold, compared with their volume from then on. A protect
// trap is a store into a page the guest
// maps, which KVM reports only for a real store, so its copy is never cold.
//
// A cold copy is given back only once it is coldCopyAge old. KVM's worker takes
// the page writable and only then does the vCPU retry its access, so a copy just
// made holds the origin's bytes whether the guest meant to read or to store.
// Compared at once, a store's copy would go back too, and the store would trap
// on the origin and copy again: nothing lost, but every cold store copied
// twice, and a fork's resume is mostly cold stores. By coldCopyAge the vCPU has
// retried, so a store's copy differs and is kept, and a read's is given back. A
// seal compares sooner, because it has to: a store that has not landed by then
// copies again after the checkpoint.

// coldCopyAge is how old a cold copy is before its session gives it back.
var coldCopyAge = 200 * time.Millisecond

// unpin is the reverse of pin: the last unpin moves origin back to the queue
// it belongs in.
func (h *Host) unpin(origin *resident, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	if _, pinned := origin.coldCopies[b]; !pinned {
		return
	}
	delete(origin.coldCopies, b)
	if len(origin.coldCopies) == 0 {
		h.unpinnedLocked(origin)
	}
}

// uncoldLocked ends b's copy being cold, if it is: a comparison found it
// changed, it was given back, or its dirty epoch or its origin ended some other
// way. Every transition that changes a page's origin or ends its dirty epoch
// calls it first. Caller holds bindingsMu.
func (r *MemoryRegion) uncoldLocked(b *binding) {
	if !b.cold {
		return
	}
	b.cold = false
	delete(r.coldPages, b.index)
	if b.origin != nil {
		r.host.unpin(b.origin, b)
	}
}

// dropCold lets every cold copy compared with pg go on without it: pg is
// going although it is pinned, because an eviction had nothing else to take,
// its identity was dropped, or a move put its bytes elsewhere. The copies stay
// cold, and are compared with the bytes their volume holds for their page
// instead, which are what pg held: see volumeHolds. Caller holds pg's lock.
func (h *Host) dropCold(pg *resident) {
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(pg.coldCopies))
	pg.coldCopies = nil
	if len(copies) > 0 {
		h.unpinnedLocked(pg)
	}
	h.pinMu.Unlock()
	for _, b := range copies {
		r := b.memoryRegion
		r.bindingsMu.Lock()
		if b.cold && b.origin == pg {
			b.origin = nil
		}
		r.bindingsMu.Unlock()
	}
}

// GiveBackColdCopies gives back at once the cold copies recorded and not yet
// taken, and reports how many went back: see giveback.go. A session takes
// them itself, coldCopyAge after they are made; this is for a pager with no
// session, and for a test.
func (r *MemoryRegion) GiveBackColdCopies(ctx context.Context) (int, error) {
	z := r.zircon

	return z.giveBackColdCopies(ctx)
}
