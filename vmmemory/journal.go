package vmmemory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A durable flush writes a disk's changed blocks to its host's journal
// (plans/fsync-journal-2026-10-06.md). Guest stores to a disk are CPU stores
// into mapped memory, which nothing logs, so the pager is what knows which
// pages changed since the last flush. One rule keeps that right:
//
//	every page the guest can store into without a fault is unjournaled.
//
// So every path that gives the guest a writable page marks it unjournaled
// (noteStoredLocked). A capture takes the unjournaled pages, write-protects the
// ones mapped writable, and reads them. The guest's next store to such a page
// takes a protect trap, which maps it writable again and marks it, copying
// nothing (unprotectForStore).
//
// A capture writes only the blocks whose bytes changed. The region keeps a
// SHA-256 digest of each block of each page it has captured: 16 KiB for a
// 2 MiB page. The digests describe what the journal holds for the page, so a
// block whose digest is unchanged is one a replay already restores. A page
// with no entry since it became the region's own takes its digests from what
// a replay starts from: its origin, a page of the volume it was copied from,
// or zeros where it was made from zeros. A page whose digests were dropped,
// or whose origin is gone, is written whole.
//
// A seal moves the unjournaled pages to its checkpoint, as its unjournaled
// list, and drops their digests. A capture while the seal stands takes the
// list too, reading the sealed copy where the guest still shares it; a page
// the guest has stored into since leaves the list. A published checkpoint
// drops the list: it holds those bytes. An abandoned one gives every page
// still on it back as unjournaled, with no digests. Every other page keeps its
// digests across a seal, which spec/journal's MCCapture checks.
//
// A capture whose journal write or sync failed gives its pages back as
// unjournaled with no digests (Captured.Fail): its entry may not be on the
// disk (spec/bugs.md, B6).
//
// Only a persistent disk is journaled. RAM is durable only through a capture
// of the whole VM, and an ephemeral disk is never durable.

// BlockBytes is the unit a capture compares and writes: a page's block.
const BlockBytes = 4096

// ErrNotJournaled reports a capture of a region the journal holds nothing of:
// RAM, or an ephemeral disk.
var ErrNotJournaled = errors.New("vmmemory: the journal holds only persistent disks")

// blockDigest is what one block's bytes hashed to.
type blockDigest [sha256.Size]byte

// regionJournal is a region's durable flush state. unjournaled is every page
// whose bytes may differ from the region's last journal entry for it, other
// than the pages a standing seal's list holds. digests is, for each page the
// region has captured, the digest of each of its blocks as the journal holds
// them. A page whose digests were dropped holds an empty list: an entry a
// replay applies may hold any bytes of it, so its next capture writes it
// whole. A page with no list at all has had no entry since it became the
// region's own, and takes its digests from what it was made from. Guarded by
// the region's bindingsMu.
type regionJournal struct {
	unjournaled pageRuns
	digests     map[uint64][]blockDigest
}

// dropDigestsLocked marks page's digests unknown: its next capture writes it
// whole. Caller holds r.bindingsMu.
func (r *MemoryRegion) dropDigestsLocked(page uint64) {
	if r.journal.digests == nil {
		r.journal.digests = make(map[uint64][]blockDigest)
	}
	r.journal.digests[page] = []blockDigest{}
}

// blocksPerPage is how many blocks one page of this region holds.
func (r *MemoryRegion) blocksPerPage() int { return int(r.host.pageSize / BlockBytes) }

// BlocksPerPage is how many blocks one page of this region holds, which bounds
// what a capture of a number of its pages takes.
func (r *MemoryRegion) BlocksPerPage() int { return r.blocksPerPage() }

// noteStoredLocked records that the guest may store into b's page without a
// fault from here: a capture's protection, if it had one, is gone, and the
// page is unjournaled. Caller holds r.bindingsMu.
func (r *MemoryRegion) noteStoredLocked(b *binding) {
	b.protected = false
	r.journal.unjournaled.add(b.index, b.index+1)
}

// forgetJournaledLocked takes page out of the journal's state: its bytes are
// ones no entry has to bring back. Caller holds r.bindingsMu.
func (r *MemoryRegion) forgetJournaledLocked(page uint64) {
	r.journal.unjournaled.remove(page)
	delete(r.journal.digests, page)
}

