package vmmemory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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
func (c *MemoryRegionCheckpoint) Settle(ctx context.Context) (int, error) {
	r := c.memoryRegion
	h := r.host
	if err := r.live.RLock(ctx); err != nil {
		return 0, err
	}
	defer r.live.RUnlock()
	if err := r.serving(); err != nil {
		return 0, err
	}
	select {
	case <-c.done:
		return 0, nil
	default:
	}
	if c.held.Load() {
		// A fork point's seal, whose pages its children are reading.
		return 0, nil
	}
	copies := c.copiesOf()
	equal := make([]*zirconvm.VmPage, len(copies))
	dropped := make([]bool, len(copies))
	failures := make([]error, len(copies))
	workers := min(max(h.cfg.SettleWorkers, 1), len(copies))
	measuring := h.measuring()
	var changed, measured, unmeasured atomic.Uint64
	var next atomic.Int64
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var worker settler
			for {
				i := int(next.Add(1)) - 1
				if i >= len(copies) {
					return
				}
				if measuring {
					blocks, known, err := r.changedBlocks(ctx, copies[i])
					if err != nil {
						failures[i] = err
						continue
					}
					if known {
						changed.Add(uint64(blocks))
						measured.Add(1)
					} else {
						unmeasured.Add(1)
					}
				}
				equal[i], failures[i] = r.compare(ctx, &worker, copies[i])
			}
		}()
	}
	wait.Wait()
	if measuring {
		h.mu.Lock()
		h.stats.ChangedBlocks += changed.Load()
		h.stats.MeasuredPages += measured.Load()
		h.stats.UnmeasuredPages += unmeasured.Load()
		h.mu.Unlock()
	}
	if err := r.reshare(ctx, c, copies, equal, dropped); err != nil {
		failures = append(failures, err)
	}
	unchanged := 0
	for _, was := range dropped {
		if was {
			unchanged++
		}
	}
	if unchanged > 0 {
		c.forgetCopies(dropped)
		h.mu.Lock()
		h.stats.UnchangedPages += uint64(unchanged)
		h.signal()
		h.mu.Unlock()
	}
	return unchanged, errors.Join(failures...)
}

// settler is one worker. Where a file cannot compare two of its own slots, or
// the two pages are in different files, the comparison needs a page of each,
// which is made once and reused for every page that worker settles.
type settler struct{ first, second []byte }

// equal reports whether two arena slots hold the same bytes. Caller holds both
// pages' locks, so neither slot can be released or refilled while it runs.
func (s *settler) equal(ctx context.Context, h *Host, first, second fileSlot) (bool, error) {
	if comparing, ok := first.file.ArenaFile.(EqualFile); ok && first.file == second.file {
		return comparing.Equal(ctx, first.slot, second.slot)
	}
	if s.first == nil {
		s.first, s.second = make([]byte, h.pageSize), make([]byte, h.pageSize)
	}
	if err := first.file.Read(ctx, first.slot, s.first); err != nil {
		return false, err
	}
	if err := second.file.Read(ctx, second.slot, s.second); err != nil {
		return false, err
	}
	return bytes.Equal(s.first, s.second), nil
}

// since is when the oldest write this checkpoint still holds was made, zero
// where it holds none. A settle can empty the set, so it is read under the
// checkpoint's own lock rather than off the field.
func (c *MemoryRegionCheckpoint) since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirtySince
}

// compare reports the origin of a checkpoint's copy when the two hold the
// same bytes, and nil where the copy stays in the checkpoint. It changes
// nothing; reshare applies what it decided.
func (r *MemoryRegion) compare(ctx context.Context, s *settler, held *binding) (*zirconvm.VmPage, error) {
	h := r.host
	r.bindingsMu.Lock()
	origin, spill := held.origin, held.spill
	r.bindingsMu.Unlock()
	if origin == nil || spill.none() {
		return nil, nil
	}
	if h.wholeRange(r, held.index) {
		// A range the half-private rule filled stays whole.
		return nil, nil
	}
	// The origin first, as every comparison takes them: clean before private.
	if err := r.host.lockPage(ctx, origin); err != nil {
		return nil, err
	}
	defer r.host.unlockPage(origin)
	if !r.host.published(origin) {
		// Evicted, or no longer its identity's page: there is nothing to
		// compare with, and the page is published as it would have been.
		return nil, nil
	}
	page, err := r.host.lockedPage(ctx, held)
	if err != nil || page == nil {
		// Spilled since the seal, which is store I/O the settle does not do.
		return nil, err
	}
	defer r.host.unlockPage(page)
	same, err := s.equal(ctx, h, frameOf(origin).fileSlot, frameOf(page).fileSlot)
	if err != nil || !same {
		return nil, err
	}
	return origin, nil
}

