package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The pager's pages run over the region layers and the identity roots of
// internal/zirconvm, the port of Zircon's page layer
// (plans/zircon-pager-port-2026-10-05.md). What is the pager's own is the
// arena and its files, isolation, placement, pressure, the connection, and
// the policy of which pages a fault reads and in what order.
//
// A page is Zircon's: a zirconvm.VmPage whose Frame is a slot of an arena file
// (a frame). The plan keeps fileSlot in place of Zircon's physical address
// (plan: Arena and residency). The arena is the node's Pmm, but it hands out
// no page itself: every page is made by the pager, filled where the pager
// reads or copies its bytes, and supplied to the object that holds it, as a
// user pager supplies pages to Zircon. What Zircon frees goes back to the
// arena here.
//
// A published checkpoint's pages of one volume are an identity root, whose
// page source is the pager, and a memory region's own pages are its layer,
// whose lookup falls through to the identity root of each offset it holds
// nothing at. What Zircon has no place for stays in a binding beside the
// layer (bindings.go): whether the page is mapped, and which page it maps.
//
// The object locks are Zircon's: the layer's, and each root's. No object lock
// is held across a mapping command or a backing read: a fault collects its
// commands while it holds them and issues them after, as DeferredOps does, and
// the window's stripe, which the fault holds from start to end, keeps two
// faults of one window from issuing commands out of order. Lock order is the
// layer, then a root, then Host.mu, then MemoryRegion.bindingsMu. A page's own
// lock (zframe.mu), which an eviction holds across taking every mapping of the
// page away, is taken before all of them.

// zframe is where one page's bytes are, and who maps it.
type zframe struct {
	fileSlot
	// mu is the page's lock, as the current core's resident page has one: an
	// eviction holds it across revoking every mapping of the page and writing
	// its bytes away, and whatever else changes what a page holds or who
	// holds it takes it first, so that neither meets the other halfway. It is
	// taken outside every object's lock and Host.mu. Every page a fault makes
	// has one, so it is a LazyMutex, which allocates nothing until something
	// waits for it.
	mu ctxsync.LazyMutex
	// kind is what the memory region that made this page maps it as, which
	// every region that ever maps it agrees on.
	kind MemoryRegionKind
	// layer is the region whose own layer holds this page, or what a
	// checkpoint of it holds beside the layer, nil for a page of an identity
	// root. A region's own page goes with the region and is never idle. A
	// retire moves a page from a layer into a root. Guarded by Host.mu.
	layer *MemoryRegion
	// aliases are the bindings that map this page. Guarded by Host.mu.
	aliases aliasSet[*zbinding]
	// lent is this page as the temporary identity root of a fork point lends
	// it (zlent), nil where no fork point names it. Guarded by Host.mu.
	lent *zirconvm.VmPage
	// coldCopies is every cold copy that will be compared with this page,
	// which keeps it in the zero-fork queue (zircon_cold.go). Guarded by
	// Host.pinMu.
	coldCopies map[*zbinding]struct{}
	// replacing counts the stores whose copy took a binding off this page and
	// whose mapping command has not replaced the guest's mapping of it yet:
	// the guest goes on reading it until then, so nothing may take it. Guarded
	// by Host.mu. It and the two marks after it share one word.
	replacing int32
	// idle marks a root's page no memory region maps: kept for the next
	// region that inherits its identity, in the don't-need queue unless a
	// cold copy pins it. Changed with Host.mu and Host.pinMu held, and read
	// with either.
	idle bool
	// dropped marks a page whose last mapping went while a store replaced it,
	// which goes back once that store's command lands. Guarded by Host.mu.
	dropped bool
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
	host *Host
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
	p.host.releaseFrame(page)
}

func (p *arenaPmm) ZeroPage() *zirconvm.VmPage { return p.zero }

// CountFreePages is how many more pages the arena may hold.
func (p *arenaPmm) CountFreePages() uint64 {
	h := p.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return uint64(max(h.cfg.ResidentPages-h.held, 0))
}

// newFrame fills a slot the caller took with data and reports the page that
// holds it, in no object yet, and locked, as the current core's create
// reports a locked page: no eviction takes it before the caller has put it
// where it goes and given its lock back. A write that fails gives the slot
// back.
func (h *Host) newFrame(ctx context.Context, at fileSlot, data []byte, kind MemoryRegionKind) (*zirconvm.VmPage, error) {
	if sim.Bug(ctx, "pager-zero-new-page") {
		// The page is created without the bytes that were loaded or copied
		// into it, which every later read of that page then sees as zeroes.
		clear(data)
	}
	if err := at.file.Write(ctx, at.slot, data); err != nil {
		return nil, h.abandonSlots(ctx, at, 1, err)
	}
	page := zirconvm.NewFramePage(newLockedZframe(at, kind, nil))
	h.noteFrame(page)
	return page, nil
}

