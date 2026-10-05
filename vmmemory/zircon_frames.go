package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// A page of the zircon core is a zirconvm.VmPage whose Frame is a slot of an
// arena file: the plan keeps fileSlot in place of Zircon's physical address
// (plan: Arena and residency). The arena is the node's Pmm, but it hands out
// no page itself: every page is made by the pager, filled where the pager
// reads or copies its bytes, and supplied to the object that holds it, as a
// user pager supplies pages to Zircon. What Zircon frees goes back to the
// arena here.

// zframe is where one page's bytes are, and who maps it.
type zframe struct {
	fileSlot
	// mu is the page's lock, as the current core's resident page has one: an
	// eviction holds it across revoking every mapping of the page and writing
	// its bytes away, and whatever else changes what a page holds or who
	// holds it takes it first, so that neither meets the other halfway. It is
	// taken outside every object's lock and Host.mu.
	mu *ctxsync.Mutex
	// kind is what the memory region that made this page maps it as, which
	// every region that ever maps it agrees on.
	kind MemoryRegionKind
	// layer is the region whose own layer holds this page, or what a
	// checkpoint of it holds beside the layer, nil for a page of an identity
	// root. A region's own page goes with the region and is never idle. A
	// retire moves a page from a layer into a root. Guarded by Host.mu.
	layer *zirconRegion
	// aliases are the bindings that map this page. Guarded by Host.mu.
	aliases aliasSet[*zbinding]
	// lent is this page as the temporary identity root of a fork point lends
	// it (zlent), nil where no fork point names it. Guarded by Host.mu.
	lent *zirconvm.VmPage
	// idle marks a root's page no memory region maps: kept for the next
	// region that inherits its identity, in the don't-need queue unless a
	// cold copy pins it. Changed with Host.mu and Host.pinMu held, and read
	// with either.
	idle bool
	// replacing counts the stores whose copy took a binding off this page and
	// whose mapping command has not replaced the guest's mapping of it yet:
	// the guest goes on reading it until then, so nothing may take it. Guarded
	// by Host.mu.
	replacing int
	// coldCopies is every cold copy that will be compared with this page,
	// which keeps it in the zero-fork queue (zircon_cold.go). Guarded by
	// Host.pinMu.
	coldCopies map[*zbinding]struct{}
}

// zlent is the frame of a page a fork point lends: the page of its parent's
// layer, under the name the point gives it, in the point's temporary
// identity root. A Zircon page is in one object, so the root holds a page of
// its own that names the parent's frame; it is never the root's to give back.
type zlent struct{ frame *zframe }

// frameOf is the frame of a page this core made.
func frameOf(p *zirconvm.VmPage) *zframe {
	if lent, ok := p.Frame.(zlent); ok {
		return lent.frame
	}
	return p.Frame.(*zframe)
}

// isLent reports a page of a fork point's temporary identity root that names
// its parent's frame.
func isLent(p *zirconvm.VmPage) bool {
	_, ok := p.Frame.(zlent)
	return ok
}

// errPagerAllocates refuses the one allocation Zircon makes for itself. Every
// page here is the pager's to make, at the slot its placement and isolation
// choose, so a path that reaches the node's Pmm for a page is one this core
// does not take.
var errPagerAllocates = errors.New("vmmemory: the zircon core makes every page itself")

// arenaPmm is the arena as the node's Pmm.
type arenaPmm struct {
	z *zirconHost
	// zero is the zero page: no frame, because a zero is mapped by a zero
	// mapping and never by a slot.
	zero *zirconvm.VmPage
}

func (p *arenaPmm) AllocPage() (*zirconvm.VmPage, error) { return nil, errPagerAllocates }

// FreePage gives a page's slot back, which is what a page Zircon frees comes
// to: an object that dropped it, a supply that found the page there already.
func (p *arenaPmm) FreePage(page *zirconvm.VmPage) {
	if isLent(page) {
		// The parent's layer holds the frame, and gives it back itself.
		return
	}
	p.z.releaseFrame(page)
}

func (p *arenaPmm) ZeroPage() *zirconvm.VmPage { return p.zero }

