package vmmemory

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
)

// checkpointBatchPages bounds how many pages one seal or retire transition
// holds the memory region for at a time, so a large dirty set costs a bounded number
// of long-held page locks and a bounded hold of the memory region.
var checkpointBatchPages = 1024

var (
	// ErrSealed reports a memory region that already holds a checkpoint. One seal is
	// outstanding at a time: the checkpoint is retired by the publication that
	// uploads it, or abandoned by Unseal, and only then may another be taken.
	ErrSealed = errors.New("managed-memory-region is already sealed")
	// ErrNotSealed reports a memory region asked for its checkpoint while it holds none.
	ErrNotSealed = errors.New("managed-memory-region has no sealed checkpoint")
)

// MemoryRegionCheckpoint is one checkpoint of a volume's dirty pages, taken by Seal
// and published by the checkpoint that uploads it. Its pages are detached
// bindings: they are not reachable from the memory region's bindings, they alias the
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
// The set is fixed once Settle has run and safe for concurrent use; Settle
// itself is the one thing that changes it, and the publication calls it before
// it reads anything.
type MemoryRegionCheckpoint struct {
	memoryRegion *MemoryRegion
	// taken is closed once the walk behind the pause has moved every page of
	// the sealed set into this checkpoint. The set is not readable before that
	// and every reader of it waits here; the walk runs with the guest already
	// running, holding the memory region the seal took, so what waits for it is a
	// fault of that memory region rather than its VM.
	taken chan struct{}
	// mu guards the set and the window below, which a settle takes pages out
	// of. Everything else here is fixed when the walk closes taken. The set is
	// in ascending page order, which is how a page of it is found.
	mu    sync.Mutex
	pages []*binding
	// dirtySince is the memory region's loss window at the seal: when the oldest of
	// these pages was written. The seal takes it off the memory region, so that what
	// the memory region reports from here is its own new writes; an abandoned
	// checkpoint hands it back, and a published one drops it, because the store
	// holds those bytes then. A settle that leaves the set empty ends it there:
	// a checkpoint holding nothing holds no unpublished write.
	dirtySince time.Time
	// held marks a checkpoint a fork point has taken. Such a seal lasts until
	// the children of that fork point have the pages they inherited, which is no
	// bound a waiting store may wait under, so it counts as no relief at all.
	held atomic.Bool
	done chan struct{}
	err  error // read only after done is closed
}

func (c *MemoryRegionCheckpoint) finish(err error) {
	c.err = err
	close(c.done)
}

func (r *MemoryRegion) currentCheckpoint() *MemoryRegionCheckpoint {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	return r.checkpoint
}
func (r *MemoryRegion) setCheckpoint(c *MemoryRegionCheckpoint) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	r.checkpoint = c
	r.sealing = false
}

// setSealing marks a seal in progress, or one that failed before it recorded
// its checkpoint; recording the checkpoint clears the mark itself.
func (r *MemoryRegion) setSealing(sealing bool) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	r.sealing = sealing
}

// Checkpoint reports the sealed checkpoint this memory region holds, nil when it holds
// none. It is what a checkpoint publishes this memory region's pages from.
func (r *MemoryRegion) Checkpoint() *MemoryRegionCheckpoint { return r.currentCheckpoint() }