// takeUnjournaled hands the region's unjournaled pages to a seal, which keeps
// them as its list, and drops their digests. Caller holds the region
// exclusively.
func (r *MemoryRegion) takeUnjournaled(ctx context.Context) pageRuns {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	list := r.journal.unjournaled
	r.journal.unjournaled = newPageRuns(r.host.pageSize)
	if sim.Bug(ctx, "journal-seal-drops-unjournaled") {
		// The seal forgets the pages no entry holds yet, so no capture takes
		// what the guest stored into them before it.
		list = newPageRuns(r.host.pageSize)
	}
	if !sim.Bug(ctx, "journal-digests-survive-unjournaled-seal") {
		for _, run := range list.runs(uint64(r.pageCount)) {
			for page := run.Page; page < run.Page+uint64(run.Count); page++ {
				r.dropDigestsLocked(page)
			}
		}
	}
	return list
}

// endUnjournaled ends a seal's list. A published checkpoint holds those
// pages' bytes, and every entry a capture made of them since the seal holds
// those same bytes, so the list goes, and so do their digests: the next copy
// of such a page takes them from the published page it is copied from. An
// abandoned one gives every page still on it back to the region as
// unjournaled, with no digests. Caller holds the region exclusively.
func (r *MemoryRegion) endUnjournaled(c *MemoryRegionCheckpoint, published bool) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	list := c.unjournaled
	c.unjournaled = newPageRuns(r.host.pageSize)
	if list.list == nil {
		return
	}
	if published {
		for _, page := range runPages(list, r.pageCount) {
			if b, _ := r.lookupLocked(page); b == nil || !b.dirty {
				delete(r.journal.digests, page)
			}
		}
		return
	}
	for _, run := range list.runs(uint64(r.pageCount)) {
		r.journal.unjournaled.add(run.Page, run.Page+uint64(run.Count))
		for page := run.Page; page < run.Page+uint64(run.Count); page++ {
			r.dropDigestsLocked(page)
		}
	}
}

// journalProtected reports whether a capture write-protected the region's
// page at index.
func (r *MemoryRegion) journalProtected(index uint64) bool {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	b, _ := r.lookupLocked(index)
	return b != nil && b.protected
}

// unprotectForStore serves a protect trap on a page a capture
// write-protected: the page is the region's own, so it is made writable where
// it is, and unjournaled. A page spilled since is reloaded writable. It
// reports a page that stopped being protected meanwhile, whose fault is
// decided again from the top.
func (r *MemoryRegion) unprotectForStore(ctx context.Context, index uint64) (retry bool, err error) {
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(index)
	if b == nil || !b.protected {
		r.bindingsMu.Unlock()
		return true, nil
	}
	if sim.Bug(ctx, "journal-trap-not-marked") {
		// The page becomes writable without becoming unjournaled, so the
		// next flush does not take what the guest stores into it.
		b.protected = false
	} else {
		r.noteStoredLocked(b)
	}
	r.noteSealableLocked(b)
	r.bindingsMu.Unlock()
	page, err := r.host.lockedPage(ctx, b)
	if err != nil {
		return false, err
	}
	if page == nil {
		resolved, err := r.refault(ctx, b)
		return !resolved && err == nil, err
	}
	defer r.host.unlockPage(page)
	// The page may not be mapped at all: a harvest or a failed capture took
	// its mapping away and kept it. Either way it is mapped from here, and an
	// eviction has to revoke it, which the mapper records.
	if err := r.mapRun(ctx, r.runAt(index, frameOf(page).fileSlot, 1), true); err != nil {
		return false, r.refusedWritable(ctx, err, index, index+1)
	}
	if err := r.resolvePages(ctx, index, 1, true); err != nil {
		return false, r.fail(err)
	}
	return false, nil
}

// Unjournaled reports the pages a flush now has to cover, in ascending order:
// every page whose bytes may differ from the region's last journal entry for
// it. A capture of them, Capture, covers every store the guest made before
// this call.
func (r *MemoryRegion) Unjournaled() []uint64 {
	c := r.currentCheckpoint()
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	pages := runPages(r.journal.unjournaled, r.pageCount)
	if c != nil && c.unjournaled.list != nil {
		pages = append(pages, runPages(c.unjournaled, r.pageCount)...)
		slices.Sort(pages)
		pages = slices.Compact(pages)
	}
	return pages
}