// noteFrame records a page of a private or a fork file at its slot, which
// is where an allocation of an isolated arena finds it.
func (h *Host) noteFrame(page *zirconvm.VmPage) {
	f := frameOf(page)
	if f.file.frames == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f.file.frames[f.slot] = page
}

// newLockedZframe is newZframe, locked.
func newLockedZframe(at fileSlot, kind MemoryRegionKind, layer *MemoryRegion) *zframe {
	f := newZframe(at, kind, layer)
	if !f.mu.TryLock() {
		panic("vmmemory: a new page's lock is held")
	}
	return f
}

// newZframe is the frame of a page at a slot, of a region's layer or nil for
// a root's.
func newZframe(at fileSlot, kind MemoryRegionKind, layer *MemoryRegion) *zframe {
	return &zframe{fileSlot: at, kind: kind, layer: layer}
}

// lockPage takes a page's lock.
func (h *Host) lockPage(ctx context.Context, p *zirconvm.VmPage) error {
	return frameOf(p).mu.Lock(ctx)
}

// unlockPage gives a page's lock back and wakes whatever waits for a page to
// be free, as Host.unlock does.
func (h *Host) unlockPage(p *zirconvm.VmPage) {
	found := h.probe.stable(context.Background(), h, frameOf(p), "unlock")
	frameOf(p).mu.Unlock()
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
	if found == "" {
		found = h.probe.take()
	}
	if found != "" {
		panic(found)
	}
}

// probeFrame is a page as the probe build's audit sees it, nil for none.
func probeFrame(p *zirconvm.VmPage) probePage {
	if p == nil {
		return nil
	}
	return frameOf(p)
}

// lockedPage is the page b names, locked, nil where it names none: Host.current
// over the zircon core. It takes the page's lock with no other lock held, and
// looks again where an eviction or a move took the page from b meanwhile.
func (h *Host) lockedPage(ctx context.Context, b *zbinding) (*zirconvm.VmPage, error) {
	for {
		h.mu.Lock()
		p := b.page
		h.mu.Unlock()
		if p == nil {
			return nil, nil
		}
		if err := h.lockPage(ctx, p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		same := b.page == p
		h.mu.Unlock()
		if same {
			return p, nil
		}
		h.unlockPage(p)
	}
}

// releaseFrame gives a page's slot back to the arena. Nothing maps it: a
// page is mapped only while a binding holds it, and Zircon frees only a page
// it holds no more. A slot the arena will not punch makes the host terminal
// and is kept, as the current core's release does.
func (h *Host) releaseFrame(p *zirconvm.VmPage) {
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
	h.dropCold(p)
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
		h.rootPages--
	}
	h.notIdleLocked(f)
	if f.file.frames[f.slot] == p {
		delete(f.file.frames, f.slot)
	}
	h.putFree(f.fileSlot)
	f.slot = -1
	h.signal()
	h.mu.Unlock()
}

// idleLocked makes a root's page nothing maps idle, at the end of the
// don't-need queue unless a cold copy pins it. Caller holds h.mu.
func (h *Host) idleLocked(p *zirconvm.VmPage) {
	f := frameOf(p)
	if f.idle || f.layer != nil || f.slot < 0 || f.aliases.len() > 0 {
		return
	}
	if _, queued := h.node.PageQueues().Backlink(p); !queued {
		// In no object: it is going, not waiting to be inherited.
		return
	}
	h.pinMu.Lock()
	defer h.pinMu.Unlock()
	f.idle = true
	h.idlePages++
	if len(f.coldCopies) == 0 {
		h.node.PageQueues().MoveToReclaimDontNeed(p)
	}
}

// notIdleLocked ends a page being idle, which a region mapping it or its
// slot going back does. Caller holds h.mu.
func (h *Host) notIdleLocked(f *zframe) {
	if !f.idle {
		return
	}
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
	// copies are the pages of a lent root copied into the point's fork file,
	// for children on this host of an isolated arena. Guarded by Host.mu.
	copies []*zirconvm.VmPage
	// published marks a lent root whose point published the name it lent:
	// it keeps the pages published under it when the seal ends. Guarded by
	// Host.mu.
	published bool
}

// rootLocked is the identity root key names, made where there is none.
// Caller holds h.mu.
func (h *Host) rootLocked(key rootKey) *identityRoot {
	if root := h.roots[key]; root != nil {
		return root
	}
	reads := newRequestSource()
	// A root holds the pages of one volume at the pages' own offsets, and no
	// memory region of this pager is larger than its logical budget.
	object, err := zirconvm.CreateIdentityRoot(h.node, reads.source, uint64(h.cfg.LogicalPages)*h.pageSize)
	if err != nil {
		panic("vmmemory: making an identity root: " + err.Error())
	}
	root := &identityRoot{key: key, object: object, pages: object.CowPages(), reads: reads}
	h.roots[key] = root
	return root
}

