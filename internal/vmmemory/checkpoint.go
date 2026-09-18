package vmmemory

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
)

// checkpointBatchPages bounds how many pages one seal or retire transition
// holds the region for at a time, so a large dirty set costs a bounded number
// of long-held page locks and a bounded hold of the region.
var checkpointBatchPages = 1024

var (
	// ErrSealed reports a region that already holds a checkpoint. One seal is
	// outstanding at a time: the checkpoint is retired by the publication that
	// uploads it, or abandoned by Unseal, and only then may another be taken.
	ErrSealed = errors.New("managed-memory region is already sealed")
	// ErrNotSealed reports a region asked for its checkpoint while it holds none.
	ErrNotSealed = errors.New("managed-memory region has no sealed checkpoint")
)

// RegionCheckpoint is one checkpoint of a volume's dirty pages, taken by Seal
// and published by the checkpoint that uploads it. Its pages are detached
// bindings: they are not reachable from the region's bindings, they alias the
// resident pages the guest had at the seal, and they own the dirty reservations
// those pages were admitted under. The set is fixed when Seal returns, which is
// what lets the guest keep running against the same pages: a store copies away
// from the checkpoint instead of into it.
//
// It is what a checkpoint reads its pages from, so it satisfies the publication
// interface volume.Checkpoint takes per volume. Nothing copies those bytes on
// the way: the upload reads the guest's own pages, and the seal is held until
// it lands.
//
// A checkpoint is immutable once Seal returns and safe for concurrent use.
type RegionCheckpoint struct {
	region *Region
	pages  []*binding
	byPage map[uint64]*binding
	// dirtySince is the region's loss window at the seal: when the oldest of
	// these pages was written. The seal takes it off the region, so that what
	// the region reports from here is its own new writes; an abandoned
	// checkpoint hands it back, and a published one drops it, because the store
	// holds those bytes then.
	dirtySince time.Time
	// held marks a checkpoint a fork point has taken. Such a seal lasts until
	// the children of that fork point have the pages they inherited, which is no
	// bound a waiting store may wait under, so it counts as no relief at all.
	held atomic.Bool
	done chan struct{}
	err  error // read only after done is closed
}

func (c *RegionCheckpoint) finish(err error) {
	c.err = err
	close(c.done)
}

func (r *Region) currentCheckpoint() *RegionCheckpoint {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	return r.checkpoint
}
func (r *Region) setCheckpoint(c *RegionCheckpoint) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	r.checkpoint = c
	r.sealing = false
}

// setSealing marks a seal in progress, or one that failed before it recorded
// its checkpoint; recording the checkpoint clears the mark itself.
func (r *Region) setSealing(sealing bool) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	r.sealing = sealing
}

// Checkpoint reports the sealed checkpoint this region holds, nil when it holds
// none. It is what a checkpoint publishes this region's pages from.
func (r *Region) Checkpoint() *RegionCheckpoint { return r.currentCheckpoint() }

// sealState reports whether a seal of this region is in progress, and the
// checkpoint it has sealed and not yet ended, which together decide whether
// another checkpoint can be asked for and whether this one will give dirty
// reservations back. The two are read under one lock: a seal takes the dirty
// set into the checkpoint before it records the checkpoint on the region, and a
// waiting store that asked between the two would otherwise find neither a
// draining checkpoint nor a dirty page and be stalled for it.
func (r *Region) sealState() (sealing bool, draining *RegionCheckpoint) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	if c := r.checkpoint; c != nil {
		select {
		case <-c.done:
		default:
			draining = c
		}
	}
	return r.sealing, draining
}

// relieves reports whether ending this checkpoint will relieve the dirty
// budget under a bound a waiting store may wait for. A publication's does: it
// gives its reservations back, and where it holds none it gives the region back
// the right to take the checkpoint that will. A fork point's hold does
// neither for as long as the children it named are still reading it, and no
// store can be left waiting on that.
func (c *RegionCheckpoint) relieves() bool { return !c.held.Load() }

// Hold marks this seal as a fork point's, which a fork point does when it
// takes the pause. It is what says the seal lasts as long as the children of
// that fork point rather than as long as an upload, so no store waiting for the
// dirty budget is left waiting on it. Sharing the pages is separate: a child
// on another host never maps them, so only a child taken in here names them.
func (c *RegionCheckpoint) Hold() { c.held.Store(true) }

