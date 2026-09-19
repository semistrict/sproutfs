package vmmemory

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// A write fault is not always a store. KVM finishes a guest's cold read from a
// worker thread that always asks for the page writable, and an architecture can
// report a guest kernel's cache maintenance on a page it is about to execute as
// a write. The pager cannot tell those faults from real ones while they wait —
// the worker will not finish until the page is writable — so it copies, and
// tells afterwards: a sealed page whose bytes are the ones the page it was
// copied from still holds is not dirty, and the checkpoint publishes nothing
// for it.

// Settle drops from this checkpoint every page whose sealed bytes are the ones
// its origin still holds, and hands the guest's page back to that origin. It
// reports how many pages it dropped.
//
// The publication calls it once, before it enumerates the pages: behind the
// pause, with the guest running, so the seal it belongs to is unchanged and
// costs the same page-table work it always did. A fork point never calls it —
// it publishes nothing, and its children inherit an unchanged page as an
// unpublished one, which the next checkpoint of each settles.
//
// It reads no disk and no store and takes none of the pager's I/O permits, so a
// sealed page the pager has spilled is left alone: what it compares is two
// resident pages, and that comparison is the whole of what the upload is
// waiting for, so the pages are divided between Config.SettleWorkers workers
// rather than queued behind one. The workers share nothing but the count and
// the set this checkpoint will list, so what a settle leaves does not depend on
// the order they finish in.
func (c *RegionCheckpoint) Settle(ctx context.Context) (int, error) {
	r := c.region
	// The bindings must stay in existence for the whole walk, which is what a
	// fault holds the region live for; nothing wider is taken, so a seal, a
	// retire or a detach waits for this settle and never for a page of it.
	if err := r.live.RLock(ctx); err != nil {
		return 0, err
	}
	defer r.live.RUnlock()
	if err := r.serving(); err != nil {
		return 0, err
	}
	select {
	case <-c.done:
		// Retired, abandoned or discarded: these pages are the guest's own
		// state again or the volume's, and there is nothing left to settle.
		return 0, nil
	default:
	}
	if c.held.Load() {
		// A fork point's seal, which publishes nothing and whose pages its
		// children are reading. Taking one out from under them would take away
		// a page they map; the next checkpoint of each settles it instead.
		return 0, nil
	}
	held := c.sealedPages()
	dropped := make([]bool, len(held))
	failures := make([]error, len(held))
	workers := min(max(r.host.cfg.SettleWorkers, 1), len(held))
	var next atomic.Int64
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var worker settler
			for {
				i := int(next.Add(1)) - 1
				if i >= len(held) {
					return
				}
				dropped[i], failures[i] = worker.settle(ctx, c, held[i])
			}
		}()
	}
	wait.Wait()
	// The outcome is read back in page order, so a settle that failed reports
	// the same thing however its workers were scheduled.
	unchanged := 0
	for _, was := range dropped {
		if was {
			unchanged++
		}
	}
	if unchanged > 0 {
		c.forget(dropped)
		h := r.host
		h.mu.Lock()
		h.stats.UnchangedPages += uint64(unchanged)
		// A store waiting for the dirty budget or for this VM's loss window
		// waits on the host's own signal, and a settle that emptied the
		// checkpoint is exactly as good to it as a checkpoint that landed.
		h.signal()
		h.mu.Unlock()
	}
	return unchanged, errors.Join(failures...)
}

// settler is one worker. Where the arena cannot compare two of its own slots
// the comparison needs a page of each, which is made once and reused for every
// page that worker settles.
type settler struct{ first, second []byte }

// equal reports whether two arena slots hold the same bytes. Caller holds both
// pages' locks, so neither slot can be released or refilled while it runs.
func (s *settler) equal(ctx context.Context, h *Host, first, second int) (bool, error) {
	if comparing, ok := h.arena.(EqualArena); ok {
		return comparing.Equal(ctx, first, second)
	}
	if s.first == nil {
		s.first, s.second = make([]byte, PageSize), make([]byte, PageSize)
	}
	if err := h.arena.Read(ctx, first, s.first); err != nil {
		return false, err
	}
	if err := h.arena.Read(ctx, second, s.second); err != nil {
		return false, err
	}
	return bytes.Equal(s.first, s.second), nil
}

