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
	equal := make([]*resident, len(held))
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
				equal[i], failures[i] = worker.compare(ctx, c, held[i])
			}
		}()
	}
	wait.Wait()
	// The comparison holds no page across the walk, so what it decided is
	// applied afterwards, in page order and in bounded batches: a settle of a
	// guest's whole working set is thousands of pages at 4 KiB, and revoking
	// them one round trip at a time is a stall the guest feels.
	if err := c.reshare(ctx, held, equal, dropped); err != nil {
		failures = append(failures, err)
	}
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
		s.first, s.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if err := h.arena.Read(ctx, first, s.first); err != nil {
		return false, err
	}
	if err := h.arena.Read(ctx, second, s.second); err != nil {
		return false, err
	}
	return bytes.Equal(s.first, s.second), nil
}

// compare reports the page one sealed page was copied from when the two hold
// the same bytes, and nil when this page stays in the checkpoint. It mutates
// nothing: what it decides is applied by reshare, in page order.
//
// The two locks are taken origin first: an origin holds a published identity
// and a sealed page is private, a clean page never becomes private, and no
// other transition waits for a clean page while holding a private one — so
// clean before private is one order for every settle at once.
func (s *settler) compare(ctx context.Context, c *RegionCheckpoint, held *binding) (*resident, error) {
	r := c.region
	h := r.host
	origin := r.originOf(held)
	if origin == nil || held.spillSlot < 0 {
		return nil, nil
	}
	if err := origin.mu.Lock(ctx); err != nil {
		return nil, err
	}
	defer h.unlock(origin)
	if !origin.published() || origin.slot < 0 {
		// Evicted, or no longer reachable by the name whose bytes these were:
		// there is nothing to compare against and nothing to re-share onto, so
		// the page is published exactly as it would have been before.
		return nil, nil
	}
	pg, err := h.current(ctx, held)
	if err != nil {
		return nil, err
	}
	if pg == nil {
		// Spilled since the seal. Reading it back is store I/O the settle does
		// not do, so the page is published as it is.
		return nil, nil
	}
	defer h.unlock(pg)
	same, err := s.equal(ctx, h, origin.slot, pg.slot)
	if err != nil || !same {
		return nil, err
	}
	return origin, nil
}

// reshare applies what the comparison decided, in bounded batches under the
// region: every page of a batch is revoked by one command per run of
// consecutive pages, and only then does each page leave the checkpoint. It
// records in dropped which pages did.
//
// The region is taken exclusively, as a retire takes it and for the same
// reason: the pages are revoked as a batch, so no fault may be part way
// through one of them, and the region is given back between batches so a fault
// waits for one batch rather than for the walk. Nothing here reads a byte.
//
// The comparison released its locks, so a guest may have stored into one of
// these pages since. That store copied away from the checkpoint's copy, which
// still holds the bytes that were compared, so the copy still leaves the
// checkpoint — the guest simply keeps the page it made instead of going back
// to the origin.
func (c *RegionCheckpoint) reshare(ctx context.Context, held []*binding, equal []*resident, dropped []bool) error {
	r := c.region
	pending := make([]int, 0, len(held))
	for i, origin := range equal {
		if origin != nil {
			pending = append(pending, i)
		}
	}
	for len(pending) > 0 {
		count := min(len(pending), revokeBatchPages)
		batch := pending[:count]
		if err := r.mu.Lock(ctx); err != nil {
			return err
		}
		err := func() error {
			defer r.mu.Unlock()
			if err := r.ready(); err != nil {
				return err
			}
			return c.reshareBatch(ctx, held, equal, dropped, batch)
		}()
		if err != nil {
			return err
		}
		pending = pending[count:]
	}
	return nil
}

// reshareBatch is one batch of reshare, with the region held exclusively. It
// locks every page it will touch for the whole batch, so the revocation that
// covers them all is issued while none of them can change underneath it.
func (c *RegionCheckpoint) reshareBatch(ctx context.Context, held []*binding, equal []*resident, dropped []bool, batch []int) error {
	r := c.region
	h := r.host
	locked := make(map[*resident]bool, 2*len(batch))
	defer func() {
		pages := make([]*resident, 0, len(locked))
		for pg := range locked {
			pages = append(pages, pg)
		}
		h.unlockAll(pages)
	}()
	// The pages this batch re-shares, and the guest bindings whose mappings of
	// them the revocation below takes away.
	type ready struct {
		index  int
		copied *resident
		origin *resident
		guest  *binding
	}
	var applying []ready
	var guests []*binding
	for _, i := range batch {
		origin := equal[i]
		if !locked[origin] {
			if err := origin.mu.Lock(ctx); err != nil {
				return err
			}
			locked[origin] = true
		}
		if !origin.published() || origin.slot < 0 {
			// Evicted since the comparison: there is nothing to re-share onto,
			// so this page is published as it would have been.
			continue
		}
		pg, err := h.current(ctx, held[i])
		if err != nil {
			return err
		}
		if pg == nil {
			continue // spilled since the comparison
		}
		if locked[pg] {
			// One resident page reached twice in a batch is one this walk has
			// already taken; current returned it locked a second time.
			h.unlock(pg)
		} else {
			locked[pg] = true
		}
		entry := ready{index: i, copied: pg, origin: origin}
		if b := r.lookupBinding(held[i].index); b != nil && r.heldBy(held[i].index, held[i]) {
			entry.guest = b
			if r.isMapped(b) {
				guests = append(guests, b)
			}
		}
		applying = append(applying, entry)
	}
	// One command per run of consecutive pages. The guest maps the copy it is
	// losing, so the mapping is taken away rather than swapped underneath it:
	// see drop.
	if err := r.revokeBindings(ctx, guests); err != nil {
		return err
	}
	for _, entry := range applying {
		if err := c.drop(ctx, held[entry.index], entry.copied, entry.origin, entry.guest); err != nil {
			return err
		}
		dropped[entry.index] = true
	}
	return nil
}

// drop takes one unchanged page out of the checkpoint. A guest that still
// shares the checkpoint's copy — which is what guest names, or nil where the
// guest has stored into the page since — is put back on the origin: the
// binding takes the origin as its resident page and becomes clean. Its mapping
// of the copy has already been taken away by the batch's revocation, because
// the page a guest maps is taken away and never swapped underneath it: a
// revocation is the one replacement that installs no page table and wakes
// nothing, and the guest's next access reaches the origin through the fault
// path, under the window that serializes every mapping of that page against
// every other. Installing the origin in its place — one command, no fence, the
// bytes identical and the page write-protected either way — is what the settle
// used to do, and it is a large part of an open defect rather than all of it:
// see docs/open-work.md, which carries the rates. Revoking is not a fix for
// that defect.
//
// Either way the checkpoint's copy goes, and with it the dirty reservation it
// held. Caller holds the region exclusively and both resident pages.
func (c *RegionCheckpoint) drop(ctx context.Context, held *binding, pg, origin *resident, guest *binding) error {
	r := c.region
	h := r.host
	note(r, held.index, "settle-drop", pg.slot, origin.slot)
	if guest != nil {
		if err := h.unlink(ctx, guest, pg); err != nil {
			return err
		}
		// The one place the pager hands a guest back an older page on purpose:
		// the audit checks the bytes here rather than trusting the comparison
		// that chose this page, and dates the origin by the copy once they
		// agree. See probe_on.go.
		if found := h.probe.reshared(ctx, h, pg, origin); found != "" {
			panic(found)
		}
		h.bind(guest, origin)
		r.retireFromCheckpoint(guest)
		h.probe.retired(guest)
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
