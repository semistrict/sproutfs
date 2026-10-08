package vmmemory

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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
// trap is a store into a page the guest maps, which KVM reports only for a
// real store, so its copy is never cold.
//
// A pin keeps the origin in the zero-fork queue, outside the reclaim queues an
// eviction takes from while anything else can go. The give-back is Zircon's
// zero-page scan widened to the origin (DedupZeroPage,
// vm_cow_pages.cc:1363-1437): it checks the copy, write-protects it, checks
// again, and puts the guest back on the origin.
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

// GiveBackColdCopies gives back at once the cold copies recorded and not yet
// taken, and reports how many went back: see giveback.go. A session takes
// them itself, coldCopyAge after they are made; this is for a pager with no
// session, and for a test.
func (r *MemoryRegion) GiveBackColdCopies(ctx context.Context) (int, error) {
	return r.givingBack(ctx, r.takeColdCopies)
}

// takeColdCopies is the cold copies recorded since it was last called, in page
// order. A session's worker takes them and gives them back coldCopyAge later.
func (r *MemoryRegion) takeColdCopies() []uint64 {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pages := slices.Sorted(maps.Keys(r.coldCopies))
	clear(r.coldCopies)
	return pages
}

// pin keeps origin in the arena while b's cold copy is compared with it: the
// first pin moves it to the zero-fork queue. Caller holds origin's lock.
func (h *Host) pin(origin *zirconvm.VmPage, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if f.coldCopies == nil {
		f.coldCopies = make(map[*binding]struct{})
	}
	first := len(f.coldCopies) == 0
	f.coldCopies[b] = struct{}{}
	if first {
		h.node.PageQueues().MoveAnonymousToAnonymousZeroFork(origin)
	}
}

// unpin is the reverse of pin: the last unpin moves origin back to the queue
// it belongs in.
func (h *Host) unpin(origin *zirconvm.VmPage, b *binding) {
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f := frameOf(origin)
	if _, pinned := f.coldCopies[b]; !pinned {
		return
	}
	delete(f.coldCopies, b)
	if len(f.coldCopies) == 0 {
		h.unpinnedLocked(origin)
	}
}

// unpinnedLocked moves a page no cold copy pins any more back where it
// belongs: the don't-need queue if it is idle, the newest reclaim queue if
// not. Caller holds h.pinMu.
func (h *Host) unpinnedLocked(origin *zirconvm.VmPage) {
	if _, queued := h.node.PageQueues().Backlink(origin); !queued {
		return
	}
	if frameOf(origin).idle {
		h.node.PageQueues().MoveToReclaimDontNeed(origin)
		return
	}
	h.node.PageQueues().MoveToReclaim(origin)
}

// dropCold lets every cold copy compared with origin go on without it, as
// Host.dropCold does: origin is going although it is pinned. The copies stay
// cold, and are compared with the bytes their volume holds for their page.
func (h *Host) dropCold(origin *zirconvm.VmPage) {
	f := frameOf(origin)
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(f.coldCopies))
	f.coldCopies = nil
	h.pinMu.Unlock()
	for _, b := range copies {
		q := b.region
		q.bindingsMu.Lock()
		if b.cold && b.origin == origin {
			b.origin = nil
		}
		q.bindingsMu.Unlock()
	}
}

// moveCold makes every cold copy compared with from compared with to instead,
// which holds the same bytes and takes from's place. Caller holds both pages'
// locks.
func (h *Host) moveCold(from, to *zirconvm.VmPage) {
	ff, tf := frameOf(from), frameOf(to)
	h.pinMu.Lock()
	copies := slices.Collect(maps.Keys(ff.coldCopies))
	ff.coldCopies = nil
	if len(copies) > 0 {
		if tf.coldCopies == nil {
			tf.coldCopies = make(map[*binding]struct{})
		}
		if len(tf.coldCopies) == 0 {
			h.node.PageQueues().MoveAnonymousToAnonymousZeroFork(to)
		}
	}
	for _, b := range copies {
		tf.coldCopies[b] = struct{}{}
	}
	h.pinMu.Unlock()
	for _, b := range copies {
		q := b.region
		q.bindingsMu.Lock()
		if b.origin == from {
			b.origin = to
		}
		q.bindingsMu.Unlock()
	}
}