// sealState reports whether a seal of this memory region is in progress, and the
// checkpoint it has sealed and not yet ended, which together decide whether
// another checkpoint can be asked for and whether this one will give dirty
// reservations back. The two are read under one lock: a seal takes the dirty
// set into the checkpoint before it records the checkpoint on the memory region, and a
// waiting store that asked between the two would otherwise find neither a
// draining checkpoint nor a dirty page and be stalled for it.
func (r *MemoryRegion) sealState() (sealing bool, draining *MemoryRegionCheckpoint) {
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
// gives its reservations back, and where it holds none it gives the memory region back
// the right to take the checkpoint that will. A fork point's hold does
// neither for as long as the children it named are still reading it, and no
// store can be left waiting on that.
func (c *MemoryRegionCheckpoint) relieves() bool { return !c.held.Load() }

// Hold marks this seal as a fork point's, which a fork point does when it
// takes the pause. It is what says the seal lasts as long as the children of
// that fork point rather than as long as an upload, so no store waiting for the
// dirty budget is left waiting on it. Sharing the pages is separate: a child
// on another host never maps them, so only a child taken in here names them.
func (c *MemoryRegionCheckpoint) Hold() { c.held.Store(true) }

// Seal takes the capture's checkpoint of this volume and returns. It revokes
// the guest's write access to every dirty page and records those pages as the
// checkpoint, so the pause it belongs to costs page-table work rather than any
// bytes. The guest may store into a sealed page as soon as this returns: it
// takes the write-protect fault and gets a private copy, while the checkpoint
// keeps the page.
//
// **The pause is the write-protect commands.** What a seal has to do per page —
// move the binding into the checkpoint, hand it the page's reservation, hand it
// where that page was copied from — is not what makes the guest's writes
// coherent; the write protection is, because from the moment a run is protected
// no store to it can land without trapping and waiting for the memory region. So the
// pause reads the runs of the dirty set, which the memory region keeps as runs for
// exactly this, protects them, and takes the whole set in one step; the walk
// over its pages runs afterwards, with the guest running, holding the memory region the
// seal took. A fault of this memory region waits for that walk, and nothing else does.
// At 4 KiB the difference is the whole of it: a GCE capture of 2,204,672 sealed
// RAM pages paused 2.14 s, of which 0.18 s was its 2,264 protect commands and
// 1.97 s was the walk.
//
// The memory region stays sealed, publishing nothing newer, until the checkpoint is
// retired. Retiring it belongs to the publication: it reads the sealed
// pages, and when it lands they become clean under the checkpoint that
// holds them. Unseal abandons a checkpoint no publication will take, which
// hands every page back to the guest as ordinary dirty state. A seal whose
// protection fails partway captures nothing: the runs it protected have their
// mappings taken away, so the guest faults and maps them writable again, and
// sealing again takes a checkpoint of everything that is dirty then.
func (r *MemoryRegion) Seal(ctx context.Context) error {
	// A capture's pause is this call, so it is timed: the range write-protects
	// are timed separately inside it, and the difference is what the pager
	// spends on the pause beside its commands.
	defer func(start time.Time) { r.host.sealLatency.Observe(r.host.clock.Since(start)) }(r.host.clock.Now())
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	held := false
	defer func() {
		if !held {
			r.mu.Unlock()
		}
	}()
	if err := r.ready(); err != nil {
		return err
	}
	if r.currentCheckpoint() != nil {
		return ErrSealed
	}
	r.setSealing(true)
	protected, err := r.protectDirtyRuns(ctx)
	if err != nil {
		r.setSealing(false)
		return errors.Join(err, r.unprotect(context.WithoutCancel(ctx), protected))
	}
	if sealSeam != nil {
		sealSeam()
	}
	// The window moves to the checkpoint with the pages it is measured over.
	// It is taken after the protection succeeded: a seal that protected nothing
	// is a checkpoint that did not happen, so it must take nothing else either.
	checkpoint := &MemoryRegionCheckpoint{memoryRegion: r, dirtySince: r.takeDirtySince(),
		done: make(chan struct{}), taken: make(chan struct{})}
	pending := r.takeDirtySet()
	r.setCheckpoint(checkpoint)
	// The memory region stays locked, and the walk gives it back. Nothing of this VM
	// but a fault of this memory region waits for it.
	held = true
	go checkpoint.take(context.WithoutCancel(ctx), pending)
	// A store that woke while the seal was in progress waited on it; the
	// checkpoint it was promised is now the memory region's to answer for.
	r.host.mu.Lock()
	r.host.signal()
	r.host.mu.Unlock()
	return nil
}

// sealSeam runs in a seal between write-protecting the dirty set and recording
// the checkpoint on the memory region. It is nil in production; a test installs one to
// wake a store waiting for the dirty budget in that pause. sealWalkSeam runs in
// the walk behind that pause, before it takes its first page, which is where a
// test holds the walk to see what the pause alone cost.
var sealSeam, sealWalkSeam func()

// take is the walk behind the pause: it detaches the checkpoint's copy of every
// page the seal froze. It runs with the guest already running, holding the
// memory region the seal locked, and gives that memory region back when it is done — so a
// fault of this memory region waits for one walk rather than its VM waiting for one
// pause. Page locks are taken in bounded batches, and no page's bytes and no
// page's mapping move.
//
// The pages are already write-protected, so nothing the guest does can change
// what this walk is recording: a store into one of them traps and waits for the
// memory region. The only thing that can fail here is the revocation a page a reclaim
// is holding needs, and that is a terminal memory region, which every reader of this
// checkpoint reports rather than publishing a set that is missing pages.
func (c *MemoryRegionCheckpoint) take(ctx context.Context, pending map[uint64]*binding) {
	r := c.memoryRegion
	defer r.mu.Unlock()
	defer close(c.taken)
	if sealWalkSeam != nil {
		sealWalkSeam()
	}
	defer func(start time.Time) { r.host.sealWalkLatency.Observe(r.host.clock.Since(start)) }(r.host.clock.Now())
	pages, err := r.takePages(ctx, pending)
	c.mu.Lock()
	c.pages = pages
	c.mu.Unlock()
	if err != nil {
		slog.ErrorContext(ctx, "vmmemory: a seal could not take its pages into the checkpoint",
			"pages", len(pages), "of", len(pending), "kind", r.kind, "error", err)
	}
}

// takePages detaches the checkpoint's copy of every page of the sealed set, in
// ascending page order and in bounded batches. Caller holds the exclusive
// memory region lock for the whole of it.
func (r *MemoryRegion) takePages(ctx context.Context, pending map[uint64]*binding) ([]*binding, error) {
	h := r.host
	bindings := sortedBindings(pending)
	// One slab of copies rather than one allocation each: a checkpoint of a
	// guest's whole working set is millions of them at a 4 KiB page.
	copies := make([]binding, len(bindings))
	pages := make([]*binding, 0, len(bindings))
	for len(bindings) > 0 {
		count := min(len(bindings), checkpointBatchPages)
		var locked []*resident
		taken := len(pages)
		err := func() error {
			defer func() { h.unlockAll(locked) }()
			var reclaiming []*binding
			for i, b := range bindings[:count] {
				pg, busy := h.tryCurrent(b)
				if pg != nil {
					locked = append(locked, pg)
				}
				held := &copies[taken+i]
				*held = binding{memoryRegion: r, index: b.index, dirty: true, spillSlot: b.spillSlot}
				switch {
				case pg != nil:
					h.bind(held, pg)
				case busy:
					// A reclaim is the only thing that can hold a resident page
					// of the memory region being sealed, and it is writing that page's
					// bytes to this page's reservation, which the checkpoint's
					// copy is taking over. Joining the copy to that page without
					// waiting for the reclaim keeps the walk off its I/O.
					h.joinReclaiming(held, b)
				}
				if busy {
					// The reclaim takes the page's mapping away, which is
					// stronger than the write protection the seal left on it and
					// is what the guest has to fault through either way.
					reclaiming = append(reclaiming, b)
				}
				r.holdInCheckpoint(b, held)
				if h.measuring() {
					r.sealSums(b.index, held)
				}
				pages = append(pages, held)
			}
			for _, b := range reclaiming {
				if err := h.revoke(ctx, b); err != nil {
					return err
				}
			}
			return nil
		}()
		// Pages count as sealed when the checkpoint takes them, so a capture that
		// never completes still reports the work its walk paid for.
		h.mu.Lock()
		h.stats.CheckpointPages += uint64(len(pages) - taken)
		h.mu.Unlock()
		if err != nil {
			return pages, err
		}
		bindings = bindings[count:]
	}
	return pages, nil
}

// protectDirtyRuns is the whole of a seal's pause: the runs of the dirty set,
// write-protected. The memory region's protection is held exclusively across reading
// them and issuing the commands, because the one thing that can take a mapping
// away while the seal holds the memory region is a reclaim, which revokes its victim's
// pages under that page's lock alone — and the seal no longer holds those
// locks. Under it, every revocation is either finished, and out of the runs
// read here, or has not begun.
func (r *MemoryRegion) protectDirtyRuns(ctx context.Context) ([]PageRun, error) {
	if err := r.protectMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer r.protectMu.Unlock()
	return r.protect(ctx, r.sealableRuns())
}

// protect takes write access away from every run of the dirty set, leaving
// their mappings, memory and page tables exactly as they are: the guest keeps
// reading the pages the checkpoint holds and traps on its next store to one.
// Nothing is copied, nothing is mapped, and no page is left without a mapping
// for the guest to fault on. One range command covers a run of consecutive
// pages whatever memory they hold, so the pause pays for runs, not pages. It
// reports the runs that landed, which a failure has to undo.
func (r *MemoryRegion) protect(ctx context.Context, runs []PageRun) ([]PageRun, error) {
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
			// not a memory region that is over: the undo above takes the protection off
			// every run that landed and the next checkpoint takes the whole
			// dirty set. Only a command that actually failed leaves the memory region
			// in a state nothing can reason about, and only that is terminal.
			if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
				return runs[:commands], err
			}
			return runs[:commands], r.fail(err)
		}
		commands++
		pages += run.Count
	}
	return runs, nil
}

