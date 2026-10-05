package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
	// kind is what the memory region that made this page maps it as, which
	// every region that ever maps it agrees on.
	kind MemoryRegionKind
	// own marks a page of a region's own layer, which goes with the region
	// and is never idle; every other page is an identity root's.
	own bool
	// aliases are the bindings that map this page. Guarded by Host.mu.
	aliases aliasSet[*zbinding]
}

// frameOf is the frame of a page this core made.
func frameOf(p *zirconvm.VmPage) *zframe { return p.Frame.(*zframe) }

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
func (p *arenaPmm) FreePage(page *zirconvm.VmPage) { p.z.releaseFrame(page) }

func (p *arenaPmm) ZeroPage() *zirconvm.VmPage { return p.zero }

// CountFreePages is how many more pages the arena may hold.
func (p *arenaPmm) CountFreePages() uint64 {
	h := p.z.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return uint64(max(h.cfg.ResidentPages-h.held, 0))
}

// newFrame fills a slot the caller took with data and reports the page that
// holds it, in no object yet. It is the current core's create: a write that
// fails gives the slot back.
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
	return zirconvm.NewFramePage(&zframe{fileSlot: at, kind: kind}), nil
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
	if err := f.file.Release(context.Background(), f.slot); err != nil {
		slog.Warn("vmmemory: giving a page's slot back failed", "slot", f.slot, "error", err)
		h.mu.Lock()
		h.err = errors.Join(h.err, fmt.Errorf("managed arena terminal: %w", err))
		h.signal()
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	if f.aliases.len() != 0 {
		h.mu.Unlock()
		panic("vmmemory: the zircon core freed a page a memory region maps")
	}
	h.putFree(f.fileSlot)
	f.slot = -1
	h.signal()
	h.mu.Unlock()
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
// it was admitted under, the page it was copied from, and whether
// write-ahead made it.
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
	// dirty marks a page of the layer the region has stored into, Dirty
	// there, which the guest may store into where it is; spill is the dirty
	// reservation it was admitted under, origin the root's page it was
	// copied from, and ahead marks one write-ahead made before any store.
	// Guarded by zirconRegion.mu.
	dirty  bool
	spill  reservation
	origin *zirconvm.VmPage
	ahead  bool
}

// aliasLocked makes b an alias of p: the region maps it, so it is not idle.
// Caller holds h.mu.
func (z *zirconHost) aliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	if f.aliases.len() == 0 && !f.own {
		z.host.idlePages--
	}
	f.aliases.add(b)
	b.page = p
	b.region.region.resident++
}

// unaliasLocked takes b's page away from it. A root's page nothing else maps
// is idle: it stays in its root, for the next region that inherits its
// identity, and is the first memory an allocation short of a slot gives up.
// Caller holds h.mu.
func (z *zirconHost) unaliasLocked(b *zbinding) {
	p := b.page
	f := frameOf(p)
	f.aliases.remove(b)
	b.page = nil
	b.region.region.resident--
	if f.aliases.len() == 0 && !f.own {
		z.host.idlePages++
		z.node.PageQueues().MoveToReclaimDontNeed(p)
	}
}

// adoptLocked counts a page a supply put in an identity root, which nothing
// maps yet: idle until a binding takes it. Caller holds h.mu.
func (z *zirconHost) adoptLocked(*zirconvm.VmPage) {
	z.host.idlePages++
	z.rootPages++
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
	idle, ok := z.node.PageQueues().PeekDontNeedWhere(func(p *zirconvm.VmPage) bool {
		return frameOf(p).aliases.len() == 0
	})
	h.mu.Unlock()
	if !ok {
		return false
	}
	if !z.evictIdle(idle.Cow, idle.Offset) {
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
	h.mu.Lock()
	h.idlePages -= int(success.NumPages)
	z.rootPages -= success.NumPages
	h.signal()
	h.mu.Unlock()
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
	if link, ok := z.node.PageQueues().Backlink(page); ok {
		z.evictIdle(link.Cow, link.Offset)
	}
}
