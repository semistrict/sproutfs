package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform/sim"
)

// pageKey identifies immutable bytes by the store page object that holds them.
// A pager page is exactly one store page, so one identity covers a whole pageKey.
type pageKey struct {
	id control.Identity
}

func (f pageKey) zero() bool { return f.id.Zero }

type resident struct {
	mu      *ctxsync.Mutex
	slot    int
	key     pageKey
	private bool
	// kind is what the memory region that created this page maps it as, RAM or PMEM.
	// A page identity names a volume, so every memory region that ever maps this page
	// agrees; it is kept on the page rather than read off an alias because a
	// page can outlive every mapping of it — the page a store copied away from
	// is host memory whether anything maps it or not.
	kind MemoryRegionKind
	// aliases is protected by Host.mu, not by this page's lock: a seal joins
	// the checkpoint's copy to a page a reclaim is already holding, and the
	// reclaim finds it there.
	aliases aliasSet
	// recent is this page's place on Host.lru, and idle its place on Host.idle
	// while no memory region maps it. Protected by Host.mu.
	recent, idle pageLinks
	// replacing counts the stores that have taken a binding off this page and
	// whose mapping command has not yet replaced the guest's mapping of it. The
	// guest goes on reading this page's offset until that command lands, so no
	// reclaim may take the page and its memory may not go back; dropped marks one
	// whose last binding went while it was held, whose memory therefore goes back
	// when the last of those stores lands rather than at the unlink. Both are
	// protected by Host.mu. See replacement.
	replacing int
	dropped   bool
}

// published reports a resident page holding a page identity some checkpoint
// gave it, whose bytes therefore cannot change while it holds that name. It is
// what a copy may remember as its origin: a private page is not one, whatever
// name a fork point lent it, and neither is a page with no identity at all.
// Caller holds the page's lock, or creates it.
func (pg *resident) published() bool {
	return pg != nil && !pg.private && pg.key != (pageKey{}) && !pg.key.zero()
}

// Caller holds the resident lock, or owns a currently nonresident binding
// under MemoryRegion.mu. Eviction publishes backing before making it nonresident.
func (h *Host) read(ctx context.Context, b *binding, pg *resident, dst []byte) error {
	if pg != nil {
		return h.arena.Read(ctx, pg.slot, dst)
	}
	if b.zero {
		clear(dst)
		return nil
	}
	if b.dirty {
		if b.checkpoint != nil {
			// The checkpoint's detached copy owns this page's only current bytes; the
			// two share a resident page, so a nonresident binding means both are.
			return h.read(ctx, b.checkpoint, nil, dst)
		}
		sum, held := h.spillDigest(b.spillSlot)
		if !held {
			return errors.New("private page has no current backing")
		}
		n, err := h.spill.ReadAt(ctx, dst, int64(b.spillSlot)*int64(h.pageSize))
		if err == nil && n != len(dst) {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		// The spill file is the only copy of this page, and a local device that
		// loses or garbles a sector of it would otherwise hand the guest memory
		// it never wrote. The page is refused instead.
		if crc32.Checksum(dst, spillChecksums) != sum {
			return ErrSpillCorrupt
		}
		return nil
	}
	_, err := b.memoryRegion.loadBacking(ctx, b.index*h.pageSize, dst)
	return err
}

// aliases reports the bindings one resident page is reachable from. The set can
// grow while that page's lock is held, which is what joinReclaiming does, so a
// reclaim reads it again rather than once.
func (h *Host) aliases(pg *resident) []*binding {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]*binding, 0, pg.aliases.len())
	for b := range pg.aliases.all() {
		result = append(result, b)
	}
	return result
}

func (h *Host) bind(b *binding, pg *resident) {
	h.mu.Lock()
	found := h.probe.bind(h, b, pg)
	h.mappedLocked(pg)
	pg.aliases.add(b)
	b.resident = pg
	h.mu.Unlock()
	what := "bind-shared"
	if pg.private {
		what = "bind-private"
	}
	note(b.memoryRegion, b.index, what, pg.slot, -1)
	if found != "" {
		panic(found)
	}
}

// bindRun is bind for pages[k] to bindings[k], under one host lock.
func (h *Host) bindRun(bindings []*binding, pages []*resident) {
	found := ""
	h.mu.Lock()
	for k, b := range bindings {
		if f := h.probe.bind(h, b, pages[k]); f != "" && found == "" {
			found = f
		}
		h.mappedLocked(pages[k])
		pages[k].aliases.add(b)
		b.resident = pages[k]
	}
	h.mu.Unlock()
	for k, b := range bindings {
		note(b.memoryRegion, b.index, "bind-private", pages[k].slot, -1)
	}
	if found != "" {
		panic(found)
	}
}

