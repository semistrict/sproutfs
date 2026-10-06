package vmmemory

import (
	"context"
	"errors"

	"github.com/semistrict/sproutfs/control"
)

// pageKey identifies immutable bytes by the store page object that holds them.
// A pager page is exactly one store page, so one identity covers a whole pageKey.
type pageKey struct {
	id control.Identity
}

func (f pageKey) zero() bool { return f.id.Zero }

// abandonSlots gives back reserved slots whose contents failed to arrive, or
// that their reserver turned out not to need. A failed write may have allocated
// partial contents, so each is punched before it is accounted free; a failed
// punch makes the host terminal.
func (h *Host) abandonSlots(ctx context.Context, at fileSlot, count int, err error) error {
	var cleanup error
	for i := range count {
		cleanup = errors.Join(cleanup, at.file.Release(context.WithoutCancel(ctx), at.slot+i))
	}
	h.mu.Lock()
	if cleanup != nil {
		h.err = errors.Join(err, cleanup)
	} else {
		for i := range count {
			h.putFree(at.plus(i))
		}
	}
	h.signal()
	h.mu.Unlock()
	return errors.Join(err, cleanup)
}

// storedPage is the identity the volume now gives one page, which is what
// decides whether its resident page can be shared.
type storedPage struct {
	id     pageKey
	stored bool
}

// ErrUndroppable reports a retire that would have given up the only copy of
// bytes a guest wrote. It fails the retire rather than the VM: the checkpoint is
// durable either way, the page stays sealed and the guest keeps its memory, and
// the pager reports why its pages are still sealed.
var ErrUndroppable = errors.New("a page the volume holds no object for is not zeros")

// allZero reports whether a page a checkpoint read holds only zeros. It feeds
// the write-ahead statistics alone: no sharing or other decision reads contents.
func allZero(data []byte) bool {
	for _, v := range data {
		if v != 0 {
			return false
		}
	}
	return true
}
