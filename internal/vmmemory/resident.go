package vmmemory

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/platform/sim"
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
	// kind is what the region that created this page maps it as, RAM or PMEM.
	// A page identity names a volume, so every region that ever maps this page
	// agrees; it is kept on the page rather than read off an alias because a
	// page can outlive every mapping of it — the page a store copied away from
	// is host memory whether anything maps it or not.
	kind RegionKind
	// aliases is protected by Host.mu, not by this page's lock: a seal joins
	// the checkpoint's copy to a page a reclaim is already holding, and the
	// reclaim finds it there.
	aliases map[*binding]struct{}
	recent  *list.Element
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
// under Region.mu. Eviction publishes backing before making it nonresident.
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
	_, err := b.region.loadBacking(ctx, b.index*h.pageSize, dst)
	return err
}

// aliases reports the bindings one resident page is reachable from. The set can
// grow while that page's lock is held, which is what joinReclaiming does, so a
// reclaim reads it again rather than once.
func (h *Host) aliases(pg *resident) []*binding {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]*binding, 0, len(pg.aliases))
	for b := range pg.aliases {
		result = append(result, b)
	}
	return result
}

func (h *Host) bind(b *binding, pg *resident) {
	h.mu.Lock()
	pg.aliases[b] = struct{}{}
	b.resident = pg
	h.mu.Unlock()
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
		pg.aliases[held] = struct{}{}
		held.resident = pg
	}
}

func (h *Host) touch(pg *resident) {
	h.mu.Lock()
	h.lru.MoveToBack(pg.recent)
	h.mu.Unlock()
}

// create fills an already reserved slot and returns its locked page, not yet
// visible in the sharing index.
func (h *Host) create(ctx context.Context, slot int, data []byte, key pageKey, private bool, kind RegionKind) (*resident, error) {
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
func (h *Host) createZeros(ctx context.Context, slot, count int, kind RegionKind) ([]*resident, error) {
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
	pages := make([]*resident, count)
	for i := range pages {
		pages[i] = h.adopt(slot+i, pageKey{}, true, kind)
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
func (h *Host) adopt(slot int, key pageKey, private bool, kind RegionKind) *resident {
	pg := &resident{mu: ctxsync.NewMutex(), slot: slot, key: key, private: private, kind: kind, aliases: make(map[*binding]struct{})}
	_ = pg.mu.Lock(context.Background())
	h.mu.Lock()
	pg.recent = h.lru.PushBack(pg)
	h.signal()
	h.mu.Unlock()
	return pg
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
	h.putFree(pg.slot)
	pg.slot = -1
	h.lru.Remove(pg.recent)
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
// and any other region that inherits that identity maps it instead of reading
// it. Nothing is pinned by this: the page is clean, so the next reclaim short of
// a slot takes it like any other. Caller holds the page's lock.
func (h *Host) leave(b *binding, pg *resident) {
	h.mu.Lock()
	delete(pg.aliases, b)
	b.resident = nil
	h.signal()
	h.mu.Unlock()
}

// releaseOrigin gives up a page a copy was made from once nothing maps it and
// nothing names it any more, which is what detaching a region does with the
// pages its stores left behind: a page no binding reaches is one nothing else
// would ever release. A page something still maps, or one already evicted, is
// left alone.
func (h *Host) releaseOrigin(ctx context.Context, pg *resident) error {
	if err := pg.mu.Lock(ctx); err != nil {
		return err
	}
	defer h.unlock(pg)
	h.mu.Lock()
	keep := pg.slot < 0 || len(pg.aliases) > 0
	h.mu.Unlock()
	if keep {
		return nil
	}
	return h.release(ctx, pg)
}

func (h *Host) unlink(ctx context.Context, b *binding, pg *resident) error {
	h.mu.Lock()
	last := len(pg.aliases) == 1
	h.mu.Unlock()
	if last {
		if err := h.release(ctx, pg); err != nil {
			return err
		}
	}
	h.mu.Lock()
	delete(pg.aliases, b)
	b.resident = nil
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
// metadata, not a page transition, so it runs with neither the region nor any
// page held; the batch's pages are in ascending order, so one window's extents
// answer for the run of pages that falls in it.
func (r *Region) storedIdentities(ctx context.Context, held []*binding) (map[uint64]storedPage, error) {
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
func (r *Region) publishLocked(ctx context.Context, b *binding, pg *resident, id pageKey, stored bool) error {
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
	if err := h.revoke(ctx, b); err != nil {
		return err
	}
	return h.unlink(ctx, b, pg)
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
	for b := range pg.aliases {
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