// markCold makes b's copy of origin cold, and records it for its session to
// give back, as MemoryRegion.markCold does. It reports whether it did: a copy
// that no longer remembers origin is not one.
func (r *MemoryRegion) markCold(b *binding, origin *zirconvm.VmPage) bool {
	h := r.host
	r.bindingsMu.Lock()
	h.pinMu.Lock()
	_, pinned := frameOf(origin).coldCopies[b]
	h.pinMu.Unlock()
	if b.origin != origin || !b.writable() {
		r.bindingsMu.Unlock()
		return false
	}
	if !pinned {
		// An eviction with nothing else to take took origin while the copy
		// was being made: the copy is compared with its volume instead.
		b.origin = nil
	}
	b.cold, b.coldAt = true, h.clock.Now().UnixNano()
	if r.coldPages == nil {
		r.coldPages = make(map[uint64]*binding)
	}
	r.coldPages[b.index] = b
	if r.coldCopies == nil {
		r.coldCopies = make(map[uint64]struct{})
	}
	r.coldCopies[b.index] = struct{}{}
	r.bindingsMu.Unlock()
	select {
	case r.coldCopied <- struct{}{}:
	default:
	}
	return true
}

// requeueCold hands a cold copy back to its session when a give-back could not
// finish it.
func (r *MemoryRegion) requeueCold(b *binding) {
	r.bindingsMu.Lock()
	queued := b.cold && b.writable()
	if queued {
		if r.coldCopies == nil {
			r.coldCopies = make(map[uint64]struct{})
		}
		r.coldCopies[b.index] = struct{}{}
	}
	r.bindingsMu.Unlock()
	if queued {
		select {
		case r.coldCopied <- struct{}{}:
		default:
		}
	}
}

// uncoldLocked ends b's copy being cold, if it is, as
// MemoryRegion.uncoldLocked does. Caller holds r.bindingsMu.
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

// isCold reports whether b's copy is cold and still its own dirty state.
func (r *MemoryRegion) isCold(b *binding) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.cold && b.writable()
}

// coldSince reports when b's copy became cold.
func (r *MemoryRegion) coldSince(b *binding) time.Time {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return time.Unix(0, b.coldAt)
}

// originOf reports the page b was copied from, nil where it was copied from
// nothing a comparison may use.
func (r *MemoryRegion) originOf(b *binding) *zirconvm.VmPage {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.origin
}

// forgetOrigin stops comparing b with origin: the guest changed it, or origin
// has gone. It is left alone where b has been copied again since.
func (r *MemoryRegion) forgetOrigin(b *binding, origin *zirconvm.VmPage) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if b.origin == origin {
		r.uncoldLocked(b)
		b.origin = nil
	}
}

// endDirty ends b's dirty epoch with no checkpoint, because its bytes are the
// ones the page it shares again holds, and reports the reservation it was
// admitted under, for the caller to give back. A region left with no dirty
// page holds no unpublished write, so its loss window ends too.
func (r *MemoryRegion) endDirty(b *binding) reservation {
	r.host.probe.retired(b)
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	r.uncoldLocked(b)
	spill := b.spill
	b.spill, b.dirty, b.ahead, b.origin = noReservation, false, false, nil
	delete(r.dirtySet, b.index)
	r.noteSealableLocked(b)
	// Its bytes are its origin's again, which no journal entry has to bring
	// back: a copy a capture took is protected, so never a cold copy.
	r.forgetJournaledLocked(b.index)
	if len(r.dirtySet) == 0 {
		r.dirtySince = time.Time{}
	}
	return spill
}