// Seal takes the capture's checkpoint of this volume and returns. It revokes
// the guest's write access to every dirty page and records those pages as the
// checkpoint, so the pause it belongs to costs page-table work rather than any
// bytes. The guest may store into a sealed page as soon as this returns: it
// takes the write-protect fault and gets a private copy, while the checkpoint
// keeps the page.
//
// The region stays sealed, publishing nothing newer, until the checkpoint is
// retired. Retiring it belongs to the publication: it reads the sealed
// pages, and when it lands they become clean under the checkpoint that
// holds them. Unseal abandons a checkpoint no publication will take, which
// hands every page back to the guest as ordinary dirty state. A seal that fails
// partway captures nothing: it hands the pages it took back, so sealing again
// takes a checkpoint of everything that is dirty then.
func (r *Region) Seal(ctx context.Context) error {
	// A capture's pause is this call, so it is timed: the range write-protects
	// are timed separately inside it, and the difference is the pager's own
	// bookkeeping of taking the pages into the checkpoint.
	defer func(start time.Time) { r.host.sealLatency.Observe(r.host.clock.Since(start)) }(r.host.clock.Now())
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	if r.currentCheckpoint() != nil {
		return ErrSealed
	}
	r.setSealing(true)
	pages, err := r.sealPages(ctx)
	if err != nil {
		r.setSealing(false)
		return err
	}
	if sealSeam != nil {
		sealSeam()
	}
	// The window moves to the checkpoint with the pages it is measured over.
	// It is taken after the seal succeeded: a seal that failed partway hands
	// every page back, so it must hand nothing else back either.
	checkpoint := &RegionCheckpoint{region: r, pages: pages, dirtySince: r.takeDirtySince(),
		byPage: make(map[uint64]*binding, len(pages)), done: make(chan struct{})}
	for _, held := range pages {
		checkpoint.byPage[held.index] = held
	}
	r.setCheckpoint(checkpoint)
	// A store that woke while the seal was in progress waited on it; the
	// checkpoint it was promised is now the region's to answer for.
	r.host.mu.Lock()
	r.host.signal()
	r.host.mu.Unlock()
	return nil
}

// sealSeam runs in a seal between taking the dirty set into the checkpoint and
// recording the checkpoint on the region. It is nil in production; a test
// installs one to wake a store waiting for the dirty budget in that pause.
var sealSeam func()

// sealPages write-protects every dirty page and detaches the checkpoint's copy
// of it. Caller holds the exclusive region lock. Page locks are taken in
// bounded batches; each batch is protected as whole runs of consecutive pages,
// so the cost is one range command per run whatever memory those pages hold,
// and no page's bytes and no page's mapping move.
//
// A batch that fails leaves the pages it write-protected and the pages it did
// not in one incoherent half-seal, which no capture may be built on. It is
// undone: every page taken goes back to the guest as ordinary dirty state, and
// a later Seal takes a whole checkpoint of whatever is dirty then.
func (r *Region) sealPages(ctx context.Context) ([]*binding, error) {
	h := r.host
	var pages []*binding
	bindings := r.dirtySnapshot()
	for len(bindings) > 0 {
		if err := context.Cause(ctx); err != nil {
			// The capture this pause belongs to was abandoned. What it has taken
			// so far is half a seal, which no capture may be built on, so it is
			// handed back like any other failure partway.
			return nil, errors.Join(err, r.abandonPages(context.WithoutCancel(ctx), pages))
		}
		count := min(len(bindings), checkpointBatchPages)
		var locked []*resident
		taken := len(pages)
		err := func() error {
			defer func() { h.unlockAll(locked) }()
			var runs []PageRun
			var reclaiming []*binding
			for _, b := range bindings[:count] {
				pg, busy := h.tryCurrent(b)
				if pg != nil {
					locked = append(locked, pg)
				}
				held := &binding{region: r, index: b.index, dirty: true, spillSlot: b.spillSlot}
				switch {
				case pg != nil:
					h.bind(held, pg)
				case busy:
					// A reclaim is the only thing that can hold a resident page
					// of the region being sealed, and it is writing that page's
					// bytes to this page's reservation, which the checkpoint's
					// copy is taking over. Joining the copy to that page without
					// waiting for the reclaim keeps the pause off its I/O.
					h.joinReclaiming(held, b)
				}
				if busy {
					// The reclaim takes the page's mapping away, which is
					// stronger than write-protecting it and is what the guest
					// has to fault through either way. Revoking here too costs
					// one command and leaves nothing writable in between.
					reclaiming = append(reclaiming, b)
				} else if r.isMapped(b) {
					// Runs need only be consecutive pages: a range protection
					// does not care which memory they hold.
					if n := len(runs); n > 0 && runs[n-1].Page+uint64(runs[n-1].Count) == b.index {
						runs[n-1].Count++
					} else {
						runs = append(runs, PageRun{Page: b.index, Count: 1})
					}
				}
				r.holdInCheckpoint(b, held)
				pages = append(pages, held)
			}
			for _, b := range reclaiming {
				if err := h.revoke(ctx, b); err != nil {
					return err
				}
			}
			return r.protect(ctx, runs)
		}()
		// Pages count as sealed when the checkpoint takes them, so a capture that
		// never completes still reports the work its pause paid for.
		h.mu.Lock()
		h.stats.CheckpointPages += uint64(len(pages) - taken)
		h.mu.Unlock()
		if err != nil {
			// The undo runs whatever cancelled this seal; only a terminal
			// region or host can fail it, and nothing proceeds after those.
			return nil, errors.Join(err, r.abandonPages(context.WithoutCancel(ctx), pages))
		}
		bindings = bindings[count:]
	}
	return pages, nil
}