// CountFreePages is how many more pages the arena may hold.
func (p *arenaPmm) CountFreePages() uint64 {
	h := p.z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return uint64(max(h.cfg.ResidentPages-h.held, 0))
}

// newFrame fills a slot the caller took with data and reports the page that
// holds it, in no object yet, and locked, as the current core's create
// reports a locked page: no eviction takes it before the caller has put it
// where it goes and given its lock back. A write that fails gives the slot
// back.
func (z *zirconHost) newFrame(ctx context.Context, at fileSlot, data []byte, kind MemoryRegionKind) (*zirconvm.VmPage, error) {
	h := z.host
	if sim.Bug(ctx, "pager-zero-new-page") {
		// The page is created without the bytes that were loaded or copied
		// into it, which every later read of that page then sees as zeroes.
		clear(data)
	}
	if err := at.file.Write(ctx, at.slot, data); err != nil {
		return nil, h.abandonSlots(ctx, at, 1, err)
	}
	return zirconvm.NewFramePage(newLockedZframe(at, kind, nil)), nil
}

// newLockedZframe is newZframe, locked.
func newLockedZframe(at fileSlot, kind MemoryRegionKind, layer *zirconRegion) *zframe {
	f := newZframe(at, kind, layer)
	if !f.mu.TryLock() {
		panic("vmmemory: a new page's lock is held")
	}
	return f
}

// newZframe is the frame of a page at a slot, of a region's layer or nil for
// a root's.
func newZframe(at fileSlot, kind MemoryRegionKind, layer *zirconRegion) *zframe {
	return &zframe{fileSlot: at, kind: kind, layer: layer, mu: ctxsync.NewMutex()}
}

// lockPage takes a page's lock.
func (z *zirconHost) lockPage(ctx context.Context, p *zirconvm.VmPage) error {
	return frameOf(p).mu.Lock(ctx)
}

// unlockPage gives a page's lock back and wakes whatever waits for a page to
// be free, as Host.unlock does.
func (z *zirconHost) unlockPage(p *zirconvm.VmPage) {
	frameOf(p).mu.Unlock()
	h := z.host
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
}