// joinReclaiming makes a binding an alias of the resident page another binding
// holds, without that page's lock. It is what a seal uses for a page a reclaim
// is holding: the reclaim writes that page's bytes to the reservation the seal
// has just handed to the checkpoint's copy, so the copy has to be in the alias set
// the reclaim reads, or already have been when it read it.
func (h *Host) joinReclaiming(held, b *binding) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pg := b.resident; pg != nil {
		h.mappedLocked(pg)
		pg.aliases.add(held)
		held.resident = pg
	}
}

func (h *Host) touch(pg *resident) {
	h.mu.Lock()
	h.lru.moveToBack(pg)
	h.mu.Unlock()
}

// create fills an already reserved slot and returns its locked page, not yet
// visible in the sharing index.
func (h *Host) create(ctx context.Context, slot int, data []byte, key pageKey, private bool, kind MemoryRegionKind) (*resident, error) {
	if sim.Bug(ctx, "pager-zero-new-page") {
		// The page is created without the bytes that were loaded or copied
		// into it, which every later read of that page then sees as zeroes.
		clear(data)
	}
	if err := h.arena.Write(ctx, slot, data); err != nil {
		return nil, h.abandonSlots(ctx, slot, 1, err)
	}
	return h.adopt(slot, key, private, kind), nil
}

// createZeros fills count consecutive reserved slots with zeros and returns
// their locked private pages. Every free slot is punched, so it already reads
// as zeros: an arena that can make such a slot mappable without writing it
// does so for the whole run at once, and only another arena is written.
func (h *Host) createZeros(ctx context.Context, slot, count int, kind MemoryRegionKind) ([]*resident, error) {
	var err error
	if zeroing, ok := h.arena.(ZeroArena); ok {
		err = zeroing.Zero(ctx, slot, count)
	} else {
		zeros := make([]byte, h.pageSize)
		for s := slot; s < slot+count && err == nil; s++ {
			err = h.arena.Write(ctx, s, zeros)
		}
	}
	if err != nil {
		return nil, h.abandonSlots(ctx, slot, count, err)
	}
	return h.adoptRun(slot, count, kind), nil
}

// createZeroRuns fills the slots of every run with zeros and returns their
// locked private pages in page order, which is the order the runs are in. A run
// that fails takes the runs after it and the pages before it with it, so a
// store that could not have its whole run leaves the arena exactly as it was.
func (h *Host) createZeroRuns(ctx context.Context, runs []MapRun, kind MemoryRegionKind) ([]*resident, error) {
	if len(runs) == 1 {
		return h.createZeros(ctx, runs[0].Slot, runs[0].Count, kind)
	}
	var pages []*resident
	for i, run := range runs {
		created, err := h.createZeros(ctx, run.Slot, run.Count, kind)
		if err == nil {
			pages = append(pages, created...)
			continue
		}
		for _, pg := range pages {
			err = errors.Join(err, h.release(ctx, pg))
			h.unlock(pg)
		}
		for _, rest := range runs[i+1:] {
			err = errors.Join(err, h.abandonSlots(ctx, rest.Slot, rest.Count, nil))
		}
		return nil, err
	}
	return pages, nil
}

// abandonSlots gives back reserved slots whose contents failed to arrive, or
// that their reserver turned out not to need. A failed write may have allocated
// partial contents, so each is punched before it is accounted free; a failed
// punch makes the host terminal.
func (h *Host) abandonSlots(ctx context.Context, slot, count int, err error) error {
	var cleanup error
	for s := slot; s < slot+count; s++ {
		cleanup = errors.Join(cleanup, h.arena.Release(context.WithoutCancel(ctx), s))
	}
	h.mu.Lock()
	if cleanup != nil {
		h.err = errors.Join(err, cleanup)
	} else {
		for s := slot; s < slot+count; s++ {
			h.putFree(s)
		}
	}
	h.signal()
	h.mu.Unlock()
	return errors.Join(err, cleanup)
}

// adopt makes a filled slot a locked resident page, most recently used.
func (h *Host) adopt(slot int, key pageKey, private bool, kind MemoryRegionKind) *resident {
	pg := &resident{mu: ctxsync.NewMutex(), slot: slot, key: key, private: private, kind: kind}
	_ = pg.mu.Lock(context.Background())
	h.mu.Lock()
	h.lru.pushBack(pg)
	h.signal()
	h.mu.Unlock()
	return pg
}

// adoptRun is adopt for the private zero pages of count consecutive slots, in
// slot order. A write-ahead run is thousands of pages that every other fault
// of the host is waiting to see, so they join the recency list under one host
// lock and wake waiters once.
func (h *Host) adoptRun(slot, count int, kind MemoryRegionKind) []*resident {
	pages := make([]*resident, count)
	for i := range pages {
		pg := &resident{mu: ctxsync.NewMutex(), slot: slot + i, private: true, kind: kind}
		_ = pg.mu.Lock(context.Background())
		pages[i] = pg
	}
	h.mu.Lock()
	for _, pg := range pages {
		h.lru.pushBack(pg)
	}
	h.signal()
	h.mu.Unlock()
	return pages
}