// protect takes write access away from every run of pages the seal took,
// leaving their mappings, memory and page tables exactly as they are: the guest
// keeps reading the pages the checkpoint holds and traps on its next store to
// one. Nothing is copied, nothing is mapped, and no page is left without a
// mapping for the guest to fault on. One range command covers a run of
// consecutive pages whatever memory they hold, so the pause pays for runs, not
// pages.
func (r *Region) protect(ctx context.Context, runs []PageRun) error {
	h := r.host
	commands, pages := 0, 0
	defer func() {
		// A run protected before a later one failed is still work the pause
		// paid for, and the undo that follows has to account for it.
		h.mu.Lock()
		h.stats.Protections += uint64(commands)
		h.stats.ProtectedPages += uint64(pages)
		h.mu.Unlock()
	}()
	for _, run := range runs {
		if err := r.protectPages(ctx, run.Page, run.Count); err != nil {
			// A seal that ran out of time is a checkpoint that did not happen,
			// not a region that is over: the undo above gives every page it
			// took back to the guest and the next checkpoint takes the whole
			// dirty set. Only a command that actually failed leaves the region
			// in a state nothing can reason about, and only that is terminal.
			if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
				return err
			}
			return r.fail(err)
		}
		commands++
		pages += run.Count
	}
	return nil
}

