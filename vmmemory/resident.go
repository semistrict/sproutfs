package vmmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// pageKey identifies immutable bytes by the store page object that holds them.
// A pager page is exactly one store page, so one identity covers a whole pageKey.
type pageKey struct {
	id control.Identity
}

func (f pageKey) zero() bool { return f.id.Zero }

type resident struct {
	mu *ctxsync.Mutex
	// fileSlot is where the page is. Its slot is -1 once the page's memory
	// has gone back.
	fileSlot
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
	aliases aliasSet[*binding]
	// queue is this page's place in Host.queues: a reclaim or isolate queue,
	// ordered by when a fault last touched it; the don't-need queue while it
	// is idle; or the zero-fork queue while a cold copy will be compared with
	// it. Its backlink is its file and the byte offset of its slot. See
	// queues.go.
	queue zirconvm.PageQueueNode[*resident, *arenaFile]
	// queued marks a page in the queues, from its creation until its memory
	// goes back. idle marks a page no memory region maps, kept for the next one
	// that inherits its identity. Both are changed with Host.mu and Host.pinMu
	// held, and read with either.
	queued, idle bool
	// replacing counts the stores that have taken a binding off this page and
	// whose mapping command has not yet replaced the guest's mapping of it. The
	// guest goes on reading this page's offset until that command lands, so no
	// reclaim may take the page and its memory may not go back; dropped marks one
	// whose last binding went while it was held, whose memory therefore goes back
	// when the last of those stores lands rather than at the unlink. Both are
	// protected by Host.mu. See replacement.
	replacing int
	dropped   bool
	// coldCopies is every cold copy that will be compared with this page,
	// which keeps it in the zero-fork queue, where an eviction takes it only
	// when nothing else can go. It is guarded by Host.pinMu. See cold.go.
	coldCopies map[*binding]struct{}
}

// QueueNode is the page's place in the page queues.
func (pg *resident) QueueNode() *zirconvm.PageQueueNode[*resident, *arenaFile] { return &pg.queue }

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
		return pg.file.Read(ctx, pg.slot, dst)
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
		// The spill file is the only copy of this page, and a local device that
		// loses or garbles a sector of it would otherwise hand the guest memory
		// it never wrote. The storage checks it, and the page is refused.
		return h.readSpill(ctx, b.spill, dst)
	}
	_, err := b.memoryRegion.loadBacking(ctx, b.index*h.pageSize, dst)
	return err
}

func (h *Host) bind(b *binding, pg *resident) {
	h.mu.Lock()
	found := h.probe.bind(h, b, pg)
	h.mappedLocked(pg)
	aliasLocked(pg, b)
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

// abandonSlots gives back reserved slots whose contents failed to arrive, or
// that their reserver turned out not to need. A failed write may have allocated
// partial contents, so each is punched before it is accounted free; a failed
// punch makes the host terminal.
func (h *Host) abandonSlots(ctx context.Context, at fileSlot, count int, err error) error {
	var cleanup error
	for i := range count {
		cleanup = errors.Join(cleanup, at.file.Release(context.WithoutCancel(ctx), at.slot+i))
	}
	h.mu.Lock()
	if cleanup != nil {
		h.err = errors.Join(err, cleanup)
	} else {
		for i := range count {
			h.putFree(at.plus(i))
		}
	}
	h.signal()
	h.mu.Unlock()
	return errors.Join(err, cleanup)
}

func (h *Host) release(ctx context.Context, pg *resident) error {
	// Nothing the pager takes under pressure is pinned, so a page going here is
	// going for a reason of its own, and a cold copy of it has nothing left to
	// be compared with.
	h.dropCold(pg)
	if err := pg.file.Release(ctx, pg.slot); err != nil {
		h.mu.Lock()
		h.err = fmt.Errorf("managed arena terminal: %w", err)
		h.signal()
		result := h.err
		h.mu.Unlock()
		return result
	}
	h.mu.Lock()
	note(nil, 0, "release", pg.slot, -1)
	if pg.file.pages != nil {
		delete(pg.file.pages, pg.slot)
		delete(pg.file.digests, pg.slot)
	}
	h.putFree(pg.fileSlot)
	pg.slot = -1
	h.dequeueLocked(pg)
	if pg.key != (pageKey{}) && h.clean[pg.key] == pg {
		delete(h.clean, pg.key)
		h.cleanVersion++
	}
	h.signal()
	h.mu.Unlock()
	return nil
}

// aliasLocked makes b an alias of pg, and counts pg against b's memory region
// where no alias of that memory region counted it already. A page a memory
// region reaches twice, from its binding and from a checkpoint's copy of it, is
// one page of the arena. Caller holds h.mu.
func aliasLocked(pg *resident, b *binding) {
	counted := mappedBy(pg, b.memoryRegion)
	if pg.aliases.add(b) && !counted {
		b.memoryRegion.resident++
	}
}

// unaliasLocked is the reverse of aliasLocked. Caller holds h.mu.
func unaliasLocked(pg *resident, b *binding) {
	if pg.aliases.remove(b) && !mappedBy(pg, b.memoryRegion) {
		b.memoryRegion.resident--
	}
}

// mappedBy reports whether any alias of pg belongs to r. Caller holds h.mu.
func mappedBy(pg *resident, r *MemoryRegion) bool {
	for b := range pg.aliases.all() {
		if b.memoryRegion == r {
			return true
		}
	}
	return false
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
	unaliasLocked(pg, b)
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

// ErrUndroppable reports a retire that would have given up the only copy of
// bytes a guest wrote. It fails the retire rather than the VM: the checkpoint is
// durable either way, the page stays sealed and the guest keeps its memory, and
// the pager reports why its pages are still sealed.
var ErrUndroppable = errors.New("a page the volume holds no object for is not zeros")

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
