package vmmemory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/control"
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
	copies []*zbinding
	done   chan struct{}
	err    error // read only after done is closed
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
	z := r.zircon

	return z.seal(ctx)
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
	return c.zircon().dirtyPages(c)
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
	return c.zircon().share(ctx, c, ref, volume)
}

// ReadDirty fills dst, exactly one pager page, with the bytes the seal froze. A
// store the guest made since then copied that page away from the checkpoint, so
// it is not in them. It takes an I/O permit and the page's lock, never the
// memory region's: the guest goes on faulting and storing while a checkpoint reads its
// checkpoint.
func (c *MemoryRegionCheckpoint) ReadDirty(ctx context.Context, page uint64, dst []byte) error {
	return c.zircon().readDirty(ctx, c, page, dst)
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
	z := r.zircon

	return z.endSeal(ctx, checkpoint, published)
}