// leaveOutColdCopies leaves out of the set a seal took every cold copy that
// still holds its origin's bytes, which stays the guest's, cold and writable.
// Caller holds the region exclusively.
//
// The cold copies are listed under one hold of bindingsMu and compared, each
// with its pages locked, after it. With the region held exclusively no fault,
// store, give-back, capture or other seal runs meanwhile, so each copy stays
// in the set, dirty and the region's own. An eviction still may: it spills a
// copy, or takes its origin and makes it compared with its volume, or a move
// gives it another origin. stillOrigins asks which under the pages' locks, and
// uncoldLocked unpins whatever origin the copy names by then.
func (r *MemoryRegion) leaveOutColdCopies(ctx context.Context, pending map[uint64]*binding) (int, error) {
	h := r.host
	r.bindingsMu.Lock()
	var cold []*binding
	for index, b := range r.coldPages {
		if pending[index] == b {
			cold = append(cold, b)
		}
	}
	r.bindingsMu.Unlock()
	slices.SortFunc(cold, func(a, b *binding) int { return int(a.index) - int(b.index) })
	var buffers settler
	var unchanged []*binding
	for _, b := range cold {
		same, err := r.stillOrigins(ctx, b, &buffers)
		if err != nil {
			// The copies left out so far go back in the set, which then
			// takes them as it takes every other dirty page: out of both
			// the set and the region's dirty set, a later store into one
			// would reach no checkpoint.
			if !sim.Bug(ctx, "pager-seal-drops-the-cold-copies-before-a-failed-compare") {
				for _, left := range unchanged {
					pending[left.index] = left
				}
			}
			return 0, err
		}
		if same {
			delete(pending, b.index)
			unchanged = append(unchanged, b)
			continue
		}
		r.bindingsMu.Lock()
		r.uncoldLocked(b)
		r.bindingsMu.Unlock()
	}
	if len(unchanged) == 0 {
		return 0, nil
	}
	// The copies left out are the region's dirty pages again under a hold of
	// their own. Nothing but an eviction ran since they were compared, and an
	// eviction leaves a page dirty and its own: noteSealableLocked reads
	// whether each is mapped now, and unprotectMapped asks again under the
	// region's protection, which every revocation holds.
	r.bindingsMu.Lock()
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*binding)
	}
	for _, b := range unchanged {
		r.dirtySet[b.index] = b
		r.noteSealableLocked(b)
	}
	r.bindingsMu.Unlock()
	if err := r.unprotectMapped(ctx, unchanged); err != nil {
		return 0, err
	}
	h.mu.Lock()
	h.stats.UnchangedPages += uint64(len(unchanged))
	h.mu.Unlock()
	return len(unchanged), nil
}

// stillOrigins reports whether a cold copy holds exactly its origin's bytes.
// An origin no longer there to compare with ends the copy being cold, and it
// is reported changed. Caller holds the region exclusively.
func (r *MemoryRegion) stillOrigins(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	origin := r.originOf(b)
	if origin == nil {
		return r.volumeHolds(ctx, b, buffers)
	}
	if err := r.host.lockPage(ctx, origin); err != nil {
		return false, err
	}
	defer r.host.unlockPage(origin)
	if !r.host.published(origin) {
		return false, nil
	}
	page, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		defer r.host.unlockPage(page)
		return buffers.equal(ctx, h, frameOf(origin).fileSlot, frameOf(page).fileSlot)
	}
	// Spilled: the reservation holds the copy's bytes.
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	f := frameOf(origin)
	if err := f.file.Read(ctx, f.slot, buffers.first); err != nil {
		return false, err
	}
	if err := h.readSpill(ctx, r.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}

// unprotectMapped takes the write protection the pause put on the pages of
// bindings off those the guest still maps, one command per page, with the
// region's protection held exclusively, as MemoryRegion.unprotectMapped does.
// Caller holds the region exclusively.
func (r *MemoryRegion) unprotectMapped(ctx context.Context, bindings []*binding) error {
	if err := wlockAdmitted(ctx, "vmmemory/protection", r.protectMu); err != nil {
		return err
	}
	defer r.protectMu.Unlock()
	for _, b := range bindings {
		if !r.isMapped(b) {
			continue
		}
		if err := r.resolvePages(ctx, b.index, 1, true); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// volumeHolds reports whether a cold copy whose origin has gone holds exactly
// the bytes its volume holds for its page. A backing that may answer with
// another host's bytes has no such guarantee, and the copy is reported
// changed.
func (r *MemoryRegion) volumeHolds(ctx context.Context, b *binding, buffers *settler) (bool, error) {
	h := r.host
	if r.peer {
		return false, nil
	}
	if buffers.first == nil {
		buffers.first, buffers.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if _, err := r.loadBacking(ctx, b.index*h.pageSize, buffers.first); err != nil {
		return false, err
	}
	page, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page != nil {
		defer r.host.unlockPage(page)
		f := frameOf(page)
		if err := f.file.Read(ctx, f.slot, buffers.second); err != nil {
			return false, err
		}
	} else if err := h.readSpill(ctx, r.spillOf(b), buffers.second); err != nil {
		return false, err
	}
	return slices.Equal(buffers.first, buffers.second), nil
}