// reshare applies what the comparison decided, in bounded batches with the
// region held exclusively: every page of a batch is revoked by one command
// per run, and only then does each leave the checkpoint. It records in
// dropped which did.
func (r *MemoryRegion) reshare(ctx context.Context, c *MemoryRegionCheckpoint, copies []*binding,
	equal []*zirconvm.VmPage, dropped []bool) error {
	pending := make([]int, 0, len(copies))
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
			return r.reshareBatch(ctx, copies, equal, dropped, batch)
		}()
		if err != nil {
			return err
		}
		pending = pending[count:]
	}
	return nil
}

// reshareBatch is one batch of reshare, with the region held exclusively.
// It holds every page it will touch for the whole batch, so the revocation
// that covers them all is issued while none of them can change.
func (r *MemoryRegion) reshareBatch(ctx context.Context, copies []*binding, equal []*zirconvm.VmPage,
	dropped []bool, batch []int) error {
	locked := make(map[*zirconvm.VmPage]bool, 2*len(batch))
	defer func() {
		for page := range locked {
			r.host.unlockPage(page)
		}
	}()
	type ready struct {
		index  int
		copied *zirconvm.VmPage
		origin *zirconvm.VmPage
		guest  *binding
	}
	var applying []ready
	var guests []*binding
	for _, i := range batch {
		origin := equal[i]
		if !locked[origin] {
			if err := r.host.lockPage(ctx, origin); err != nil {
				return err
			}
			locked[origin] = true
		}
		if !r.host.published(origin) {
			continue
		}
		page, err := r.host.lockedPage(ctx, copies[i])
		if err != nil {
			return err
		}
		if page == nil {
			continue
		}
		if locked[page] {
			r.host.unlockPage(page)
		} else {
			locked[page] = true
		}
		entry := ready{index: i, copied: page, origin: origin}
		if b := r.lookupBinding(copies[i].index); b != nil && r.checkpointCopy(b) == copies[i] {
			entry.guest = b
			if r.isMapped(b) {
				guests = append(guests, b)
			}
		}
		applying = append(applying, entry)
	}
	// The guest's mapping of the copy it is losing is taken away rather than
	// swapped underneath it: see dropCopy.
	if err := r.revokeBindings(ctx, guests); err != nil {
		return err
	}
	for _, entry := range applying {
		if err := r.dropCopy(ctx, copies[entry.index], entry.copied, entry.origin, entry.guest); err != nil {
			return err
		}
		dropped[entry.index] = true
	}
	return nil
}

// dropCopy takes one unchanged page out of the checkpoint. A guest that still
// shares the checkpoint's copy goes back on the origin and is clean. Its
// mapping of the copy has already been taken away by the batch's revocation,
// because the page a guest maps is taken away and never swapped underneath it:
// a revocation installs no page table and wakes nothing, and the guest's next
// access reaches the origin through the fault path, under the window that
// orders every mapping of that page. Revoking rather than installing the
// origin lowered the rate of a fan-out panic whose cause was elsewhere: see "A
// post-copy child's own published pages" in docs/migration.md.
//
// The copy's page leaves the layer, AwaitingClean in its page list or held
// beside it, and goes back, and so does its reservation. Caller holds the
// region exclusively and both pages.
func (r *MemoryRegion) dropCopy(ctx context.Context, held *binding, page, origin *zirconvm.VmPage, guest *binding) error {
	h := r.host
	ps := h.pageSize
	if guest != nil {
		// The one place the pager hands a guest back an older page on
		// purpose: the audit checks the bytes here rather than trusting the
		// comparison that chose this page.
		if found := h.probe.reshared(ctx, h, frameOf(page), frameOf(origin)); found != "" {
			panic(found)
		}
	}
	h.mu.Lock()
	if guest != nil {
		r.host.unaliasLocked(guest)
		r.host.aliasLocked(guest, origin)
	}
	r.host.unaliasLocked(held)
	others := frameOf(page).aliases.len() > 0
	h.mu.Unlock()
	if guest != nil {
		r.bindingsMu.Lock()
		r.retireFromCheckpointLocked(guest)
		r.bindingsMu.Unlock()
		r.host.node.PageQueues().MarkAccessed(origin)
	}
	if others {
		return fmt.Errorf("vmmemory: a settled page is still mapped at page %d", held.index)
	}
	if r.layer.RemovePage(held.index*ps, page) {
		r.host.releaseFrame(page)
	}
	r.bindingsMu.Lock()
	spill := held.spill
	held.spill, held.dirty, held.origin = noReservation, false, nil
	r.bindingsMu.Unlock()
	h.releaseSpill(spill)
	return nil
}

// forgetCopies rebuilds the copies this checkpoint lists without those a
// settle dropped, in the order it had them, so the result is the workers'
// outcome and not their order. A checkpoint left holding nothing holds no
// unpublished write either, so the window it took off the memory region at the
// seal ends here.
func (c *MemoryRegionCheckpoint) forgetCopies(dropped []bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copies := make([]*binding, 0, len(c.copies))
	for i, held := range c.copies {
		if i < len(dropped) && dropped[i] {
			continue
		}
		copies = append(copies, held)
	}
	c.copies = copies
	if len(c.copies) == 0 {
		c.dirtySince = time.Time{}
	}
}
