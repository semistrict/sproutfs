package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A migration destination's peer backing over the zircon core, as the
// current core takes it (window.go, fault.go): a page the backing serves
// from the source's own dirty pages is the guest's state since the source's
// last checkpoint, which no checkpoint has, so it enters the region as its
// own dirty state, a Dirty page of its layer under a dirty reservation, and
// the backing is told the region went on to hold it.

// publishPrivate takes one page a load brought in and the backing reported
// another host's, as windowPlan.publish's private case does. The faulting
// page brings the reservation the waiting path admitted it under; any other
// page takes only one that is free now, and is left to a later fault where
// none is.
func (p *zplan) publishPrivate(ctx context.Context, page uint64, data []byte) error {
	z := p.z
	r := z.region
	h := r.host
	i := page - p.start
	at := p.reserved[i]
	if at.file != r.privateFile() {
		// The extents named this page the volume's, and the load found it
		// another host's: its bytes go in the region's own file.
		var moved bool
		if at, moved = p.ownInstead(page); !moved {
			if page != p.fault {
				p.fresh[i] = false
				return nil
			}
			return errUnpublishedReservation
		}
	}
	var spill reservation
	if page == p.fault && p.spill != nil && !p.spill.none() {
		spill, *p.spill = *p.spill, noReservation
	} else {
		taken, err := h.tryTakeSpill()
		if err != nil {
			if page != p.fault && errors.Is(err, ErrCapacity) {
				p.fresh[i] = false
				return nil
			}
			if !errors.Is(err, ErrCapacity) {
				return err
			}
			// The extents did not say this page was the source's, so the
			// fault holds no reservation for it. It takes one and retries.
			return errUnpublishedReservation
		}
		spill = taken
	}
	p.reserved[i] = fileSlot{slot: -1}
	frame, err := z.host.newFrame(ctx, at, data, r.kind)
	if err != nil {
		h.releaseSpill(spill)
		return err
	}
	frameOf(frame).layer = z
	p.locked = append(p.locked, frame)
	if err := z.supplyDirty(ctx, page, []*zirconvm.VmPage{frame}); err != nil {
		h.releaseSpill(spill)
		return err
	}
	if page == p.fault {
		// The supply answered the faulting page's own read request.
		p.request.answer(nil)
	}
	z.mu.Lock()
	b := z.bindingLocked(page)
	z.uncoldLocked(b)
	b.zero, b.checkpoint, b.spill, b.dirty, b.origin, b.ahead = false, nil, spill, true, nil, false
	z.noteDirtyLocked(b)
	z.mu.Unlock()
	h.mu.Lock()
	if b.page != nil {
		z.host.unaliasLocked(b)
	}
	z.host.aliasLocked(b, frame)
	h.mu.Unlock()
	p.pages[i], p.fresh[i], p.private[i] = frame, true, true
	return nil
}

// ownInstead is windowPlan.ownInstead over the zircon core: a page's slot
// moves from a file another region may read to the region's own file. It
// takes nothing it cannot have at once.
func (p *zplan) ownInstead(page uint64) (fileSlot, bool) {
	r := p.z.region
	h := r.host
	i := page - p.start
	h.mu.Lock()
	defer h.mu.Unlock()
	h.putFree(p.reserved[i])
	p.reserved[i] = fileSlot{slot: -1}
	for _, at := range r.ownPlaces(page, false) {
		if taken := h.takeOwnLocked(r, page, at); taken.slot >= 0 {
			return taken, true
		}
	}
	return fileSlot{}, false
}