// DirtyPages reports the pages this checkpoint publishes, in ascending order. A
// pager page is a store page, so these are the store pages a checkpoint
// republishes.
func (c *RegionCheckpoint) DirtyPages() []uint64 {
	pages := make([]uint64, 0, len(c.pages))
	for _, held := range c.pages {
		pages = append(pages, held.index)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	return pages
}

// UnpublishedAge is how long the oldest write this checkpoint holds has gone
// unpublished, zero where it holds none. It is the region's loss window as the
// seal took it, still running: a fork point's hold can outlast several
// intervals, and the child that inherits these pages inherits their age with
// them rather than starting a window of its own.
func (c *RegionCheckpoint) UnpublishedAge() time.Duration {
	if c.dirtySince.IsZero() {
		return 0
	}
	return max(c.region.host.clock.Since(c.dirtySince), 0)
}

// Share names every page this checkpoint holds by the identity the checkpoint
// publishing it gives that page, so a region that inherits the identity maps
// the resident page instead of reading the page. A fork point is what calls it,
// when a child of that fork point is taken in on this host: the fork point takes
// a reference of its own and publishes nothing under it, so the name it gives
// the parent's unpublished pages is shared by every child of that fork point and
// claimed by nothing else, ever.
//
// The pages stay the guest's private dirty state under it. Nothing is copied
// and nothing becomes durable: the name lasts exactly as long as the seal,
// because until the seal ends the bytes cannot change, and it is taken back
// when the seal ends. A page already evicted is simply not named — whoever
// inherits it reads the page through its own backing, which reaches these same
// pages through the seal.
func (c *RegionCheckpoint) Share(ctx context.Context, ref control.Ref, volume string) error {
	h := c.region.host
	c.held.Store(true)
	for _, held := range c.pages {
		key := pageKey{id: control.Identity{Ref: ref, Volume: volume, Page: held.index}}
		if err := h.locked(ctx, held, func(pg *resident) error {
			h.share(pg, key)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReadDirty fills dst, exactly one pager page, with the bytes the seal froze. A
// store the guest made since then copied that page away from the checkpoint, so
// it is not in them. It takes an I/O permit and the page's lock, never the
// region's: the guest goes on faulting and storing while a checkpoint reads its
// checkpoint.
func (c *RegionCheckpoint) ReadDirty(ctx context.Context, page uint64, dst []byte) error {
	h := c.region.host
	if len(dst) != PageSize {
		return ErrRange
	}
	select {
	case <-c.done:
		// Retired, abandoned or discarded: these pages are the guest's own
		// state again or the volume's, and the memory behind them holds whatever
		// has happened since, which is not what this checkpoint holds. A region
		// that discarded it reports why it ended.
		if c.err != nil {
			return c.err
		}
		return ErrNotSealed
	default:
	}
	held := c.byPage[page]
	if held == nil {
		return ErrRange
	}
	release, err := h.beginCheckpointIO(ctx)
	if err != nil {
		return err
	}
	defer release()
	pg, err := h.current(ctx, held)
	if err != nil {
		return err
	}
	if pg != nil {
		defer h.unlock(pg)
	}
	if err := h.read(ctx, held, pg, dst); err != nil {
		return err
	}
	if held.ahead && allZero(dst) {
		h.mu.Lock()
		h.stats.WriteAheadZeroPages++
		h.mu.Unlock()
	}
	return nil
}

// Retire ends the seal this checkpoint holds. published reports that the
// checkpoint which read these pages was selected, so their bytes are the
// volume's now and every page the guest has not stored into since the seal
// becomes ordinary clean state under the checkpoint's identity. Otherwise the
// pages go back to the guest as dirty state and the next checkpoint takes them
// again.
//
// Either way the region publishes again once this returns. It is idempotent: a
// checkpoint already retired, or one a detached region discarded, reports
// success.
func (c *RegionCheckpoint) Retire(ctx context.Context, published bool) error {
	return c.region.endSeal(ctx, c, published)
}

// Unseal abandons a checkpoint no publication is going to take and ends the
// seal. Its pages become ordinary dirty state again, exactly as they were
// before the seal, and the next checkpoint takes them. A capture whose
// publication landed does not come here: that checkpoint is retired by
// RegionCheckpoint.Retire, which makes its pages clean under the checkpoint
// that holds them.
//
// Guest stores never waited for the seal: with copy-on-write, what it holds back
// is the region's next checkpoint, not the guest.
func (r *Region) Unseal(ctx context.Context) error {
	return r.endSeal(ctx, nil, false)
}

// endSeal retires or abandons the region's checkpoint and ends the seal. A
// failure leaves the checkpoint in place, so the caller can repeat the call
// with a live context; nothing else may seal this region until it succeeds.
//
// The set is as large as a capture's, so it is walked in the same bounded
// batches a seal takes, and the region is given back between them: a fault
// waits for one batch, not for the walk. Each batch's volume metadata is looked
// up first, with neither the region nor any page held. A page already retired
// is skipped, so a repeated call finishes what a failed one left.
func (r *Region) endSeal(ctx context.Context, checkpoint *RegionCheckpoint, published bool) error {
	// The walk gives the region up between batches, so what keeps the
	// checkpoint this region's for the whole of it is the lifetime lock and one
	// end at a time.
	if err := r.live.RLock(ctx); err != nil {
		return err
	}
	defer r.live.RUnlock()
	if err := r.endMu.Lock(ctx); err != nil {
		return err
	}
	defer r.endMu.Unlock()
	current := r.currentCheckpoint()
	if current == nil || (checkpoint != nil && current != checkpoint) {
		// Detach discarded it, or it has already been retired.
		return nil
	}
	for pages := current.pages; len(pages) > 0; {
		batch := pages[:min(len(pages), checkpointBatchPages)]
		var identities map[uint64]storedPage
		if published {
			var err error
			if identities, err = r.storedIdentities(ctx, batch); err != nil {
				return err
			}
		}
		if err := r.mu.Lock(ctx); err != nil {
			return err
		}
		err := func() error {
			defer r.mu.Unlock()
			if published {
				return r.finalizeCheckpoint(ctx, batch, identities)
			}
			return r.abandonPages(ctx, batch)
		}()
		if err != nil {
			return err
		}
		pages = pages[len(batch):]
	}
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if !published {
		// These pages are the guest's dirty state again, so the window they were
		// written in is the region's again. A window that restarted here would
		// bound nothing: the host that cannot publish is exactly the host whose
		// publications keep failing.
		r.restoreDirtySince(current.dirtySince)
	}
	r.setCheckpoint(nil)
	current.finish(nil)
	// A store waiting for the dirty budget waits for whichever checkpoint ends
	// next, so every end wakes it — including one that held no reservation to
	// give back, whose retire released nothing and signalled nothing.
	r.host.mu.Lock()
	r.host.signal()
	r.host.mu.Unlock()
	return nil
}

// finalizeCheckpoint retires every page of a published checkpoint. A page the
// guest has not stored into since the seal becomes ordinary clean state, its
// resident page joining the sharing index under the identity the volume now
// reports and staying mapped to the guest. A page the guest copied away from
// keeps only the checkpoint's own copy, which is released here. Either way the
// dirty reservation goes back.
//
// The page and the checkpoint's copy of it share one resident page, so retiring
// both and publishing the result happen under one hold of that page's lock: a
// private page must never be reachable from a binding that owns neither a
// spill reservation nor a checkpoint, which is what a concurrent eviction
// between those steps would find, and it would punch the page with nowhere to
// put its bytes.
func (r *Region) finalizeCheckpoint(ctx context.Context, held []*binding, identities map[uint64]storedPage) error {
	h := r.host
	for _, checkpoint := range held {
		if checkpoint.spillSlot < 0 {
			continue
		}
		b := r.lookupBinding(checkpoint.index)
		shared := b != nil && r.heldBy(checkpoint.index, checkpoint)
		lineage := identities[checkpoint.index]
		id, stored := lineage.id, lineage.stored
		pg, err := h.current(ctx, checkpoint)
		if err != nil {
			return err
		}
		if pg != nil {
			// The name the seal gave the page goes with the seal; the identity
			// the volume reports now takes its place below.
			h.unshare(pg)
		}
		if shared {
			r.retireFromCheckpoint(b)
		}
		if pg != nil {
			err = h.unlink(ctx, checkpoint, pg)
			if err == nil && shared {
				err = r.publishLocked(ctx, b, pg, id, stored)
			}
			h.unlock(pg)
			if err != nil {
				return err
			}
		}
		h.releaseSpill(checkpoint.spillSlot)
		checkpoint.spillSlot, checkpoint.dirty = -1, false
	}
	return nil
}

// abandonPages gives up on a checkpoint's pages. A page the guest still shares
// with the checkpoint takes its copy's reservation back and is dirty again, so
// nothing the guest wrote before the seal is lost; a page the guest copied away
// from needs only its own newer state, so the checkpoint's copy is released.
// Caller holds the exclusive region lock.
//
// The reservation moves under the resident page's lock, together with the
// unlink that takes the checkpoint's alias away: an eviction that saw the two
// apart would find a private page with neither a reservation of its own nor a
// checkpoint still holding one, skip spilling it, and punch the page.
func (r *Region) abandonPages(ctx context.Context, pages []*binding) error {
	h := r.host
	var restored []*binding
	for _, held := range pages {
		if held.spillSlot < 0 {
			continue // already retired
		}
		b := r.lookupBinding(held.index)
		shared := b != nil && r.heldBy(held.index, held)
		slot := held.spillSlot
		pg, err := h.current(ctx, held)
		if err != nil {
			return err
		}
		if pg != nil {
			h.unshare(pg)
		}
		if shared {
			if pg != nil {
				// The guest takes this page back as dirty state it may store
				// into in place, so nothing else may still be reading it.
				if err := h.dropSharers(ctx, pg, b, held); err != nil {
					h.unlock(pg)
					return err
				}
			}
			r.restoreFromCheckpoint(b, held)
			if r.isMapped(b) {
				restored = append(restored, b)
			}
		} else {
			held.spillSlot, held.dirty = -1, false
		}
		// The live page keeps its memory when it still shares one; unlink
		// releases it only where the checkpoint's copy is its last alias.
		if pg != nil {
			err = h.unlink(ctx, held, pg)
			h.unlock(pg)
			if err != nil {
				return err
			}
		}
		if !shared {
			h.releaseSpill(slot)
		}
	}
	// The seal left these pages mapped in the read-only form the guest traps
	// on. Take those mappings away, so the next store faults and maps the page
	// writable again instead of resolving a store against a read-only mapping.
	return r.revokeLocked(ctx, restored)
}

// discardCheckpoint drops a checkpoint entirely, which is what detaching a
// region does: its memory users are gone and unpublished stores are discarded
// with them.
func (r *Region) discardCheckpoint(ctx context.Context, checkpoint *RegionCheckpoint) error {
	h := r.host
	for _, held := range checkpoint.pages {
		if err := h.locked(ctx, held, func(pg *resident) error {
			// The seal ends with the region, so its name for the page does
			// too. Whatever still shares the page keeps it: nothing can store
			// into it any more, and its bytes stay what the seal froze.
			h.unshare(pg)
			return h.unlink(ctx, held, pg)
		}); err != nil {
			return err
		}
		if held.spillSlot >= 0 {
			h.releaseSpill(held.spillSlot)
			held.spillSlot, held.dirty = -1, false
		}
	}
	checkpoint.finish(ErrClosed)
	return nil
}