// unprotect takes the mappings of the runs a failed seal write-protected away,
// so the guest's next store to one of those pages faults and maps it writable
// again rather than resolving a store against a read-only mapping. It is the
// whole of what such a seal has to undo, because the pause takes nothing else.
// Caller holds the exclusive memory region lock.
func (r *MemoryRegion) unprotect(ctx context.Context, runs []PageRun) error {
	var bindings []*binding
	for _, run := range runs {
		for page := run.Page; page < run.Page+uint64(run.Count); page++ {
			bindings = append(bindings, r.binding(page))
		}
	}
	return r.revokeLocked(ctx, bindings)
}

// DirtyPages reports the pages this checkpoint publishes, in ascending order. A
// pager page is a store page, so these are the store pages a checkpoint
// republishes.
func (c *MemoryRegionCheckpoint) DirtyPages() []uint64 {
	held := c.sealedPages()
	pages := make([]uint64, 0, len(held))
	for _, held := range held {
		pages = append(pages, held.index)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	return pages
}

// UnpublishedAge is how long the oldest write this checkpoint holds has gone
// unpublished, zero where it holds none. It is the memory region's loss window as the
// seal took it, still running: a fork point's hold can outlast several
// intervals, and the child that inherits these pages inherits their age with
// them rather than starting a window of its own.
func (c *MemoryRegionCheckpoint) UnpublishedAge() time.Duration {
	since := c.since()
	if since.IsZero() {
		return 0
	}
	return max(c.memoryRegion.host.clock.Since(since), 0)
}

// Share names every page this checkpoint holds by the identity the checkpoint
// publishing it gives that page, so a memory region that inherits the identity maps
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
func (c *MemoryRegionCheckpoint) Share(ctx context.Context, ref control.Ref, volume string) error {
	h := c.memoryRegion.host
	c.held.Store(true)
	for _, held := range c.sealedPages() {
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
// memory region's: the guest goes on faulting and storing while a checkpoint reads its
// checkpoint.
func (c *MemoryRegionCheckpoint) ReadDirty(ctx context.Context, page uint64, dst []byte) error {
	h := c.memoryRegion.host
	if uint64(len(dst)) != h.pageSize {
		return ErrRange
	}
	select {
	case <-c.done:
		// Retired, abandoned or discarded: these pages are the guest's own
		// state again or the volume's, and the memory behind them holds whatever
		// has happened since, which is not what this checkpoint holds. A memory region
		// that discarded it reports why it ended.
		if c.err != nil {
			return c.err
		}
		return ErrNotSealed
	default:
	}
	held := c.heldPage(page)
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
// Either way the memory region publishes again once this returns. It is idempotent: a
// checkpoint already retired, or one a detached memory region discarded, reports
// success.
func (c *MemoryRegionCheckpoint) Retire(ctx context.Context, published bool) error {
	return c.memoryRegion.endSeal(ctx, c, published)
}

// Unseal abandons a checkpoint no publication is going to take and ends the
// seal. Its pages become ordinary dirty state again, exactly as they were
// before the seal, and the next checkpoint takes them. A capture whose
// publication landed does not come here: that checkpoint is retired by
// MemoryRegionCheckpoint.Retire, which makes its pages clean under the checkpoint
// that holds them.
//
// Guest stores never waited for the seal: with copy-on-write, what it holds back
// is the memory region's next checkpoint, not the guest.
func (r *MemoryRegion) Unseal(ctx context.Context) error {
	return r.endSeal(ctx, nil, false)
}

// endSeal retires or abandons the memory region's checkpoint and ends the seal. A
// failure leaves the checkpoint in place, so the caller can repeat the call
// with a live context; nothing else may seal this memory region until it succeeds.
//
// The set is as large as a capture's, so it is walked in the same bounded
// batches a seal takes, and the memory region is given back between them: a fault
// waits for one batch, not for the walk. Each batch's volume metadata is looked
// up first, with neither the memory region nor any page held. A page already retired
// is skipped, so a repeated call finishes what a failed one left.
func (r *MemoryRegion) endSeal(ctx context.Context, checkpoint *MemoryRegionCheckpoint, published bool) error {
	// The walk gives the memory region up between batches, so what keeps the
	// checkpoint this memory region's for the whole of it is the lifetime lock and one
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
	for pages := current.sealedPages(); len(pages) > 0; {
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
		// written in is the memory region's again. A window that restarted here would
		// bound nothing: the host that cannot publish is exactly the host whose
		// publications keep failing.
		r.restoreDirtySince(current.since())
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
//
// The pages this retire hands back have their mappings taken away first, all of
// them together: see revokeHandedBack.
func (r *MemoryRegion) finalizeCheckpoint(ctx context.Context, held []*binding, identities map[uint64]storedPage) error {
	h := r.host
	if err := r.revokeHandedBack(ctx, held, identities); err != nil {
		return err
	}
	for _, checkpoint := range held {
		if checkpoint.spillSlot < 0 {
			continue
		}
		b := r.lookupBinding(checkpoint.index)
		shared := b != nil && r.heldBy(checkpoint.index, checkpoint)
		now := identities[checkpoint.index]
		id, stored := now.id, now.stored
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
			h.probe.retired(b)
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

// revokeHandedBack takes the guest's mapping away from every page of one retire
// batch the retire is about to hand back: the volume holds no object for it, so
// it is a hole again and there is nothing to put in that mapping's place. It is
// the one revocation a published retire issues, and it goes as one command per
// run of consecutive pages, before the walk.
//
// What a retire hands back at a 4 KiB page is the write-ahead pages the guest
// never stored into, and write-ahead makes them in runs — thousands of pages of
// them in one checkpoint, where a round trip each, serialized on the mapping
// lock, is a stall the guest feels. A GCE fan-out of two forks on 2026-09-23
// spent 12,428 revocations against 15,477 write-ahead pages on exactly that, one
// command per page. The walk then finds these pages unmapped and revokes
// nothing; the one page it can still revoke by itself is a page whose identity
// another resident already holds, which is rare and alone.
//
// Deciding a hand-back needs no page's lock: the identity the volume now reports
// was read before this batch took the memory region, and whether the guest still shares
// the checkpoint's copy moves only under the exclusive memory region lock this holds.
// Reading a page to check that it may be handed back needs that page's lock and
// nothing wider, and it runs here, before a mapping has moved, so ErrUndroppable
// still leaves the checkpoint durable, the pages sealed and the guest holding
// its memory — one fault per page it had mapped, and not a byte lost.
func (r *MemoryRegion) revokeHandedBack(ctx context.Context, held []*binding, identities map[uint64]storedPage) error {
	h := r.host
	var guests []*binding
	for _, checkpoint := range held {
		if checkpoint.spillSlot < 0 {
			continue
		}
		now := identities[checkpoint.index]
		if now.stored && !now.id.zero() {
			// The volume holds an object for this page, so the retire publishes
			// it and the guest keeps reading it through the mapping it has.
			continue
		}
		b := r.lookupBinding(checkpoint.index)
		if b == nil || !r.heldBy(checkpoint.index, checkpoint) {
			// The guest stored into the page since the seal, so only the
			// checkpoint's own copy is given up and no mapping of the guest's
			// names it.
			continue
		}
		if err := h.locked(ctx, checkpoint, func(pg *resident) error {
			return h.droppable(ctx, b, pg, now.stored, now.id)
		}); err != nil {
			return err
		}
		if r.isMapped(b) {
			guests = append(guests, b)
		}
	}
	return r.revokeLocked(ctx, guests)
}

// abandonPages gives up on a checkpoint's pages. A page the guest still shares
// with the checkpoint takes its copy's reservation back and is dirty again, so
// nothing the guest wrote before the seal is lost; a page the guest copied away
// from needs only its own newer state, so the checkpoint's copy is released.
// Caller holds the exclusive memory region lock.
//
// The reservation moves under the resident page's lock, together with the
// unlink that takes the checkpoint's alias away: an eviction that saw the two
// apart would find a private page with neither a reservation of its own nor a
// checkpoint still holding one, skip spilling it, and punch the page.
func (r *MemoryRegion) abandonPages(ctx context.Context, pages []*binding) error {
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
			if h.measuring() {
				r.unsealSums(b.index, held)
			}
			// An abandoned checkpoint gives the page straight back: the guest
			// may store into it again, and into these very bytes.
			h.probe.granted(b, pg, nil)
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
// memory region does: its memory users are gone and unpublished stores are discarded
// with them.
func (r *MemoryRegion) discardCheckpoint(ctx context.Context, checkpoint *MemoryRegionCheckpoint) error {
	h := r.host
	for _, held := range checkpoint.sealedPages() {
		if err := h.locked(ctx, held, func(pg *resident) error {
			// The seal ends with the memory region, so its name for the page does
			// too. Whatever still shares the page keeps it: nothing can store
			// into it any more, and its bytes stay what the seal froze.
			h.unshare(pg)
			return h.unlink(ctx, held, pg)
		}); err != nil {
			return err
		}
		if origin := held.origin; origin != nil {
			held.origin = nil
			if err := h.releaseOrigin(ctx, origin); err != nil {
				return err
			}
		}
		if held.spillSlot >= 0 {
			h.releaseSpill(held.spillSlot)
			held.spillSlot, held.dirty = -1, false
		}
	}
	checkpoint.finish(ErrClosed)
	return nil
}