// lockedPage is the page b names, locked, nil where it names none: Host.current
// over the zircon core. It takes the page's lock with no other lock held, and
// looks again where an eviction or a move took the page from b meanwhile.
func (z *zirconHost) lockedPage(ctx context.Context, b *zbinding) (*zirconvm.VmPage, error) {
	h := z.host
	for {
		h.mu.Lock()
		p := b.page
		h.mu.Unlock()
		if p == nil {
			return nil, nil
		}
		if err := z.lockPage(ctx, p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		same := b.page == p
		h.mu.Unlock()
		if same {
			return p, nil
		}
		z.unlockPage(p)
	}
}

// releaseFrame gives a page's slot back to the arena. Nothing maps it: a
// page is mapped only while a binding holds it, and Zircon frees only a page
// it holds no more. A slot the arena will not punch makes the host terminal
// and is kept, as the current core's release does.
func (z *zirconHost) releaseFrame(p *zirconvm.VmPage) {
	h := z.host
	f := frameOf(p)
	if f.slot < 0 {
		return
	}
	h.mu.Lock()
	mapped := f.aliases.len() != 0
	h.mu.Unlock()
	if mapped {
		panic("vmmemory: the zircon core freed a page a memory region maps")
	}
	// Nothing the pager takes under pressure is pinned, so a page going here
	// is going for a reason of its own, and a cold copy of it has nothing left
	// to be compared with.
	z.dropCold(p)
	if err := f.file.Release(context.Background(), f.slot); err != nil {
		slog.Warn("vmmemory: giving a page's slot back failed", "slot", f.slot, "error", err)
		h.mu.Lock()
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: %w", err))
		h.signal()
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	if f.layer == nil {
		z.rootPages--
	}
	z.notIdleLocked(f)
	h.putFree(f.fileSlot)
	f.slot = -1
	h.signal()
	h.mu.Unlock()
}

// idleLocked makes a root's page nothing maps idle, at the end of the
// don't-need queue unless a cold copy pins it. Caller holds h.mu.
func (z *zirconHost) idleLocked(p *zirconvm.VmPage) {
	f := frameOf(p)
	if f.idle || f.layer != nil || f.slot < 0 || f.aliases.len() > 0 {
		return
	}
	h := z.host
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f.idle = true
	h.idlePages++
	if len(f.coldCopies) == 0 {
		z.node.PageQueues().MoveToReclaimDontNeed(p)
	}
}

// notIdleLocked ends a page being idle, which a region mapping it or its
// slot going back does. Caller holds h.mu.
func (z *zirconHost) notIdleLocked(f *zframe) {
	if !f.idle {
		return
	}
	h := z.host
	h.pinMu.Lock()
	f.idle = false
	h.pinMu.Unlock()
	h.idlePages--
}

// identityRoot is the pages one published checkpoint holds of one volume, an
// object whose pages are Clean and never change, which the layers of the
// memory regions that read them fall through to. Its offsets are the pages'
// own, because a page is shared under its identity only at the page its
// identity names (windowPlan.identity). Its page source is the pager, and a
// READ request of it is the read of its missing pages, which a fault or a
// prefetch answers.
type identityRoot struct {
	key    rootKey
	object *zirconvm.ObjectPaged
	pages  *zirconvm.CowPages
	reads  *requestSource
	// lent is the checkpoint of the fork point whose pages this root lends
	// under the name the point gave them, nil for a published checkpoint's
	// root. A lent root goes when the seal ends. Guarded by Host.mu.
	lent *MemoryRegionCheckpoint
	// lentPages are the pages of a lent root, each naming its parent's frame.
	// Guarded by Host.mu.
	lentPages []*zirconvm.VmPage
}

// rootLocked is the identity root key names, made where there is none.
// Caller holds h.mu.
func (z *zirconHost) rootLocked(key rootKey) *identityRoot {
	if root := z.roots[key]; root != nil {
		return root
	}
	h := z.host
	reads := newRequestSource()
	// A root holds the pages of one volume at the pages' own offsets, and no
	// memory region of this pager is larger than its logical budget.
	object, err := zirconvm.CreateIdentityRoot(z.node, reads.source, uint64(h.cfg.LogicalPages)*h.pageSize)
	if err != nil {
		panic("vmmemory: making an identity root: " + err.Error())
	}
	root := &identityRoot{key: key, object: object, pages: object.CowPages(), reads: reads}
	z.roots[key] = root
	return root
}

// root is rootLocked, taking h.mu.
func (z *zirconHost) root(key rootKey) *identityRoot {
	h := z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return z.rootLocked(key)
}

// supply gives an object the pages a read brought in for the run of
// consecutive pages from first, each a frame in no object: a user pager's
// SupplyPages, to an identity root or to a region's own layer. A page some
// other read supplied first stays, and this one is freed, which gives its
// slot back. The supply answers every READ request of the run.
func (z *zirconHost) supply(ctx context.Context, object *zirconvm.ObjectPaged, first uint64, pages []*zirconvm.VmPage) error {
	ps := z.host.pageSize
	length := uint64(len(pages)) * ps
	list := z.splices.Get().(*zirconvm.PageSpliceList[zirconvm.VmPage])
	list.Initialize(length)
	for i, p := range pages {
		if err := list.Insert(uint64(i)*ps, zirconvm.Page(p)); err != nil {
			panic("vmmemory: building a supply: " + err.Error())
		}
	}
	list.Finalize()
	err := object.SupplyPages(ctx, first*ps, length, list, zirconvm.PagerSupply)
	if err != nil {
		list.Free()
	}
	list.Reuse()
	z.splices.Put(list)
	return err
}

// rootResolver is a memory region layer's RootResolver: the identity root of
// an offset, and the offset in it, by the identity the fault located there.
// A fault sets the locations it holds while it holds the layer's lock, and a
// lookup reaches only offsets it has located.
//
// A root is named only where it holds the page. A page no root holds is the
// region's own to read, so the layer's lookup asks the layer's own page
// source for it, which is the region's: a fault's own read is not in flight
// for prefetches to see, as in the current core (pagerequests.go, and the
// step 8 decision of TASK-92). A fault that meets a page a prefetch is
// reading waits on the prefetch's request in the root before it looks
// (zplan.inFlight).
type rootResolver struct {
	region *zirconRegion
	// located is what the fault holding the layer's lock located, nil
	// otherwise. Guarded by the layer's lock.
	located *locations
	// last caches the root of the identity last resolved: a run of pages is
	// mostly of one checkpoint. Guarded by the layer's lock.
	last     rootKey
	lastRoot *identityRoot
}

// Locate is the root and offset holding the content of offset.
func (res *rootResolver) Locate(offset uint64) (*zirconvm.CowPages, uint64, bool) {
	loc := res.located
	if loc == nil {
		return nil, 0, false
	}
	z := res.region
	ps := z.host.host.pageSize
	page := offset / ps
	if !loc.holds(page) {
		return nil, 0, false
	}
	key, named := identityAt(loc, page)
	if !named || key.zero() {
		return nil, 0, false
	}
	root := rootOf(key)
	if res.lastRoot == nil || res.last != root {
		res.last, res.lastRoot = root, z.host.root(root)
	}
	pages, offset := res.lastRoot.pages, key.id.Page*ps
	lock := pages.Lock()
	lock.Lock()
	held := pages.PageLocked(offset) != nil
	lock.Unlock()
	if !held {
		return nil, 0, false
	}
	return pages, offset, true
}

// identityAt is windowPlan.identity of a page of located: the store page whose
// bytes it reads, which a page is shared under only at its own number.
func identityAt(loc *locations, page uint64) (pageKey, bool) {
	e, found := loc.extent(page)
	if !found {
		return pageKey{}, false
	}
	if e.Identity.Zero {
		return pageKey{id: e.Identity}, true
	}
	if e.Identity.Ref.IsZero() || e.Identity.Page != page {
		return pageKey{}, false
	}
	return pageKey{id: e.Identity}, true
}

// zbinding is what a memory region keeps beside its layer for one page:
// whether its mapping is installed, and the page it maps, which Zircon keeps
// in page tables it can read back and the pager cannot; and of a page the
// region has stored into, what Zircon has no place for: the dirty reservation
// it was admitted under, the checkpoint's copy it shares, the page it was
// copied from, whether it is cold, and whether write-ahead made it.
//
// A checkpoint's copy of a page is a zbinding too, detached from the page
// list beside the layer, as the current core's is: it aliases the page the
// guest had at the seal, AwaitingClean in the layer, and owns the reservation
// that page was admitted under (D1, D5). The guest's binding shares it until
// a store copies away from it.
type zbinding struct {
	region *zirconRegion
	index  uint64
	// page is the page this region maps here, nil where it maps none or a
	// zero: a page of an identity root, or of the region's own layer. Guarded
	// by Host.mu, as the page's aliases are.
	page *zirconvm.VmPage
	// mapped is whether the mapping is installed, and zero whether it is a
	// zero. inZeroRun marks a bound page a compressed zero run maps, as the
	// current core's binding does. Guarded by zirconRegion.mu.
	mapped, zero, inZeroRun bool
	// dirty marks a page that is the region's own state and not yet its
	// volume's: Dirty in the layer, AwaitingClean while it shares the
	// checkpoint's copy, or spilled. spill is the dirty reservation it was
	// admitted under, which a seal hands to checkpoint, the copy it then
	// shares until a store copies away from it. origin is the root's page it
	// was copied from, cold marks a copy a store trap made of origin that is
	// not yet known to be the guest's state, from coldAt (Unix nanoseconds),
	// and ahead marks one write-ahead made before any store. Guarded by
	// zirconRegion.mu.
	dirty      bool
	spill      reservation
	checkpoint *zbinding
	origin     *zirconvm.VmPage
	cold       bool
	coldAt     int64
	ahead      bool
}

// aliasLocked makes b an alias of p: the region maps it, so it is not idle.
// A page a region reaches twice, from its binding and from a checkpoint's copy
// of it, is one page of the arena. Caller holds h.mu.
func (z *zirconHost) aliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	z.notIdleLocked(f)
	counted := f.mappedBy(b.region)
	if f.aliases.add(b) && !counted {
		b.region.region.resident++
	}
	b.page = p
}

// unaliasLocked takes b's page away from it. A root's page nothing else maps
// is idle: it stays in its root, for the next region that inherits its
// identity, and is the first memory an allocation short of a slot gives up.
// Caller holds h.mu.
func (z *zirconHost) unaliasLocked(b *zbinding) {
	p := b.page
	b.page = nil
	z.forgetAliasLocked(b, p)
}

// forgetAliasLocked takes b off the aliases of p, which b no longer names.
// Caller holds h.mu.
func (z *zirconHost) forgetAliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	if !f.aliases.remove(b) {
		return
	}
	if !f.mappedBy(b.region) {
		b.region.region.resident--
	}
	z.idleLocked(p)
}