func (h *Host) release(ctx context.Context, pg *resident) error {
	if err := h.arena.Release(ctx, pg.slot); err != nil {
		h.mu.Lock()
		h.err = fmt.Errorf("managed arena terminal: %w", err)
		h.signal()
		result := h.err
		h.mu.Unlock()
		return result
	}
	h.mu.Lock()
	note(nil, 0, "release", pg.slot, -1)
	h.putFree(pg.slot)
	pg.slot = -1
	h.lru.remove(pg)
	h.mappedLocked(pg)
	if pg.key != (pageKey{}) && h.clean[pg.key] == pg {
		delete(h.clean, pg.key)
		h.cleanVersion++
	}
	h.signal()
	h.mu.Unlock()
	return nil
}

// leave takes a binding's alias off the page it copied away from and leaves
// that page in the arena, where unlink would release it once its last alias
// went. The bytes stay under the identity they are published by, so the settle
// has something to compare this copy against and something to re-share it onto,
// and any other memory region that inherits that identity maps it instead of reading
// it. Nothing is pinned by this: the page is clean, so the next reclaim short of
// a slot takes it like any other. Caller holds the page's lock.
func (h *Host) leave(b *binding, pg *resident) {
	h.mu.Lock()
	pg.aliases.remove(b)
	b.resident = nil
	h.idleLocked(pg)
	h.signal()
	h.mu.Unlock()
}

// idleLocked puts a page no memory region maps any more on the idle list, newest
// last, where it waits for a memory region that inherits its identity or for an
// allocation that needs its slot. Caller holds h.mu.
func (h *Host) idleLocked(pg *resident) {
	if pg.aliases.len() == 0 && pg.slot >= 0 {
		h.idle.pushBack(pg)
	}
}

// mappedLocked takes a page off the idle list, which a memory region mapping it again
// or its memory going back does. Caller holds h.mu.
func (h *Host) mappedLocked(pg *resident) {
	h.idle.remove(pg)
}

// releaseOrigin gives up a page a copy was made from once nothing maps it and
// nothing names it any more, which is what detaching a memory region does with the
// pages its stores left behind: a page no binding reaches is one nothing else
// would ever release. A page something still maps, or one already evicted, is
// left alone.
func (h *Host) releaseOrigin(ctx context.Context, pg *resident) error {
	if err := pg.mu.Lock(ctx); err != nil {
		return err
	}
	defer h.unlock(pg)
	h.mu.Lock()
	keep := pg.slot < 0 || pg.aliases.len() > 0
	if !keep && pg.replacing > 0 {
		// A store of another memory region is replacing the guest's mapping of this
		// page. Its memory goes back when that command lands, exactly as it
		// would for the binding that store took away.
		pg.dropped, keep = true, true
	}
	h.mu.Unlock()
	if keep {
		return nil
	}
	return h.release(ctx, pg)
}

func (h *Host) unlink(ctx context.Context, b *binding, pg *resident) error {
	note(b.memoryRegion, b.index, "unlink from "+caller(), pg.slot, -1)
	h.mu.Lock()
	last := pg.aliases.len() == 1
	// A page a store is replacing keeps its memory until that store's mapping
	// command lands: the guest is still reading this offset. The release is not
	// re-decided there, only deferred.
	if last && pg.replacing > 0 {
		pg.dropped, last = true, false
	}
	// A published page is still the page its identity names when the last
	// memory region mapping it goes: it stays, idle, for the next memory region that
	// inherits that identity, and an allocation short of a slot gives it up
	// before anything mapped. A private page is one memory region's state and nobody
	// else's, so it goes with that memory region.
	if last && pg.published() && h.clean[pg.key] == pg {
		last = false
	}
	h.mu.Unlock()
	if last {
		if err := h.release(ctx, pg); err != nil {
			return err
		}
	}
	h.mu.Lock()
	pg.aliases.remove(b)
	b.resident = nil
	h.idleLocked(pg)
	h.mu.Unlock()
	return nil
}

// storedPage is the identity the volume now gives one page, which is what
// decides whether its resident page can be shared.
type storedPage struct {
	id     pageKey
	stored bool
}

