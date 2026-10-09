package vmmemory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A checkpoint is Zircon's writeback with the plan's departures
// (plans/zircon-pager-port-2026-10-05.md, "Dirty tracking, seal, checkpoint
// and flush").
//
//   - The seal is WritebackBegin with D3: the pause write-protects the runs
//     of the dirty set and takes the set whole, and the walk behind it, with
//     the guest running and the region held, makes each resident page of the
//     set AwaitingClean in the region's layer and hands its reservation to
//     the checkpoint's copy of it, a binding beside the layer.
//   - A store into a page the checkpoint holds splits it (D1): the store's copy
//     is the Dirty page, and the checkpoint keeps the page of its pause
//     beside the page list (zirconvm SplitAwaitingClean).
//   - The retire of a published checkpoint is WritebackEnd and a move of each
//     page out of the layer into the identity root of the checkpoint that
//     published it. A page the volume holds no object for is given back.
//   - The abandon of one that did not land is D4: WritebackAbandon makes each
//     page Dirty again with its own reservation, and its read-only mapping
//     goes.
//   - The settle stays the pager's: a sealed page whose bytes are its origin's
//     leaves the checkpoint, and the guest maps the origin again.
//
// Every transition holds the region: the walk and each batch of a retire, an
// abandon or a settle hold it exclusively, so no fault of the region decides
// meanwhile.

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
	mu sync.Mutex
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
	// digests is what each page's bytes hashed to when the publication read
	// them, in an isolated arena, and fork the file this checkpoint lends its
	// pages to children on this host in. Both are guarded by mu.
	digests map[uint64]digest
	fork    *arenaFile
	// copies is the set, in ascending page order: the checkpoint's copy of
	// each page, beside the region's layer. Guarded by mu.
	copies []*binding
	// unjournaled is the pages the region had not journaled at the seal, which
	// a capture takes until the guest stores into them or the seal ends
	// (journal.go). Guarded by the region's bindingsMu.
	unjournaled pageRuns
	done        chan struct{}
	err         error // read only after done is closed
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
//
// An ephemeral disk's seal takes nothing and records no checkpoint: no
// checkpoint holds its pages, and a VMM asks every memory region it maps to
// seal for a capture, so it succeeds rather than failing the capture.
func (r *MemoryRegion) Seal(ctx context.Context) error {
	h := r.host
	defer func(start time.Time) { h.sealLatency.Observe(h.clock.Since(start)) }(h.clock.Now())
	if err := wlockAdmitted(ctx, "vmmemory/region", r.mu); err != nil {
		return err
	}
	held := false
	defer func() {
		if !held {
			r.unlock()
		}
	}()
	if err := r.ready(); err != nil {
		return err
	}
	if r.Ephemeral() {
		return nil
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
	// The window moves to the checkpoint with the pages it is measured over,
	// once the protection has succeeded: a seal that protected nothing takes
	// nothing else either.
	checkpoint := &MemoryRegionCheckpoint{memoryRegion: r, dirtySince: r.takeDirtySince(),
		done: make(chan struct{}), taken: make(chan struct{}), unjournaled: r.takeUnjournaled(ctx)}
	pending := r.takeDirtySet()
	r.setCheckpoint(checkpoint)
	// The region stays locked, and the walk gives it back.
	held = true
	go r.take(context.WithoutCancel(ctx), checkpoint, pending)
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
	return nil
}

// sealSeam runs in a seal between write-protecting the dirty set and recording
// the checkpoint on the memory region. It is nil in production; a test installs one to
// wake a store waiting for the dirty budget in that pause. sealWalkSeam runs in
// the walk behind that pause, before it takes its first page, which is where a
// test holds the walk to see what the pause alone cost.
var sealSeam, sealWalkSeam func()

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

// DirtyPages reports the pages this checkpoint publishes, in ascending order. A
// pager page is a store page, so these are the store pages a checkpoint
// republishes.
func (c *MemoryRegionCheckpoint) DirtyPages() []uint64 {
	copies := c.copiesOf()
	pages := make([]uint64, 0, len(copies))
	for _, held := range copies {
		pages = append(pages, held.index)
	}
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
//
// The checkpoint's resident pages become a temporary identity root under the
// name the fork point lends them, so a child of the point on this host maps
// them rather than reading them, until the seal ends (plan: Fork sharing).
func (c *MemoryRegionCheckpoint) Share(ctx context.Context, ref control.Ref, volume string) error {
	r := c.memoryRegion
	h := r.host
	if err := r.inTenant(ref); err != nil {
		return err
	}
	// The end of the seal takes back what a Share lent, so the two run one at
	// a time: a Share holds what endSeal holds, and a Detach, which discards
	// the seal, waits for it too. A seal that has ended lends nothing, and a
	// child reads what it inherited through its own backing. Without this a
	// Share after the end registered a root no end would ever take back.
	if !sim.Bug(ctx, "pager-share-an-ended-seal") {
		if err := rlockAdmitted(ctx, "vmmemory/live", r.live); err != nil {
			return err
		}
		defer r.live.RUnlock()
		if err := lockAdmitted(ctx, "vmmemory/end", r.endMu.TryLock, r.endMu.WaitFree); err != nil {
			return err
		}
		defer r.endMu.Unlock()
		if r.currentCheckpoint() != c {
			return nil
		}
	}
	c.held.Store(true)
	if h.isolated() {
		h.mu.Lock()
		h.lent[lentKey{ref, volume}] = c
		h.mu.Unlock()
	}
	root := r.host.lentRoot(c, rootKey{ref: ref, volume: volume})
	for _, held := range c.copiesOf() {
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if page == nil {
			// Spilled: whoever inherits it reads it through its own backing,
			// which reaches these same bytes through the seal.
			continue
		}
		r.host.lend(ctx, root, page, held.index)
		r.host.unlockPage(page)
	}
	return nil
}

// ReadDirty fills dst, exactly one pager page, with the bytes the seal froze. A
// store the guest made since then copied that page away from the checkpoint, so
// it is not in them. It takes an I/O permit and the page's lock, never the
// memory region's: the guest goes on faulting and storing while a checkpoint reads its
// checkpoint.
//
// The bytes are the checkpoint's copy's page, or its reservation where it was
// spilled.
func (c *MemoryRegionCheckpoint) ReadDirty(ctx context.Context, page uint64, dst []byte) error {
	r := c.memoryRegion
	h := r.host
	if uint64(len(dst)) != h.pageSize {
		return ErrRange
	}
	// The region is held live from the look at the seal to the read's end: a
	// detach discards the seal and frees its reservations, and it waits for
	// this, so the copy read is never one it has freed. A read after the
	// detach finds the seal over. Nothing here waits for what a detach holds
	// first: it takes the region live before anything else.
	if !sim.Bug(ctx, "pager-read-dirty-beside-a-detach") {
		if err := rlockAdmitted(ctx, "vmmemory/live", r.live); err != nil {
			return err
		}
		defer r.live.RUnlock()
	}
	select {
	case <-c.done:
		if c.err != nil {
			return c.err
		}
		return ErrNotSealed
	default:
	}
	held := c.copyOf(page)
	if held == nil {
		return ErrRange
	}
	release, err := h.beginCheckpointIO(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := r.readHeld(ctx, held, dst); err != nil {
		return err
	}
	if h.isolated() {
		// The digest is of exactly what the upload is given, which a copy into
		// another file is checked against when another region inherits it.
		sum := digestOf(dst)
		c.mu.Lock()
		if c.digests == nil {
			c.digests = make(map[uint64]digest)
		}
		c.digests[page] = sum
		c.mu.Unlock()
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
//
// What a fork point lends ends page by page, not before the walk: each page's
// lent name goes under the page's lock as the page leaves the seal, before it
// is handed back to its guest or to the arena (unlend), unless the page moves
// into the point's root, where adopt takes the name out. A child maps or
// copies a lent page only holding that same lock (lookup, forkCopy), so none
// reaches a page once it has left the seal, and one that reached it before
// mapped the bytes the seal froze, which dropSharers then takes from it where
// the guest will store into them again.
func (r *MemoryRegion) endSeal(ctx context.Context, checkpoint *MemoryRegionCheckpoint, published bool) error {
	if err := rlockAdmitted(ctx, "vmmemory/live", r.live); err != nil {
		return err
	}
	defer r.live.RUnlock()
	if err := lockAdmitted(ctx, "vmmemory/end", r.endMu.TryLock, r.endMu.WaitFree); err != nil {
		return err
	}
	defer r.endMu.Unlock()
	current := r.currentCheckpoint()
	if current == nil || (checkpoint != nil && current != checkpoint) {
		return nil
	}
	for copies := current.copiesOf(); len(copies) > 0; {
		batch := copies[:min(len(copies), checkpointBatchPages)]
		var identities map[uint64]storedPage
		if published {
			var err error
			if identities, err = r.storedIdentities(ctx, batch); err != nil {
				return err
			}
		}
		if err := wlockAdmitted(ctx, "vmmemory/region", r.mu); err != nil {
			return err
		}
		err := func() error {
			defer r.unlock()
			if published {
				return r.finalizeCheckpoint(ctx, current, batch, identities)
			}
			return r.abandonCopies(ctx, batch)
		}()
		if err != nil {
			return err
		}
		copies = copies[len(batch):]
		// The region is given back here, and each batch decides every page of
		// it again under the region's next hold: a store meanwhile may have
		// copied a page of a later batch away from the checkpoint. What the
		// batches never decide again is the copies themselves, which only a
		// settle changes, and a settle is over before a publication retires
		// (MemoryRegionCheckpoint.Settle). In a controlled run another task
		// goes on here.
		if err := sim.Admit(ctx, "vmmemory/retire-batch"); err != nil {
			return err
		}
	}
	if err := wlockAdmitted(ctx, "vmmemory/region", r.mu); err != nil {
		return err
	}
	defer r.unlock()
	if !published {
		r.restoreDirtySince(current.since())
	}
	r.endUnjournaled(current, published)
	if err := r.endFork(ctx, current); err != nil {
		return err
	}
	r.setCheckpoint(nil)
	current.finish(nil)
	r.host.mu.Lock()
	r.host.signal()
	r.host.mu.Unlock()
	return nil
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

// protectDirtyRuns is the whole of the pause: the runs of the dirty set,
// write-protected, with the region's protection held exclusively, so no
// revocation runs beside it.
func (r *MemoryRegion) protectDirtyRuns(ctx context.Context) ([]PageRun, error) {
	if err := wlockAdmitted(ctx, "vmmemory/protection", r.protectMu); err != nil {
		return nil, err
	}
	defer r.protectMu.Unlock()
	r.bindingsMu.Lock()
	runs := r.dirtyRuns.runs(uint64(r.pageCount))
	r.bindingsMu.Unlock()
	return r.protect(ctx, runs)
}

// unprotect takes away the mappings of the runs a failed seal protected, so
// the guest's next store to one faults and maps it writable again. Caller
// holds the region exclusively.
func (r *MemoryRegion) unprotect(ctx context.Context, runs []PageRun) error {
	var bindings []*binding
	r.bindingsMu.Lock()
	for _, run := range runs {
		for page := run.Page; page < run.Page+uint64(run.Count); page++ {
			bindings = append(bindings, r.bindingLocked(page))
		}
	}
	r.bindingsMu.Unlock()
	return r.revokeBindings(ctx, bindings)
}

// takeDirtySet hands the whole dirty set to a seal in one step and leaves the
// region with none. Caller holds the region exclusively.
func (r *MemoryRegion) takeDirtySet() map[uint64]*binding {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pending := r.dirtySet
	r.dirtySet = nil
	r.dirtyRuns = newPageRuns(r.host.pageSize)
	return pending
}

// take is the walk behind the pause, as MemoryRegionCheckpoint.take: it
// makes every page of the sealed set the checkpoint's, with the guest
// running and the region the seal took held, and gives the region back.
func (r *MemoryRegion) take(ctx context.Context, c *MemoryRegionCheckpoint, pending map[uint64]*binding) {
	h := r.host
	defer r.unlock()
	defer close(c.taken)
	if sealWalkSeam != nil {
		sealWalkSeam()
	}
	defer func(start time.Time) { h.sealWalkLatency.Observe(h.clock.Since(start)) }(h.clock.Now())
	// A cold copy the guest did not change is no part of any checkpoint.
	if _, err := r.leaveOutColdCopies(ctx, pending); err != nil {
		slog.ErrorContext(ctx, "vmmemory: a seal could not compare its cold copies", "kind", r.kind, "error", err)
	}
	copies, err := r.takePages(ctx, pending)
	c.mu.Lock()
	c.copies = copies
	c.mu.Unlock()
	if err != nil {
		slog.ErrorContext(ctx, "vmmemory: a seal could not take its pages into the checkpoint",
			"pages", len(copies), "of", len(pending), "kind", r.kind, "error", err)
	}
}

// takePages makes the checkpoint's copy of every page of the sealed set, in
// ascending page order and in bounded batches: the copy aliases the page the
// guest has, which becomes AwaitingClean in the layer (WritebackBegin), and
// takes the reservation it was admitted under and where it was copied from.
// The guest's binding shares the copy until a store copies away from it.
// Caller holds the region exclusively.
func (r *MemoryRegion) takePages(ctx context.Context, pending map[uint64]*binding) ([]*binding, error) {
	h := r.host
	ps := h.pageSize
	bindings := make([]*binding, 0, len(pending))
	for _, b := range pending {
		bindings = append(bindings, b)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].index < bindings[j].index })
	// One slab of copies rather than an allocation each.
	slab := make([]binding, len(bindings))
	copies := make([]*binding, 0, len(bindings))
	for len(bindings) > 0 {
		count := min(len(bindings), checkpointBatchPages)
		batch := bindings[:count]
		taken := len(copies)
		var resident []bool
		h.mu.Lock()
		for i, b := range batch {
			held := &slab[taken+i]
			*held = binding{region: r, index: b.index, dirty: true}
			own := b.page != nil && frameOf(b.page).layer == r
			if b.page != nil {
				r.host.aliasLocked(held, b.page)
			}
			resident = append(resident, own)
		}
		h.mu.Unlock()
		// The region is held exclusively, so between here and the writeback
		// nothing changes a page of the batch but an eviction, which holds the
		// page's lock and no region's: a prefetch takes no dirty page. One may
		// take a page of the batch now. It reads a page's
		// aliases before their reservations, and again while the aliases grow
		// (evictPage), and the copy joined them above, so whichever side of the
		// move below it reads, the bytes reach the reservation the copy ends up
		// with. The page it took is out of the layer, so resident may name a
		// page that is gone, and the writeback, which looks at what the layer
		// holds, skips it. In a controlled run another task goes on here.
		if err := sim.Admit(ctx, "vmmemory/seal-take"); err != nil {
			// The walk's context is never cancelled, and the walk takes every
			// page of the set whatever this says.
			slog.ErrorContext(ctx, "vmmemory: a seal's walk went on without its admission", "kind", r.kind, "error", err)
		}
		r.bindingsMu.Lock()
		for i, b := range batch {
			r.holdInCheckpointLocked(b, &slab[taken+i])
		}
		r.bindingsMu.Unlock()
		for i, b := range batch {
			if h.measuring() {
				r.sealSums(b.index, &slab[taken+i])
			}
			copies = append(copies, &slab[taken+i])
		}
		// Each run of consecutive pages the layer holds begins its writeback in
		// one call: its Dirty pages become AwaitingClean.
		var failure error
		for i := 0; i < count; {
			if !resident[i] {
				i++
				continue
			}
			run := 1
			for i+run < count && resident[i+run] && batch[i+run].index == batch[i].index+uint64(run) {
				run++
			}
			if err := r.layer.WritebackBegin(batch[i].index*ps, uint64(run)*ps, false); err != nil {
				failure = errors.Join(failure, err)
			}
			i += run
		}
		h.mu.Lock()
		h.stats.CheckpointPages += uint64(count)
		h.mu.Unlock()
		if failure != nil {
			return copies, failure
		}
		bindings = bindings[count:]
	}
	return copies, nil
}

// holdInCheckpointLocked hands b's reservation, where it was copied from and
// whether write-ahead made it to held, the checkpoint's copy, which b shares
// from here: it stays dirty, and a store must copy away from the checkpoint
// before it can change its bytes. Whether a journal capture write-protected
// b goes to held too: the seal's own protection stands in for it while the
// seal lasts, and an abandon gives it back (restoreFromCheckpointLocked).
// Caller holds r.bindingsMu.
func (r *MemoryRegion) holdInCheckpointLocked(b, held *binding) {
	r.uncoldLocked(b)
	held.protected, b.protected, b.zeroed = b.protected, false, false
	b.checkpoint, held.spill, b.spill = held, b.spill, noReservation
	held.ahead, b.ahead = b.ahead, false
	held.origin, b.origin = b.origin, nil
	delete(r.dirtySet, b.index)
	r.noteSealableLocked(b)
}

// copiesOf is the checkpoint's copies, in ascending page order, once the walk
// behind the pause has made them.
func (c *MemoryRegionCheckpoint) copiesOf() []*binding {
	<-c.taken
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.copies
}

// copyOf is the checkpoint's copy of one page, nil where it holds none.
func (c *MemoryRegionCheckpoint) copyOf(page uint64) *binding {
	copies := c.copiesOf()
	at := sort.Search(len(copies), func(i int) bool { return copies[i].index >= page })
	if at < len(copies) && copies[at].index == page {
		return copies[at]
	}
	return nil
}

// readHeld reads the bytes a checkpoint's copy holds: its page's, or its
// reservation's where it was spilled.
func (r *MemoryRegion) readHeld(ctx context.Context, held *binding, dst []byte) error {
	h := r.host
	page, err := r.host.lockedPage(ctx, held)
	if err != nil {
		return err
	}
	if page != nil {
		defer r.host.unlockPage(page)
		f := frameOf(page)
		return f.file.Read(ctx, f.slot, dst)
	}
	// The copy was spilled. A refault may give it a page again before the
	// reservation is read, and that page holds the reservation's bytes, which
	// the copy keeps: only a settle, the end of the seal and a detach take the
	// reservation away. A publication reads before the first two, and every
	// caller holds the region live, which keeps the third out.
	r.bindingsMu.Lock()
	spill := held.spill
	r.bindingsMu.Unlock()
	if readSpillSeam != nil {
		readSpillSeam()
	}
	return h.readSpill(ctx, spill, dst)
}

// readSpillSeam runs in a read of a spilled checkpoint copy between taking
// its reservation and reading it. It is nil in production; a test detaches
// the region there.
var readSpillSeam func()

// lentRoot is the temporary identity root of what c lends under key, made
// where there is none.
func (h *Host) lentRoot(c *MemoryRegionCheckpoint, key rootKey) *identityRoot {
	h.mu.Lock()
	defer h.mu.Unlock()
	root := h.rootLocked(key)
	root.lent = c
	return root
}

// lend names page, a checkpoint's, in root at index, where the root holds
// nothing there and no point lends it already. The root's page names the
// page's frame, and is never the root's to give back (lentFrame). Caller holds
// the page's lock.
func (h *Host) lend(ctx context.Context, root *identityRoot, page *zirconvm.VmPage, index uint64) {
	f := frameOf(page)
	h.mu.Lock()
	named := f.lent != nil
	h.mu.Unlock()
	if named {
		return
	}
	// Nothing names f until it is named below: every change to f.lent is made
	// with the page's lock held, which the caller holds, but dropLentRoot's,
	// and the end of the seal that runs it waits for this Share (endMu). A
	// supply of another read to the root at index is what supplyIfEmpty looks
	// for again. In a controlled run another task goes on here, and one that
	// wants the page waits for its lock.
	if err := sim.Admit(ctx, "vmmemory/lend"); err != nil {
		// Nothing is lent: a child reads the page through its own backing.
		return
	}
	lent := zirconvm.NewFramePage(lentFrame{frame: f})
	if !h.supplyIfEmpty(ctx, root.pages, index, lent) {
		return
	}
	// It is the parent's frame, which the parent's layer queues: the root's
	// page waits where no eviction looks.
	h.node.PageQueues().MoveToWired(lent)
	h.mu.Lock()
	f.lent = lent
	root.lentPages = append(root.lentPages, lent)
	h.mu.Unlock()
}

// unlend takes away the name a fork point lends page under, where one does:
// the page naming it in the point's temporary root goes, so a child that
// faults on the name from here reads it through its own backing. A page that
// leaves the checkpoint which froze it does this before its lock is given
// back, whether it goes back to its guest as dirty state or to the arena: a
// child that mapped it after would read the guest's next stores, or a slot
// given back. Caller holds the page's lock.
func (h *Host) unlend(ctx context.Context, page *zirconvm.VmPage) {
	if sim.Bug(ctx, "pager-lend-a-page-past-its-checkpoint") {
		// The bug leaves the name until the seal ends.
		return
	}
	h.dropLentName(page)
}

// dropLentName takes the page naming page in a fork point's temporary root
// out of that root, where there is one: unlend's work, which a page leaving
// every object does too (removeFromObject). Caller holds the page's lock.
func (h *Host) dropLentName(page *zirconvm.VmPage) {
	f := frameOf(page)
	h.mu.Lock()
	lent := f.lent
	f.lent = nil
	h.mu.Unlock()
	if lent == nil {
		return
	}
	if link, ok := h.node.PageQueues().Backlink(lent); ok {
		lock := link.Cow.Lock()
		lock.Lock()
		link.Cow.RemovePageLocked(link.Offset, lent)
		lock.Unlock()
	}
}

// supplyIfEmpty supplies one page to an object at index, where the object
// holds nothing there, and reports whether it did. Where it did not, the page
// is still the caller's: a pager's supply would give it back.
func (h *Host) supplyIfEmpty(ctx context.Context, object *zirconvm.CowPages, index uint64,
	page *zirconvm.VmPage) bool {
	ps := h.pageSize
	deferred := zirconvm.NewDeferredOps(object)
	defer deferred.Finish()
	lock := object.Lock()
	lock.Lock()
	defer lock.Unlock()
	if object.PageLocked(index*ps) != nil {
		return false
	}
	list := h.splices.Get().(*zirconvm.PageSpliceList[zirconvm.VmPage])
	list.Initialize(ps)
	if err := list.Insert(0, zirconvm.Page(page)); err != nil {
		panic("vmmemory: building a supply: " + err.Error())
	}
	list.Finalize()
	if err := object.SupplyPagesLocked(zirconvm.CowRange{Offset: index * ps, Len: ps}, list, zirconvm.PagerSupply,
		deferred); err != nil {
		panic("vmmemory: supplying a page to an object that holds none there: " + err.Error())
	}
	list.Reuse()
	h.splices.Put(list)
	return true
}

// retireFromCheckpointLocked ends b's dirty epoch: its bytes are the volume's
// now, or its origin's. Caller holds r.bindingsMu.
func (r *MemoryRegion) retireFromCheckpointLocked(b *binding) {
	r.host.probe.retired(b)
	r.uncoldLocked(b)
	b.checkpoint, b.dirty, b.origin = nil, false, nil
	delete(r.dirtySet, b.index)
	r.noteSealableLocked(b)
}

// restoreFromCheckpointLocked hands an abandoned checkpoint's copy back to
// the page it was taken from: its reservation, where it was copied from and
// whether write-ahead made it, and the page is dirty again, exactly as it was
// before the seal: a page a journal capture had write-protected is
// write-protected again, so it maps writable only through the protect trap
// that makes it unjournaled. Caller holds r.bindingsMu.
func (r *MemoryRegion) restoreFromCheckpointLocked(b, held *binding) {
	r.uncoldLocked(b)
	b.checkpoint, b.spill, b.dirty, b.ahead = nil, held.spill, true, held.ahead
	b.origin, held.origin = held.origin, nil
	b.protected = held.protected
	held.spill, held.dirty, held.ahead, held.protected = noReservation, false, false, false
	if r.dirtySet == nil {
		r.dirtySet = make(map[uint64]*binding)
	}
	r.dirtySet[b.index] = b
	r.noteSealableLocked(b)
}

// storedIdentities is the identity the volume now gives each page of one
// retire batch, located once per read-ahead window, with neither the region
// nor any page held.
func (r *MemoryRegion) storedIdentities(ctx context.Context, batch []*binding) (map[uint64]storedPage, error) {
	result := make(map[uint64]storedPage, len(batch))
	var window *plan
	for _, held := range batch {
		if r.spillOf(held).none() {
			continue
		}
		index := held.index
		if window == nil || index < window.start || index >= window.end {
			start, end := r.window(index)
			var err error
			if window, err = r.plan(ctx, start, end, end); err != nil {
				return nil, err
			}
		}
		id, stored := window.identity(index)
		result[index] = storedPage{id, stored}
	}
	return result, nil
}

// spillOf is the reservation b holds.
func (r *MemoryRegion) spillOf(b *binding) reservation {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	return b.spill
}

// finalizeCheckpoint retires every page of one batch of a published
// checkpoint, as MemoryRegion.finalizeCheckpoint does. A page the guest still
// shares with the checkpoint is the volume's now: it becomes Clean, leaves
// the layer for the identity root of its identity, and stays mapped. A page
// the guest copied away from keeps only the checkpoint's copy, which goes.
// Either way the reservation goes back. Caller holds the region exclusively.
func (r *MemoryRegion) finalizeCheckpoint(ctx context.Context, c *MemoryRegionCheckpoint, batch []*binding,
	identities map[uint64]storedPage) error {
	h := r.host
	if err := r.revokeHandedBack(ctx, batch, identities); err != nil {
		return err
	}
	for _, held := range batch {
		spill := r.spillOf(held)
		if spill.none() {
			continue
		}
		// What shared and spill say holds until the page is done: every change
		// to a binding's checkpoint or to a copy's reservation is made with the
		// region held, by a fault, a seal, a settle, an end of a seal or a
		// detach, and this holds it exclusively. The locks given up between the
		// reads below guard the bindings, not the decision.
		b := r.lookupBinding(held.index)
		shared := b != nil && r.checkpointCopy(b) == held
		now := identities[held.index]
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if shared {
			r.bindingsMu.Lock()
			r.retireFromCheckpointLocked(b)
			r.bindingsMu.Unlock()
		}
		if page != nil {
			if shared {
				err = r.publish(ctx, c, b, held, page, now)
			} else {
				err = r.retireCopy(ctx, held, page, now)
			}
			// A page that did not move into a root is no longer the bytes
			// the seal froze: it went back, or it is the guest's own again.
			// The name a fork point lends it under goes with it now, while
			// its lock is held, and not when the seal ends: a child that
			// faults on the name in between would map it.
			if !r.host.published(page) {
				r.host.unlend(ctx, page)
			}
			r.host.unlockPage(page)
			if err != nil {
				return err
			}
		}
		// The page is done, and its lock given back: in a controlled run
		// another task goes on here, a child's fault on the page say. A retire
		// that stops here is repeated, and finds the page retired and its
		// reservation still to give back.
		if err := sim.Admit(ctx, "vmmemory/retire-page"); err != nil {
			return err
		}
		// Nothing reads the copy's reservation from here: the publication
		// that read it is over, and the copy names no page an eviction could
		// reach it through. Where it holds the copy's bytes, they are the
		// version the page was published as, and the spill file keeps them.
		h.retireSpill(spill, now)
		r.bindingsMu.Lock()
		held.spill, held.dirty = noReservation, false
		r.bindingsMu.Unlock()
	}
	return nil
}

// revokeHandedBack takes the guest's mapping away from every page of one
// retire batch the retire is about to hand back: the volume holds no object
// for it, so it is a hole again and there is nothing to put in that mapping's
// place. It goes as one command per run of consecutive pages, before the walk,
// once each page is known to be zeros (droppable).
//
// What a retire hands back at a 4 KiB page is the write-ahead pages the guest
// never stored into, and write-ahead makes them in runs: thousands of pages in
// one checkpoint, where a round trip each, serialized on the mapping lock, is
// a stall the guest feels. A GCE fan-out of two forks on 2026-09-23 spent
// 12,428 revocations against 15,477 write-ahead pages on exactly that, one
// command per page.
func (r *MemoryRegion) revokeHandedBack(ctx context.Context, batch []*binding, identities map[uint64]storedPage) error {
	var guests []*binding
	for _, held := range batch {
		if r.spillOf(held).none() {
			continue
		}
		now := identities[held.index]
		if now.stored && !now.id.zero() {
			continue
		}
		b := r.lookupBinding(held.index)
		if b == nil || r.checkpointCopy(b) != held {
			continue
		}
		if err := r.droppable(ctx, held, now); err != nil {
			return err
		}
		if r.isMapped(b) {
			guests = append(guests, b)
		}
	}
	return r.revokeBindings(ctx, guests)
}

// droppable refuses the one thing a retire may not get wrong. A page given up
// because the volume holds no object for it is a page the volume must be able
// to reproduce without one, and the only such page is zeros: a publication
// writes an all-zero page as a sparse hole and the volume reads it back as
// zeros. A page with anything else in it holds bytes that exist nowhere but
// here, so dropping it would hand the guest an older version of memory it
// wrote. It reads the page, which is why it runs only where a page is about to
// be dropped.
func (r *MemoryRegion) droppable(ctx context.Context, held *binding, now storedPage) error {
	h := r.host
	if now.stored && !now.id.zero() {
		return nil
	}
	page, err := r.host.lockedPage(ctx, held)
	if err != nil || page == nil {
		return err
	}
	defer r.host.unlockPage(page)
	f := frameOf(page)
	data := make([]byte, h.pageSize)
	if err := f.file.Read(ctx, f.slot, data); err != nil {
		return err
	}
	if allZero(data) {
		return nil
	}
	return fmt.Errorf("%w: page %d of %s", ErrUndroppable, held.index, r.kind)
}

// publish retires a page the guest still shares with the checkpoint: its
// writeback ends, so it is Clean, and it leaves the layer for the identity
// root of the identity the volume now gives it, where the guest goes on
// mapping it. A page the volume holds no object for, or whose identity
// another page holds already, is given back. A page of the region's private
// file the publication read no digest of stays the region's own Clean page:
// another region could not check a copy of it. Caller holds the region
// exclusively and the page's lock.
func (r *MemoryRegion) publish(ctx context.Context, c *MemoryRegionCheckpoint, b, held *binding,
	page *zirconvm.VmPage, now storedPage) error {
	h := r.host
	ps := h.pageSize
	h.mu.Lock()
	r.host.unaliasLocked(held)
	h.mu.Unlock()
	// The page's lock and the region keep everything below to this page:
	// nothing gains an alias of a page without holding its lock, nothing of
	// the region changes, and the adopt below is in a root from where another
	// region finds it, and waits for its lock, so the digest is in place
	// before any region can move the page out of the private file.
	if err := r.layer.WritebackEnd(b.index*ps, ps); err != nil {
		return err
	}
	f := frameOf(page)
	if now.stored && !now.id.zero() {
		sum := c.digestOf(b.index)
		if f.file.owner != nil && sum == nil {
			return nil
		}
		if r.host.adopt(ctx, r, page, b.index, now.id) {
			if f.file.owner != nil {
				h.mu.Lock()
				f.file.digests[f.slot] = *sum
				h.mu.Unlock()
			}
			return nil
		}
	}
	// The one hand-back that arrives alone: a page whose identity another
	// page holds. One the volume holds no object for was revoked with its
	// batch (revokeHandedBack), and this skips it. Only the region's own
	// faults map b again, and it is held exclusively, so b stays unmapped
	// until it lets the page go.
	if err := r.revoke(ctx, b); err != nil {
		return err
	}
	// In a controlled run another task goes on here. A retire that stops
	// here leaves the page the region's own Clean page, which its guest maps
	// again on its next fault, and is repeated.
	if err := sim.Admit(ctx, "vmmemory/hand-back"); err != nil {
		return err
	}
	h.mu.Lock()
	r.host.unaliasLocked(b)
	h.mu.Unlock()
	if _, err := r.host.dropSharers(ctx, page); err != nil {
		return err
	}
	r.layer.RemovePage(b.index*ps, page)
	r.host.giveUp(page)
	return nil
}

// retireCopy ends the checkpoint's own copy of a page the guest has stored
// into since the seal. It goes back with the checkpoint, unless children of
// a fork point on this host still map it: it holds what the checkpoint
// published, so it becomes the page of its identity where the volume gives
// it one and no other page holds it, and those children keep it. Caller
// holds the region exclusively and the page's lock.
func (r *MemoryRegion) retireCopy(ctx context.Context, held *binding, page *zirconvm.VmPage, now storedPage) error {
	h := r.host
	ps := h.pageSize
	h.mu.Lock()
	r.host.unaliasLocked(held)
	mapped := frameOf(page).aliases.len() > 0
	h.mu.Unlock()
	if !mapped {
		// The end of the writeback frees what the checkpoint held beside the
		// page list (D1).
		return r.layer.WritebackEnd(held.index*ps, ps)
	}
	if !r.layer.RemovePage(held.index*ps, page) {
		return fmt.Errorf("vmmemory: the checkpoint's copy of page %d is not in its region's layer", held.index)
	}
	if now.stored && !now.id.zero() && r.host.adopt(ctx, nil, page, held.index, now.id) {
		return nil
	}
	if _, err := r.host.dropSharers(ctx, page); err != nil {
		return err
	}
	r.host.giveUp(page)
	return nil
}

// adopt moves page into the identity root of id at the page id names, where
// that root holds nothing there, and reports whether it did. from is the
// layer that held it at index, out of which it is taken first, nil where it
// is out of every object already; a page adopt took out and could not move
// is out of every object, and the caller's to give back. The page is a
// root's from here: Clean, named by its identity, idle once nothing maps it.
// Caller holds the page's lock.
func (h *Host) adopt(ctx context.Context, from *MemoryRegion, page *zirconvm.VmPage, index uint64, id pageKey) bool {
	ps := h.pageSize
	root := h.root(rootOf(id))
	lock := root.pages.Lock()
	lock.Lock()
	existing := root.pages.PageLocked(id.id.Page * ps)
	if existing != nil && h.lentHere(root, existing) {
		// A fork point published the name it lent: its parent's own page
		// takes the name back, and the root is a published checkpoint's from
		// here, whose lent pages go when the seal ends.
		root.pages.RemovePageLocked(id.id.Page*ps, existing)
		h.mu.Lock()
		if f := frameOf(existing); isLent(existing) && f.lent == existing {
			f.lent = nil
		}
		root.published = true
		h.mu.Unlock()
		existing = nil
	}
	lock.Unlock()
	if existing != nil {
		return false
	}
	// Another read may supply the root at this page once its lock is given
	// up: a region that read the identity from its volume. supplyIfEmpty looks
	// again under the lock, and the page is then the caller's to give back,
	// whose guest reads the same bytes from the root. Nothing else changes
	// the page, whose lock the caller holds. In a controlled run another task
	// goes on here.
	if err := sim.Admit(ctx, "vmmemory/adopt"); err != nil {
		// Not moved: the caller gives the page back.
		return false
	}
	if from != nil && !from.layer.RemovePage(index*ps, page) {
		return false
	}
	if !h.supplyIfEmpty(ctx, root.pages, id.id.Page, page) {
		return false
	}
	h.mu.Lock()
	frameOf(page).layer = nil
	h.rootPages++
	h.idleLocked(page)
	h.mu.Unlock()
	return true
}

// abandonCopies abandons the copies of one batch (D4): a page the guest still
// shares with the checkpoint takes its copy's reservation back and is Dirty
// again in the layer, and its read-only mapping goes so the next store maps it
// writable; a page the guest copied away from needs only its own newer state,
// so the checkpoint's copy goes. Caller holds the region exclusively.
func (r *MemoryRegion) abandonCopies(ctx context.Context, batch []*binding) error {
	h := r.host
	ps := h.pageSize
	var restored []*binding
	for _, held := range batch {
		spill := r.spillOf(held)
		if spill.none() {
			continue
		}
		// What shared and spill say holds until the page is done, as in
		// finalizeCheckpoint: every change to either is made with the region
		// held, and this holds it exclusively.
		b := r.lookupBinding(held.index)
		shared := b != nil && r.checkpointCopy(b) == held
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if page != nil {
			// The guest takes the page back as dirty state it may store into in
			// place, so nothing else may still be reading it, nor map it from
			// here: the name a fork point lends it under goes now, while its
			// lock is held, and not when the seal ends.
			r.host.unlend(ctx, page)
			// A sharer that keeps the page is terminal, and reads nothing
			// the guest stores into it from here.
			if _, err := r.host.dropSharers(ctx, page, b, held); err != nil {
				r.host.unlockPage(page)
				return err
			}
			h.mu.Lock()
			r.host.unaliasLocked(held)
			h.mu.Unlock()
		}
		if shared {
			forget := sim.Bug(ctx, "journal-abandon-forgets-protection")
			r.bindingsMu.Lock()
			if forget {
				// The bug gives the page back writable, though no flush will
				// take what the guest stores into it.
				held.protected = false
			}
			r.restoreFromCheckpointLocked(b, held)
			r.bindingsMu.Unlock()
			// An abandoned checkpoint gives the page straight back: the guest
			// may store into it again, and into these very bytes.
			h.probe.granted(b, probeFrame(page), nil)
			if h.measuring() {
				r.unsealSums(b.index, held)
			}
			if page != nil {
				// AwaitingClean is Dirty again, with the reservation it holds
				// (D4).
				err = r.layer.WritebackAbandon(ctx, held.index*ps, ps)
			}
			if r.isMapped(b) {
				restored = append(restored, b)
			}
		} else {
			r.bindingsMu.Lock()
			held.spill, held.dirty = noReservation, false
			r.bindingsMu.Unlock()
			if page != nil {
				// The abandon frees what the checkpoint held beside the page
				// list.
				err = r.layer.WritebackAbandon(ctx, held.index*ps, ps)
			}
			h.releaseSpill(spill)
		}
		if page != nil {
			r.host.unlockPage(page)
		}
		if err != nil {
			return err
		}
		// The page is done, and its lock given back: in a controlled run
		// another task goes on here. A guest page restored above is still
		// mapped read-only, so a store into it waits for the region, and the
		// revocation below makes it fault again. An abandon that stops here is
		// repeated, and finds the pages before this one done.
		if err := sim.Admit(ctx, "vmmemory/abandon-page"); err != nil {
			return err
		}
	}
	return r.revokeBindings(ctx, restored)
}

// discardCheckpoint drops a checkpoint entirely, which detaching a region
// does: its pages go with the region's layer, and their reservations and the
// pages they were copied from go now.
func (r *MemoryRegion) discardCheckpoint(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	for _, held := range c.copiesOf() {
		page, err := r.host.lockedPage(ctx, held)
		if err != nil {
			return err
		}
		if page != nil {
			// The page goes with the layer: no child may map it from here,
			// nor find it under the name a fork point lends it under. The
			// name goes under the page's lock, which a child's map or copy
			// of it holds too, so ending the lending page by page leaves no
			// window (endSeal).
			r.host.unlend(ctx, page)
			_, err = r.host.dropSharers(ctx, page)
			h.mu.Lock()
			if held.page != nil {
				r.host.unaliasLocked(held)
			}
			h.mu.Unlock()
			r.host.unlockPage(page)
			if err != nil {
				return err
			}
		}
		r.bindingsMu.Lock()
		origin, spill := held.origin, held.spill
		held.origin, held.spill, held.dirty = nil, noReservation, false
		r.bindingsMu.Unlock()
		if origin != nil {
			r.host.dropOrigin(origin)
		}
		if !spill.none() {
			h.releaseSpill(spill)
		}
	}
	if err := r.endFork(ctx, c); err != nil {
		return err
	}
	c.finish(ErrClosed)
	return nil
}

// endFork takes back what a checkpoint lent when its seal ends: every
// temporary identity root it lent its pages under lends nothing from here,
// and is an ordinary root of its name. A child that maps one of its pages
// keeps mapping it: the page is its identity's now, and a child that faults
// on one the seal's end took back reads it through its own backing.
func (r *MemoryRegion) endFork(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	h.mu.Lock()
	for key, lender := range h.lent {
		if lender == c {
			delete(h.lent, key)
		}
	}
	// Each root stays the root of its name, as every root does: the pages a
	// child read under the name are in it, and the next child maps them.
	destroying := sim.Bug(ctx, "pager-destroy-a-lent-root-under-its-readers")
	var roots []*identityRoot
	for key, root := range r.host.roots {
		if root.lent != c {
			continue
		}
		roots = append(roots, root)
		root.lent = nil
		if destroying && !root.published {
			delete(r.host.roots, key)
			root.gone.Store(true)
		}
	}
	h.mu.Unlock()
	for _, root := range roots {
		if err := r.host.dropLentRoot(ctx, root); err != nil {
			return err
		}
	}
	return r.endForkFile(ctx, c)
}

// lentHere reports a page of a lent root that is not a published one: a page
// naming its parent's frame, or a copy in the point's file. Caller holds the
// root's lock.
func (h *Host) lentHere(root *identityRoot, page *zirconvm.VmPage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if root.lent == nil {
		return false
	}
	return isLent(page) || slices.Contains(root.copies, page)
}

// dropLentRoot takes what a temporary identity root lent away: each of its
// pages naming a parent's frame stops naming it, and each copy in the point's
// file goes, with every mapping of it. What else the root holds stays, and
// the root with it, an ordinary root of the name from here: the pages the
// point published under the name, and in an arena that shares them, the
// pages a child read under the name through its own backing, which the child
// maps. They go idle and are evicted as any root's pages do. Before
// 2026-10-07 a root the point did not publish was destroyed here, with every
// page a child had read into it, and the pager panicked freeing a page a
// child mapped.
func (h *Host) dropLentRoot(ctx context.Context, root *identityRoot) error {
	// The pages naming a parent's frame go first, each under its lock. A
	// child's fork copy holds the lent page it replaces from start to end,
	// and its copy joins root.copies before it lets go (forkCopy): once each
	// lent page's lock has been taken here, every copy is in root.copies, and
	// no copy starts after, because no lookup finds a lent page in the root.
	// The seal's end takes most lent pages out under their locks before
	// this, but not a page a retire published under another name, whose lent
	// page stays. Before 2026-10-07 the copies were taken first, and a
	// child's copy of such a page made meanwhile stayed in the root with its
	// file gone: the next child to find it panicked giving itself the file.
	if !sim.Bug(ctx, "pager-drop-lent-copies-before-their-lent-pages") {
		if err := h.unlendRoot(ctx, root); err != nil {
			return err
		}
	}
	// A copy in the point's file goes with the root: every child that maps
	// it reads it through its own backing from here.
	h.mu.Lock()
	copies := root.copies
	root.copies = nil
	h.mu.Unlock()
	if dropLentRootSeam != nil {
		dropLentRootSeam()
	}
	for _, page := range copies {
		if err := h.lockPage(ctx, page); err != nil {
			return err
		}
		_, err := h.dropSharers(ctx, page)
		if err == nil && frameOf(page).slot >= 0 {
			h.removeFromObject(page)
			h.giveUp(page)
		}
		h.unlockPage(page)
		if err != nil {
			return err
		}
	}
	// No copy joins root.copies once they are taken above: the lent pages
	// went first. In a controlled run another task goes on here; the root
	// goes whole whatever it says.
	cancelled := sim.Admit(ctx, "vmmemory/drop-lent-root")
	h.mu.Lock()
	lent := root.lentPages
	root.lentPages = nil
	for _, page := range lent {
		if f := frameOf(page); f.lent == page {
			f.lent = nil
		}
	}
	published := root.published
	h.mu.Unlock()
	if !published && sim.Bug(ctx, "pager-destroy-a-lent-root-under-its-readers") {
		// The bug destroys a root the point did not publish, and with it any
		// page a child read under the name and maps.
		root.object.Destroy()
	} else {
		for _, page := range lent {
			if link, ok := h.node.PageQueues().Backlink(page); ok {
				lock := link.Cow.Lock()
				lock.Lock()
				link.Cow.RemovePageLocked(link.Offset, page)
				lock.Unlock()
			}
		}
	}
	if cancelled != nil {
		return cancelled
	}
	return context.Cause(ctx)
}

// unlendRoot takes every page naming a parent's frame out of a lent root,
// each under its lock, which waits for a child's fork copy of it under way.
// A copy takes its lent page out of root.lentPages itself, and one that took
// the page's place first leaves nothing here to remove.
func (h *Host) unlendRoot(ctx context.Context, root *identityRoot) error {
	h.mu.Lock()
	lent := slices.Clone(root.lentPages)
	h.mu.Unlock()
	for _, page := range lent {
		if err := h.lockPage(ctx, page); err != nil {
			return err
		}
		h.mu.Lock()
		if f := frameOf(page); f.lent == page {
			f.lent = nil
		}
		root.lentPages = slices.DeleteFunc(root.lentPages, func(p *zirconvm.VmPage) bool { return p == page })
		h.mu.Unlock()
		if link, ok := h.node.PageQueues().Backlink(page); ok {
			lock := link.Cow.Lock()
			lock.Lock()
			link.Cow.RemovePageLocked(link.Offset, page)
			lock.Unlock()
		}
		h.unlockPage(page)
	}
	return nil
}

// dropLentRootSeam runs in the end of a fork point's seal once a lent root's
// copies are taken and before they go. It is nil in production; a test
// faults a child on a lent page there.
var dropLentRootSeam func()

// dropSharers takes page away from every binding that maps it but keep: a
// page the guest takes back as dirty state, or that goes back to the arena,
// must be no other region's. A sharer whose mapping cannot be taken away is
// terminal from here and keeps the page, which it reports: the page then goes
// back once that sharer is closed (giveUp), and the sharer's end is its own,
// not the caller's. Caller holds the page's lock.
func (h *Host) dropSharers(ctx context.Context, page *zirconvm.VmPage, keep ...*binding) (held bool, err error) {
	var sharers []*binding
	h.mu.Lock()
	for b := range frameOf(page).aliases.all() {
		if !slices.Contains(keep, b) && b.page != nil {
			sharers = append(sharers, b)
		}
	}
	h.mu.Unlock()
	// Revoked in the order the sharers' regions attached, as every step that
	// commands several regions does (MemoryRegion.serial).
	slices.SortFunc(sharers, func(a, b *binding) int {
		return cmp.Or(cmp.Compare(a.region.serial, b.region.serial), cmp.Compare(a.index, b.index))
	})
	// The sharers stay the page's until each is taken off below: a binding
	// gains or changes its page only with that page's lock held, which the
	// caller holds. The one change without it is a sharer's own detach, which
	// takes its alias off itself, and the look below sees that; its mapping
	// is then a stopped process's, which may fail the revocation as it fails
	// any. In a controlled run another task goes on here.
	if len(sharers) > 0 {
		if err := sim.Admit(ctx, "vmmemory/drop-sharers"); err != nil {
			return false, err
		}
	}
	for _, b := range sharers {
		if err := b.region.revoke(ctx, b); err != nil {
			// Before 2026-10-08 the sharer's end was the caller's: a fork
			// point's child whose client refused the revocation failed its
			// parent's unseal, retire or detach.
			if b.region.terminal.Load() == nil || sim.Bug(ctx, "pager-end-a-step-with-its-sharer") {
				return held, err
			}
			b.region.heldPages(ctx, err)
			held = true
			continue
		}
		h.mu.Lock()
		if b.page != nil {
			h.unaliasLocked(b)
		}
		h.mu.Unlock()
	}
	return held, nil
}

// published reports a page of an identity root, still resident: what a copy
// may remember as its origin, and what a settle or a give-back may put the
// guest back on.
func (h *Host) published(page *zirconvm.VmPage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := frameOf(page)
	return f.layer == nil && f.slot >= 0 && !isLent(page)
}

// endForkFileSeam runs in the end of a seal between taking its fork file's
// holders and dropping the file from their processes. It is nil in
// production; a test gives a holder another point's file there.
var endForkFileSeam func()

// dropFork takes a fork point's file back from one holder's process, and
// frees the number the file was given under there only once the drop has
// landed. The holder's filesMu keeps every give of another file to that
// process out meanwhile (giveFork), so none is given under a number about to
// be dropped, nor under one still in use. A holder that has detached, or
// cannot take a command, is dropped from nothing: whether it has detached is
// asked of the host's regions under h.mu, not of its closed mark, which its
// own lock guards.
func (h *Host) dropFork(ctx context.Context, q *MemoryRegion, f *arenaFile, number int, early bool) error {
	if !early {
		if err := lockAdmitted(ctx, "vmmemory/files", q.filesMu.TryLock, q.filesMu.WaitFree); err != nil {
			// The file stays given under its number until q detaches.
			return err
		}
		defer q.filesMu.Unlock()
	}
	h.mu.Lock()
	_, attached := h.memoryRegions[q]
	h.mu.Unlock()
	if attached && q.terminal.Load() == nil {
		if err := q.mapping.DropFile(ctx, number); err != nil {
			q.fail(err)
		}
	}
	h.mu.Lock()
	if given, ok := q.forks[f]; ok && given == number {
		delete(q.forks, f)
	}
	h.mu.Unlock()
	return nil
}

// endForkFile is the part of endFork an isolated arena's fork file needs:
// each child closes the file, and the file goes back to the arena once its
// copies, which went with the point's temporary root, are gone.
func (r *MemoryRegion) endForkFile(ctx context.Context, c *MemoryRegionCheckpoint) error {
	h := r.host
	c.mu.Lock()
	f := c.fork
	c.fork = nil
	c.mu.Unlock()
	if f == nil {
		return nil
	}
	early := sim.Bug(ctx, "pager-free-a-fork-number-before-its-drop")
	h.mu.Lock()
	holders := f.holders
	f.holders = nil
	if early {
		// The bug frees each holder's number for the file before its drop
		// lands, so a give of another point's file meanwhile takes it.
		for q := range holders {
			delete(q.forks, f)
		}
	}
	h.mu.Unlock()
	// No region is given the file from here: a region is given it as it maps
	// a copy in it (giveFork), holding the copy's lock, and the copies have
	// gone with the point's roots, whose drop waited for each copy's lock. A
	// holder may be given another point's file meanwhile, which takes the
	// next number free in its process, so each holder's number for this file
	// stays taken until its drop has landed (dropFork). In a controlled run
	// another task goes on here; the file goes back whatever it says.
	if endForkFileSeam != nil {
		endForkFileSeam()
	}
	cancelled := sim.Admit(ctx, "vmmemory/end-fork-file")
	for _, q := range inAttachOrder(holders) {
		cancelled = errors.Join(cancelled, r.host.dropFork(ctx, q, f, holders[q], early))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.slots.Held() == 0 {
		h.dropFileLocked(f)
	} else {
		// A copy a revocation could not take away belongs to a terminal
		// region, and the file goes back once that region is closed.
		f.orphaned = true
	}
	return cancelled
}