// root is rootLocked, taking h.mu.
func (h *Host) root(key rootKey) *identityRoot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rootLocked(key)
}

// supply gives an object the pages a read brought in for the run of
// consecutive pages from first, each a frame in no object: a user pager's
// SupplyPages, to an identity root or to a region's own layer. A page some
// other read supplied first stays, and this one is freed, which gives its
// slot back. The supply answers every READ request of the run.
func (h *Host) supply(ctx context.Context, object *zirconvm.ObjectPaged, first uint64, pages []*zirconvm.VmPage) error {
	ps := h.pageSize
	length := uint64(len(pages)) * ps
	list := h.splices.Get().(*zirconvm.PageSpliceList[zirconvm.VmPage])
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
	h.splices.Put(list)
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
	region *MemoryRegion
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
	r := res.region
	ps := r.host.pageSize
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
		res.last, res.lastRoot = root, r.host.root(root)
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

// aliasLocked makes b an alias of p: the region maps it, so it is not idle.
// A page a region reaches twice, from its binding and from a checkpoint's copy
// of it, is one page of the arena. Caller holds h.mu.
func (h *Host) aliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	// The probe build's audit of what a guest is handed; what it finds is
	// reported once nothing is held (unlockPage).
	h.probe.keep(h.probe.bind(h, b, f))
	h.notIdleLocked(f)
	counted := f.mappedBy(b.region)
	if f.aliases.add(b) && !counted {
		b.region.resident++
	}
	b.page = p
}

// unaliasLocked takes b's page away from it. A root's page nothing else maps
// is idle: it stays in its root, for the next region that inherits its
// identity, and is the first memory an allocation short of a slot gives up.
// Caller holds h.mu.
func (h *Host) unaliasLocked(b *zbinding) {
	p := b.page
	b.page = nil
	h.forgetAliasLocked(b, p)
}

// forgetAliasLocked takes b off the aliases of p, which b no longer names.
// Caller holds h.mu.
func (h *Host) forgetAliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	if !f.aliases.remove(b) {
		return
	}
	if !f.mappedBy(b.region) {
		b.region.resident--
	}
	h.idleLocked(p)
}

// forgetGoingAliasLocked takes b off the aliases of p, a page in no object
// any more, which therefore never becomes idle. Caller holds h.mu.
func (h *Host) forgetGoingAliasLocked(b *zbinding, p *zirconvm.VmPage) {
	f := frameOf(p)
	if f.aliases.remove(b) && !f.mappedBy(b.region) {
		b.region.resident--
	}
}

// mappedBy reports whether any alias of f belongs to r. Caller holds h.mu.
func (f *zframe) mappedBy(r *MemoryRegion) bool {
	for b := range f.aliases.all() {
		if b.region == r {
			return true
		}
	}
	return false
}

// adoptLocked counts a page a supply put in an identity root, which nothing
// maps yet: idle until a binding takes it. Caller holds h.mu.
func (h *Host) adoptLocked(p *zirconvm.VmPage) {
	h.rootPages++
	h.idleLocked(p)
}

// takeIdle gives up one idle page: a page of a root no memory region maps,
// in the order the don't-need queue keeps them. Zircon evicts it, as it
// evicts a clean page a pager backs, and it is read again when a region
// next faults on it. It reports whether it gave one up.
func (h *Host) takeIdle() bool {
	return h.takeIdleIf(func() bool {
		h.mu.Lock()
		return true
	}, nil)
}

// takeIdleIf is takeIdle, where lock takes h.mu or reports it could not, of
// the idle pages want accepts, any where want is nil. want is called with
// h.mu held.
func (h *Host) takeIdleIf(lock func() bool, want func(*zframe) bool) bool {
	if !lock() {
		return false
	}
	// The page is taken with its lock held, which keeps whatever compares or
	// copies it out of the way of its going.
	idle, ok := h.node.PageQueues().PeekDontNeedWhere(func(p *zirconvm.VmPage) bool {
		f := frameOf(p)
		if f.aliases.len() != 0 || f.replacing != 0 || f.layer != nil || (want != nil && !want(f)) || !f.mu.TryLock() {
			return false
		}
		return true
	})
	h.mu.Unlock()
	if !ok {
		return false
	}
	evicted := h.evictIdle(idle.Cow, idle.Offset)
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
func (h *Host) evictIdle(root *zirconvm.CowPages, offset uint64) bool {
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
func (h *Host) dropOrigin(page *zirconvm.VmPage) {
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
	if link, ok := h.node.PageQueues().Backlink(page); ok {
		h.evictIdle(link.Cow, link.Offset)
	}
}

// pageKey identifies immutable bytes by the store page object that holds them.
// A pager page is exactly one store page, so one identity covers a whole pageKey.
type pageKey struct {
	id control.Identity
}

func (f pageKey) zero() bool { return f.id.Zero }