// runPages is every page of runs within [0, count), in order.
func runPages(runs pageRuns, count int) []uint64 {
	var pages []uint64
	for _, run := range runs.runs(uint64(count)) {
		for page := run.Page; page < run.Page+uint64(run.Count); page++ {
			pages = append(pages, page)
		}
	}
	return pages
}

// Captured is one capture of a region's changed blocks: what one journal
// entry, or several, holds.
type Captured struct {
	region *MemoryRegion
	// pages is every page the capture took, whether it found a block of it
	// changed or not.
	pages []uint64
	// Blocks are the changed blocks' numbers, in ascending order: each
	// block's byte offset in the volume divided by BlockBytes.
	Blocks []uint64
	// Data holds the blocks, BlockBytes each, in the order Blocks names them.
	Data []byte
}

// Capture takes the changed blocks of the pages a flush has to cover. pages
// is what Unjournaled reported when the flush arrived: a page that is no
// longer unjournaled was captured since, and a page unjournaled since is not
// this flush's to cover. It write-protects the pages the guest maps writable,
// reads each page once, and keeps the blocks whose SHA-256 differs from the
// digest the region holds, hashing on Config.SettleWorkers workers.
//
// It holds the region exclusively throughout, as a seal's pause does, so no
// seal falls between taking the pages and reading them.
func (r *MemoryRegion) Capture(ctx context.Context, pages []uint64) (*Captured, error) {
	if r.kind != Pmem || r.Ephemeral() {
		return nil, ErrNotJournaled
	}
	if err := rlockAdmitted(ctx, "vmmemory/live", r.live); err != nil {
		return nil, err
	}
	defer r.live.RUnlock()
	if err := wlockAdmitted(ctx, "vmmemory/region", r.mu); err != nil {
		return nil, err
	}
	defer r.unlock()
	if err := r.ready(); err != nil {
		return nil, err
	}
	c := r.currentCheckpoint()
	taken, writable := r.takeForCapture(c, pages)
	if captureSeam != nil {
		captureSeam(taken)
	}
	if err := r.protectForCapture(ctx, taken, writable); err != nil {
		return nil, err
	}
	r.markCaptured(taken)
	reads, err := r.readCaptured(ctx, taken)
	if err != nil {
		// The pages are unjournaled again, as a failed batch's are: nothing
		// of them reached an entry.
		r.giveBackCaptured(ctx, taken)
		return nil, err
	}
	return r.captured(c, taken, reads), nil
}

// captureSeam runs in a capture between its taking the pages and its holding
// the region's protection, so a test can evict one of them there.
var captureSeam func(taken []uint64)

// takeForCapture is the pages a capture takes, in ascending order: those of
// asked that are unjournaled or on the standing seal's list. writable is the
// runs of them the guest may store into without a fault as they were taken;
// the capture protects those it finds so under the region's protection
// (writableRuns).
func (r *MemoryRegion) takeForCapture(c *MemoryRegionCheckpoint, asked []uint64) (taken []uint64, writable []PageRun) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	for _, page := range asked {
		if page >= uint64(r.pageCount) {
			continue
		}
		onList := c != nil && c.unjournaled.list != nil && c.unjournaled.has(page)
		if !r.journal.unjournaled.has(page) && !onList {
			continue
		}
		taken = append(taken, page)
	}
	return taken, r.writableRunsLocked(taken)
}

// writableRunsLocked is the runs of pages the guest may store into without a
// fault, in ascending order. Caller holds r.bindingsMu.
func (r *MemoryRegion) writableRunsLocked(pages []uint64) []PageRun {
	var writable []PageRun
	for _, page := range pages {
		if !r.dirtyRuns.has(page) {
			continue
		}
		if n := len(writable); n > 0 && writable[n-1].Page+uint64(writable[n-1].Count) == page {
			writable[n-1].Count++
		} else {
			writable = append(writable, PageRun{Page: page, Count: 1})
		}
	}
	return writable
}