// settle compares one sealed page with the page it was copied from and reports
// whether it left the checkpoint.
//
// The two locks are taken origin first: an origin holds a published identity
// and a sealed page is private, a clean page never becomes private, and no
// other transition waits for a clean page while holding a private one — so
// clean before private is one order for every settle at once.
func (s *settler) settle(ctx context.Context, c *RegionCheckpoint, held *binding) (bool, error) {
	r := c.region
	h := r.host
	origin := r.originOf(held)
	if origin == nil || held.spillSlot < 0 {
		return false, nil
	}
	if err := origin.mu.Lock(ctx); err != nil {
		return false, err
	}
	defer h.unlock(origin)
	if !origin.published() || origin.slot < 0 {
		// Evicted, or no longer reachable by the name whose bytes these were:
		// there is nothing to compare against and nothing to re-share onto, so
		// the page is published exactly as it would have been before.
		return false, nil
	}
	pg, err := h.current(ctx, held)
	if err != nil {
		return false, err
	}
	if pg == nil {
		// Spilled since the seal. Reading it back is store I/O the settle does
		// not do, so the page is published as it is.
		return false, nil
	}
	defer h.unlock(pg)
	equal, err := s.equal(ctx, h, origin.slot, pg.slot)
	if err != nil || !equal {
		return false, err
	}
	if err := c.drop(ctx, held, pg, origin); err != nil {
		// Nothing left the checkpoint: the page is published as it would have
		// been, and the failure is the publication's to answer for.
		return false, err
	}
	return true, nil
}

// drop takes one unchanged page out of the checkpoint under the lock of the
// page it is held in. A guest that still shares the checkpoint's copy is put
// back on the origin: the bytes are identical and the sealed page is
// write-protected, so the swap is invisible to it, and the page is mapped
// rather than left missing on purpose — a missing page's next read would wait,
// go through the same worker and be copied again. A guest that stored into it
// between the seal and here copied away from the checkpoint already and keeps
// its own page, which the next checkpoint publishes.
//
// Either way the checkpoint's copy goes, and with it the dirty reservation it
// held. Caller holds both the sealed page and the origin.
func (c *RegionCheckpoint) drop(ctx context.Context, held *binding, pg, origin *resident) error {
	r := c.region
	h := r.host
	if b := r.lookupBinding(held.index); b != nil && r.heldBy(held.index, held) {
		if r.isMapped(b) {
			// One command replaces the mapping, and nothing is revoked first:
			// both slots hold the same bytes and the guest's access to them is
			// write-protected either way, so there is nothing to fence.
			if err := r.mapPages(ctx, held.index, origin.slot, 1, false); err != nil {
				return r.mappingFailed(err, func() { r.unmapPages(held.index, 1) })
			}
			if err := r.resolvePages(ctx, held.index, 1, false); err != nil {
				return r.fail(err)
			}
		}
		if err := h.unlink(ctx, b, pg); err != nil {
			return err
		}
		h.bind(b, origin)
		r.retireFromCheckpoint(b)
		h.touch(origin)
	}
	// The name a seal lent the page goes with the page.
	h.unshare(pg)
	if err := h.unlink(ctx, held, pg); err != nil {
		return err
	}
	h.releaseSpill(held.spillSlot)
	held.spillSlot, held.dirty, held.origin = -1, false, nil
	return nil
}

// sealedPages is the set this checkpoint holds, which a settle is what changes.
func (c *RegionCheckpoint) sealedPages() []*binding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pages
}

// forget rebuilds the set this checkpoint lists without the pages a settle
// dropped, in the order it had them, so the result is the workers' outcome and
// not their order. A checkpoint left holding nothing holds no unpublished write
// either, so the window it took off the region at the seal ends here: what the
// region reports from now on is its own stores since.
func (c *RegionCheckpoint) forget(dropped []bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pages := make([]*binding, 0, len(c.pages))
	for i, held := range c.pages {
		if i < len(dropped) && dropped[i] {
			delete(c.byPage, held.index)
			continue
		}
		pages = append(pages, held)
	}
	c.pages = pages
	if len(c.pages) == 0 {
		c.dirtySince = time.Time{}
	}
}

// since is when the oldest write this checkpoint still holds was made, zero
// where it holds none. A settle can empty the set, so it is read under the
// checkpoint's own lock rather than off the field.
func (c *RegionCheckpoint) since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirtySince
}