// mappedBy reports whether any alias of f belongs to z. Caller holds h.mu.
func (f *zframe) mappedBy(z *zirconRegion) bool {
	for b := range f.aliases.all() {
		if b.region == z {
			return true
		}
	}
	return false
}

// adoptLocked counts a page a supply put in an identity root, which nothing
// maps yet: idle until a binding takes it. Caller holds h.mu.
func (z *zirconHost) adoptLocked(p *zirconvm.VmPage) {
	z.rootPages++
	z.idleLocked(p)
}

// takeIdle gives up one idle page: a page of a root no memory region maps,
// in the order the don't-need queue keeps them. Zircon evicts it, as it
// evicts a clean page a pager backs, and it is read again when a region
// next faults on it. It reports whether it gave one up.
func (z *zirconHost) takeIdle() bool {
	return z.takeIdleIf(func() bool {
		z.host.mu.Lock()
		return true
	})
}

// reclaimIdle is Host.reclaimIdle under this core: the host budget's cache
// eviction, which never waits for h.mu, because the budget may call it from
// inside an allocation that holds it.
func (z *zirconHost) reclaimIdle() bool { return z.takeIdleIf(z.host.mu.TryLock) }

// takeIdleIf is takeIdle, where lock takes h.mu or reports it could not.
func (z *zirconHost) takeIdleIf(lock func() bool) bool {
	h := z.host
	if !lock() {
		return false
	}
	// The page is taken with its lock held, which keeps whatever compares or
	// copies it out of the way of its going.
	idle, ok := z.node.PageQueues().PeekDontNeedWhere(func(p *zirconvm.VmPage) bool {
		f := frameOf(p)
		if f.aliases.len() != 0 || f.replacing != 0 || f.layer != nil || !f.mu.TryLock() {
			return false
		}
		return true
	})
	h.mu.Unlock()
	if !ok {
		return false
	}
	evicted := z.evictIdle(idle.Cow, idle.Offset)
	frameOf(idle.Page).mu.Unlock()
	if !evicted {
		return false
	}
	h.mu.Lock()
	h.stats.IdleDrops++
	h.mu.Unlock()
	return true
}