// protectForCapture write-protects the runs a capture takes that the guest
// maps writable, under the region's protection as a seal's pause takes it. A
// failure takes away the mappings of the runs it protected, so the guest
// faults and maps them writable again, still unjournaled.
//
// The runs are read again under the protection, as a seal reads them
// (protectDirtyRuns): an eviction takes a page's mapping away under that
// page's lock and the protection shared alone, not the region, so one that
// ended after the capture took its pages left a page the guest no longer maps,
// and a write-protect of an unmapped page is refused. writable is the runs as
// the pages were taken.
//
// The protection is given back before a failure's mappings are taken away, as
// a seal gives it back (protectDirtyRuns): the revocation takes it shared, and
// it waited for itself while the capture held it, with the region held, so
// the guest's flush never ended and its machine never detached.
func (r *MemoryRegion) protectForCapture(ctx context.Context, taken []uint64, writable []PageRun) error {
	protected, err := r.protectCaptureRuns(ctx, taken, writable)
	if err != nil && sim.Bug(ctx, "journal-unprotect-under-the-protection") {
		// The bug takes the mappings away still holding the protection.
		if err := wlockAdmitted(ctx, "vmmemory/protection", r.protectMu); err != nil {
			return err
		}
		defer r.protectMu.Unlock()
	}
	if err != nil {
		return errors.Join(err, r.unprotect(context.WithoutCancel(ctx), protected))
	}
	return nil
}

// protectCaptureRuns is protectForCapture's write-protection, with the
// region's protection held exclusively, and the runs it protected.
func (r *MemoryRegion) protectCaptureRuns(ctx context.Context, taken []uint64, writable []PageRun) ([]PageRun, error) {
	if err := wlockAdmitted(ctx, "vmmemory/protection", r.protectMu); err != nil {
		return nil, err
	}
	defer r.protectMu.Unlock()
	runs := writable
	if !sim.Bug(ctx, "journal-protect-runs-read-before-the-protection") {
		r.bindingsMu.Lock()
		runs = r.writableRunsLocked(taken)
		r.bindingsMu.Unlock()
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return r.protect(ctx, runs)
}

// markCaptured records that a capture took pages: each is no longer
// unjournaled, and the region's own dirty pages among them are write-protected,
// so the guest's next store to one traps. A copy the capture took is the
// guest's state from here, not a cold copy to give back.
func (r *MemoryRegion) markCaptured(pages []uint64) {
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	for _, page := range pages {
		r.journal.unjournaled.remove(page)
		b, _ := r.lookupLocked(page)
		if b == nil || !b.dirty || b.checkpoint != nil {
			continue
		}
		b.protected = true
		r.uncoldLocked(b)
		r.noteSealableLocked(b)
	}
}

// giveBackCaptured makes pages unjournaled again with no digests: a capture
// of them that did not reach the journal.
func (r *MemoryRegion) giveBackCaptured(ctx context.Context, pages []uint64) {
	keep := sim.Bug(ctx, "journal-failed-keeps-digests")
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	for _, page := range pages {
		r.journal.unjournaled.add(page, page+1)
		if !keep {
			// The digests would describe an entry that may not be on the
			// disk, and the next capture would leave out the blocks it holds.
			r.dropDigestsLocked(page)
		}
	}
}

// pageRead is what a capture read of one page: the bytes, the digest of each
// of its blocks, the digests they are compared with, nil where the whole page
// is written, and whether the guest still shares the sealed copy.
type pageRead struct {
	page     uint64
	skip     bool
	held     bool
	data     []byte
	digests  []blockDigest
	previous []blockDigest
}

// readCaptured reads and hashes every page a capture took, divided between
// Config.SettleWorkers workers. A page whose bytes are its volume's again,
// clean in the region, is skipped: no entry has to bring it back.
func (r *MemoryRegion) readCaptured(ctx context.Context, pages []uint64) ([]pageRead, error) {
	h := r.host
	reads := make([]pageRead, len(pages))
	failures := make([]error, len(pages))
	workers := min(max(h.cfg.SettleWorkers, 1), len(pages))
	var next atomic.Int64
	var wait sync.WaitGroup
	for range workers {
		wait.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(pages) {
					return
				}
				// The workers go on beside each other, so each page's read is
				// a task of its own: what a controlled run admits it as is the
				// page's, never the order the workers reach it in.
				read := sim.WithTask(ctx, fmt.Sprintf("capture-read-%d", pages[i]))
				reads[i], failures[i] = r.readForCapture(read, pages[i])
			}
		})
	}
	wait.Wait()
	return reads, errors.Join(failures...)
}