// storedIdentities reports that identity for every page of one retire batch,
// located once per read-ahead window rather than once per page. It is volume
// metadata, not a page transition, so it runs with neither the memory region nor any
// page held; the batch's pages are in ascending order, so one window's extents
// answer for the run of pages that falls in it.
func (r *MemoryRegion) storedIdentities(ctx context.Context, held []*binding) (map[uint64]storedPage, error) {
	result := make(map[uint64]storedPage, len(held))
	var window *windowPlan
	for _, checkpoint := range held {
		if checkpoint.spillSlot < 0 {
			continue
		}
		index := checkpoint.index
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

// publishLocked is publishClean with the page already locked, which is
// what retiring a page of a checkpoint needs: it must not be reachable
// from an unreserved binding for even a moment.
func (r *MemoryRegion) publishLocked(ctx context.Context, b *binding, pg *resident, id pageKey, stored bool) error {
	h := r.host
	drop := !stored || id.zero()
	if !drop {
		key := id
		h.mu.Lock()
		existing := h.clean[key]
		if existing == nil {
			pg.private, pg.key = false, key
			h.clean[key] = pg
			h.cleanVersion++
		}
		h.mu.Unlock()
		drop = existing != nil && existing != pg
	}
	if !drop {
		return nil
	}
	note(r, b.index, "publish-dropped "+publishReason(stored, id, h, pg), pg.slot, -1)
	// A page given up because the volume holds no object for it was checked and
	// its mapping taken away together with every other page this retire batch
	// hands back; see MemoryRegion.revokeHandedBack. What is left here is a page whose
	// identity another resident already holds, which is the one hand-back that
	// arrives alone — and one that arrives with its mapping already gone, which
	// this skips.
	if err := h.revoke(ctx, b); err != nil {
		return err
	}
	return h.unlink(ctx, b, pg)
}

// ErrUndroppable reports a retire that would have given up the only copy of
// bytes a guest wrote. It fails the retire rather than the VM: the checkpoint is
// durable either way, the page stays sealed and the guest keeps its memory, and
// the pager reports why its pages are still sealed.
var ErrUndroppable = errors.New("a page the volume holds no object for is not zeros")

// droppable refuses the one thing a retire may not get wrong. A page given up
// here because the volume holds no object for it is a page the volume must be
// able to reproduce without one, and the only such page is zeros: a publication
// writes an all-zero page as a sparse hole and the volume reads it back as
// zeros. A page with anything else in it holds bytes that exist nowhere but
// here, so dropping it would hand the guest an older version of memory it wrote.
//
// It reads the page, which is why it runs only where a page is about to be
// dropped rather than on every retire: that is a handful of pages per
// checkpoint, against every page the checkpoint holds.
func (h *Host) droppable(ctx context.Context, b *binding, pg *resident, stored bool, id pageKey) error {
	if (stored && !id.zero()) || pg == nil || pg.slot < 0 {
		return nil
	}
	data := make([]byte, h.pageSize)
	if err := h.arena.Read(ctx, pg.slot, data); err != nil {
		return err
	}
	if allZero(data) {
		return nil
	}
	return fmt.Errorf("%w: page %d of %s", ErrUndroppable, b.index, b.memoryRegion.kind)
}

// share names a private page in the sharing index without ending its privacy:
// the bytes are a seal's, immutable from the seal until it ends, and the guest
// they belong to keeps the reservation that spills them. It is what lets a
// machine that inherits the identity a fork point gives the page map that page
// instead of reading it. Caller holds the page's lock.
func (h *Host) share(pg *resident, key pageKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !pg.private || pg.key != (pageKey{}) || h.clean[key] != nil {
		return
	}
	pg.key = key
	h.clean[key] = pg
	h.cleanVersion++
}

// unshare takes a seal's name back off a page, which ending that seal does.
// A page the guest stores into again must be reachable by no identity: the
// bytes under that name stop being what the page holds. Caller holds the
// page's lock.
func (h *Host) unshare(pg *resident) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !pg.private || pg.key == (pageKey{}) {
		return
	}
	if h.clean[pg.key] == pg {
		delete(h.clean, pg.key)
		h.cleanVersion++
	}
	pg.key = pageKey{}
}

// dropSharers takes a resident page away from every binding but the ones named.
// Ending a seal that shared its pages does it for the pages the guest takes
// back:
// what the guest may store into in place must be its own, and a machine that
// gives one up reads the page through its own backing again — which by then
// holds those bytes, because a seal ends only once everything that inherited it
// has copied, published or pulled the pages. Caller holds the page's lock.
func (h *Host) dropSharers(ctx context.Context, pg *resident, keep ...*binding) error {
	var sharers []*binding
	h.mu.Lock()
	for b := range pg.aliases.all() {
		if !slices.Contains(keep, b) {
			sharers = append(sharers, b)
		}
	}
	h.mu.Unlock()
	for _, b := range sharers {
		if err := h.revoke(ctx, b); err != nil {
			return err
		}
		if err := h.unlink(ctx, b, pg); err != nil {
			return err
		}
	}
	return nil
}

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