// evictIdle gives up the page a root holds at offset, which nothing mapped
// when the caller looked. It is evicted only where it is still idle under the
// root's lock: a fault that takes a page marks it accessed there first, which
// takes it out of the don't-need queue. It reports whether it went.
func (z *zirconHost) evictIdle(root *zirconvm.CowPages, offset uint64) bool {
	h := z.host
	success, failure := root.ReclaimRangeForEviction(offset, h.pageSize, zirconvm.IgnoreHint)
	if failure != zirconvm.ReclaimSucceeded || success.NumPages == 0 {
		return false
	}
	return true
}

// dropOrigin gives up a page a region's stores copied from once nothing maps
// it, which detaching the region does, as the current core's releaseOrigin
// does: nothing else would ever give it up ahead of the pages other regions
// still read.
func (z *zirconHost) dropOrigin(page *zirconvm.VmPage) {
	h := z.host
	h.mu.Lock()
	mapped := frameOf(page).aliases.len() > 0
	h.mu.Unlock()
	if mapped {
		return
	}
	if !frameOf(page).mu.TryLock() {
		// Something holds it: it stays idle, for an idle drop to take.
		return
	}
	defer frameOf(page).mu.Unlock()
	if link, ok := z.node.PageQueues().Backlink(page); ok {
		z.evictIdle(link.Cow, link.Offset)
	}
}