// readForCapture reads one page a capture took, from the sealed copy where the
// guest still shares it and from the guest's own page or its spill otherwise,
// and hashes its blocks.
func (r *MemoryRegion) readForCapture(ctx context.Context, page uint64) (pageRead, error) {
	h := r.host
	read := pageRead{page: page}
	r.bindingsMu.Lock()
	b, _ := r.lookupLocked(page)
	var source *binding
	var origin *zirconvm.VmPage
	zeroed := false
	if b != nil && b.checkpoint != nil {
		source, read.held = b.checkpoint, true
	} else if b != nil && b.dirty {
		source, origin, zeroed = b, b.origin, b.zeroed
	}
	previous, known := r.journal.digests[page]
	r.bindingsMu.Unlock()
	if source == nil {
		read.skip = true
		return read, nil
	}
	release, err := h.beginCheckpointIO(ctx)
	if err != nil {
		return read, err
	}
	defer release()
	read.data = make([]byte, h.pageSize)
	if err := r.readHeld(ctx, source, read.data); err != nil {
		return read, err
	}
	read.digests = r.blockDigests(read.data)
	switch {
	case known:
		read.previous = previous
	case origin != nil:
		read.previous, err = r.originDigests(ctx, origin)
		if err != nil {
			return read, err
		}
	case zeroed:
		read.previous = r.zeroDigests()
	}
	return read, nil
}

// blockDigests is the digest of each block of one page's bytes.
func (r *MemoryRegion) blockDigests(data []byte) []blockDigest {
	digests := make([]blockDigest, r.blocksPerPage())
	for i := range digests {
		digests[i] = sha256.Sum256(data[i*BlockBytes : (i+1)*BlockBytes])
	}
	return digests
}

// zeroDigests is the digests of a page of zeros.
func (r *MemoryRegion) zeroDigests() []blockDigest {
	zero := sha256.Sum256(make([]byte, BlockBytes))
	digests := make([]blockDigest, r.blocksPerPage())
	for i := range digests {
		digests[i] = zero
	}
	return digests
}

// originDigests is the digests of the page a dirty page was copied from, nil
// where that page is no longer resident, or where something holds its lock:
// the capture then writes the whole page. The capture holds the region
// exclusively, and the origin is a root's page, which a fault of the region
// holds in its plan across its read and until it has taken the region back;
// waiting for the lock here, each waited for the other.
func (r *MemoryRegion) originDigests(ctx context.Context, origin *zirconvm.VmPage) ([]blockDigest, error) {
	if sim.Bug(ctx, "journal-wait-for-an-origin-under-the-region") {
		if err := r.host.lockPage(ctx, origin); err != nil {
			return nil, err
		}
	} else if !frameOf(origin).mu.TryLock() {
		return nil, nil
	}
	defer r.host.unlockPage(origin)
	if !r.host.published(origin) {
		// Evicted, or no longer its identity's page.
		return nil, nil
	}
	f := frameOf(origin)
	data := make([]byte, r.host.pageSize)
	if err := f.file.Read(ctx, f.slot, data); err != nil {
		return nil, err
	}
	return r.blockDigests(data), nil
}

// captured records what a capture read: each page's digests become what the
// capture took, a page the guest stored into since the seal leaves the seal's
// list, and the changed blocks become the capture's.
func (r *MemoryRegion) captured(c *MemoryRegionCheckpoint, pages []uint64, reads []pageRead) *Captured {
	result := &Captured{region: r, pages: pages}
	perPage := uint64(r.blocksPerPage())
	r.bindingsMu.Lock()
	defer r.bindingsMu.Unlock()
	if r.journal.digests == nil {
		r.journal.digests = make(map[uint64][]blockDigest)
	}
	for _, read := range reads {
		if !read.held && c != nil && c.unjournaled.list != nil {
			c.unjournaled.remove(read.page)
		}
		if read.skip {
			continue
		}
		for i, sum := range read.digests {
			if len(read.previous) > 0 && read.previous[i] == sum {
				continue
			}
			result.Blocks = append(result.Blocks, read.page*perPage+uint64(i))
			result.Data = append(result.Data, read.data[i*BlockBytes:(i+1)*BlockBytes]...)
		}
		r.journal.digests[read.page] = read.digests
	}
	return result
}

// Pages is every page the capture took, in ascending order, whether it found
// a block of it changed or not.
func (c *Captured) Pages() []uint64 { return c.pages }

// Fail gives the capture's pages back as unjournaled, with no digests: the
// journal write or sync that was to hold its blocks failed, so its entry may
// not be on the disk.
func (c *Captured) Fail(ctx context.Context) { c.region.giveBackCaptured(ctx, c.pages) }
