package vmmemory

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The zircon core: faults and stores over the region's layer and the identity
// roots of internal/zirconvm (plans/zircon-pager-port-2026-10-05.md, steps
// 10 to 12). It runs beside the current core, selected by Config.Core, and
// serves what the steps of the port have moved onto it. What stays the
// pager's own is shared with the current core: the arena and its files,
// isolation, placement, pressure, the connection, and the policy of which
// pages a fault reads and in what order, which moves as it is.
//
// A page is Zircon's: a VmPage whose Frame is a slot of an arena file (a
// frame). A published checkpoint's pages of one volume are an identity root,
// whose page source is the pager, and a memory region's own pages are its
// layer, whose lookup falls through to the identity root of each offset it
// holds nothing at. What Zircon has no place for stays in a binding beside the
// layer: whether the page is mapped, and which page it maps.
//
// The object locks are Zircon's: the layer's, and each root's. No object lock
// is held across a mapping command or a backing read: a fault collects its
// commands while it holds them and issues them after, as DeferredOps does, and
// the window's stripe, which the fault holds from start to end, keeps two
// faults of one window from issuing commands out of order. Lock order is the
// layer, then a root, then Host.mu, then zirconRegion.mu. A page's own lock
// (zframe.mu), which an eviction holds across taking every mapping of the
// page away, is taken before all of them.
//
// Checkpoints are zircon_checkpoint.go's, and eviction zircon_evict.go's.

// zirconHost is a pager's state under the zircon core, nil under the current
// one.
type zirconHost struct {
	host *Host
	// node is the pages' node: the arena as their Pmm and the page queues
	// that order them.
	node *zirconvm.Node
	pmm  *arenaPmm
	// roots is every identity root a memory region of this pager has located
	// a page of. A root lives until the pager closes. Guarded by Host.mu.
	roots map[rootKey]*identityRoot
	// prefetches is every prefetch whose slots are not settled yet. Guarded
	// by Host.mu, as are the prefetch counts of Host, which this core keeps
	// too.
	prefetches map[*zprefetch]struct{}
	// rootPages is how many pages the roots hold. Guarded by Host.mu.
	rootPages uint64
	// splices lends the splice lists a supply hands its pages over in, as
	// *zirconvm.PageSpliceList[zirconvm.VmPage]: a fault supplies a page,
	// and making a list for each was a sixth of what a fault at random
	// allocated.
	splices sync.Pool
	// multis lends the requests a fault's lookup makes, as
	// *zirconvm.MultiPageRequest, each back once it is answered.
	multis sync.Pool
}

// zirconRegion is a memory region's state under the zircon core.
type zirconRegion struct {
	region *MemoryRegion
	host   *zirconHost
	// layer is the region's own pages: Zircon's VmObjectPaged over a
	// VmCowPages whose page source is the region's own (MemoryRegion.reads)
	// and whose lookup falls through to the identity root resolver names.
	layer    *zirconvm.ObjectPaged
	pages    *zirconvm.CowPages
	resolver *rootResolver
	// mu guards beside, the bindings beside the layer, the dirty set and its
	// age.
	mu sync.Mutex
	// beside is the page list of the bindings beside the layer: a slot holds
	// the binding of a page the region maps or maps from, and an Untracked
	// zero interval a compressed zero run, pages mapped to zero with no
	// binding at all, as the current core's page list does.
	beside *zirconvm.PageList[zbinding]
	// dirtySet is every page the region may store into where it is: its own
	// Dirty state, which no checkpoint holds, and which the next seal takes
	// whole. dirtyRuns is the same set less the pages the region does not
	// map, held as runs, which is what a seal's pause write-protects; every
	// transition that changes whether a page is one of those keeps it
	// (noteSealableLocked). dirtySince is when the oldest write the region
	// holds that no checkpoint covers was made, zero while it holds none: the
	// loss window's bookkeeping, as MemoryRegion.dirtySince is the current
	// core's.
	dirtySet   map[uint64]*zbinding
	dirtyRuns  pageRuns
	dirtySince time.Time
	// coldPages is every cold copy of the region, and coldCopies the ones its
	// session's worker has not taken yet (cold.go).
	coldPages  map[uint64]*zbinding
	coldCopies map[uint64]struct{}
}

func newZirconHost(h *Host) *zirconHost {
	z := &zirconHost{host: h, roots: make(map[rootKey]*identityRoot), prefetches: make(map[*zprefetch]struct{})}
	z.pmm = &arenaPmm{z: z, zero: zirconvm.NewFramePage(nil)}
	// No compression: a frame's bytes are the pager's to move, so a page's
	// dirty reservation, the reference a spill writes its bytes to, is kept in
	// its binding beside the layer, and the spill is the pager's own
	// (zircon_evict.go).
	z.node = zirconvm.NewNode(z.pmm, h.pageSize, nil)
	// Every page ages in the reclaim queues, a region's Dirty and
	// AwaitingClean pages too, since each can be spilled (D2); a page a cold
	// copy pins waits in the zero-fork queue outside them.
	z.node.AgeDirtyPages()
	z.node.PageQueues().EnableAnonymousReclaim(false)
	z.splices.New = func() any { return zirconvm.NewPageSpliceList[zirconvm.VmPage](h.pageSize, z.node) }
	z.multis.New = func() any { return zirconvm.NewMultiPageRequest() }
	return z
}

// attach admits and attaches a memory region as the current core's Attach
// does, with a region layer of its own. A migration destination's peer
// backing, whose loads can return bytes no checkpoint holds, is refused: its
// pages are private dirty state from the moment they arrive, which this core
// takes on with the rest of the dirty set in step 12.
func (z *zirconHost) attach(ctx context.Context, backing MemoryRegionBacking, mapping Mapping) (*MemoryRegion, error) {
	if _, peer := backing.Backing.(UnpublishedLoader); peer {
		return nil, unsupported(CoreZircon, "attach a backing another host serves")
	}
	h := z.host
	r, err := h.admit(ctx, backing, mapping)
	if err != nil {
		return nil, err
	}
	zr, err := z.newRegion(r)
	if err != nil {
		h.mu.Lock()
		h.logical -= r.pageCount
		h.forgetFilesLocked(r)
		delete(h.memoryRegions, r)
		h.mu.Unlock()
		return nil, err
	}
	r.zircon = zr
	if err := r.giveFiles(ctx); err != nil {
		return r, err
	}
	if err := r.Populate(ctx); err != nil {
		return r, err
	}
	return r, nil
}

// newRegion makes a memory region's layer.
func (z *zirconHost) newRegion(r *MemoryRegion) (*zirconRegion, error) {
	ps := z.host.pageSize
	zr := &zirconRegion{region: r, host: z, beside: zirconvm.NewPageList[zbinding](ps), dirtyRuns: newPageRuns(ps)}
	zr.resolver = &rootResolver{region: zr}
	// The layer's source is the region's own, and it traps dirty
	// transitions, as a VMO whose pager tracks its writes does: a page of
	// the layer becomes Dirty only when the pager says so (DirtyPages), which
	// a store does once it holds the page's dirty reservation.
	proxy := zirconvm.NewPagerProxy(true)
	r.reads = &requestSource{source: zirconvm.NewPageSource(proxy), proxy: proxy}
	layer, err := zirconvm.CreateRegionLayer(z.node, r.reads.source, uint64(r.pageCount)*ps, zr.resolver)
	if err != nil {
		return nil, err
	}
	zr.layer, zr.pages = layer, layer.CowPages()
	return zr, nil
}

// close is what Host.Close does under this core before it gives back every
// slot: once no region is attached, the identity roots go, and their pages
// give their slots back as they go.
func (z *zirconHost) close() error {
	h := z.host
	h.mu.Lock()
	if h.logical != 0 {
		h.mu.Unlock()
		return errors.New("managed-memory regions still attached")
	}
	roots := z.roots
	z.roots = make(map[rootKey]*identityRoot)
	h.mu.Unlock()
	// Every page of a root goes back with it, and with its slot its count
	// of the roots' pages and of the idle ones (releaseFrame).
	for _, root := range roots {
		root.object.Destroy()
	}
	return nil
}

// verify is the current core's Verify: the pager has nothing of the core's to
// check.
func (z *zirconRegion) verify(ctx context.Context) error {
	r := z.region
	if err := r.mu.RLock(ctx); err != nil {
		return err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return err
	}
	if err := r.countAllocated(); err != nil {
		return err
	}
	if err := r.backing.Verify(ctx); err != nil {
		return r.fail(err)
	}
	return nil
}

// detach takes everything a memory region holds away, as the current core's
// Detach does: its prefetches end, every page it maps is no longer mapped by
// it, its own pages go back, and its layer goes.
func (z *zirconRegion) detach(ctx context.Context) error {
	r := z.region
	h := r.host
	if err := r.live.Lock(ctx); err != nil {
		return err
	}
	defer r.live.Unlock()
	if err := z.cancelPrefetches(ctx); err != nil {
		return err
	}
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	// A sealed checkpoint goes with the region, whatever publication may still
	// be reading it: its memory users are gone, and its unpublished stores
	// with them.
	if checkpoint := r.currentCheckpoint(); checkpoint != nil {
		if err := z.discardCheckpoint(ctx, checkpoint); err != nil {
			return err
		}
		r.setCheckpoint(nil)
	}
	// Every page this region mapped from a root is mapped by it no more, and
	// one nothing else maps is idle, kept for the next region that inherits
	// its identity.
	// Its writes go with it, and their reservations back to the budget. The
	// pages they were copied from go too, where nothing else maps them.
	z.mu.Lock()
	var bound []*zbinding
	var origins []*zirconvm.VmPage
	z.eachBoundLocked(0, uint64(r.pageCount), func(b *zbinding) {
		bound = append(bound, b)
		if b.origin != nil {
			origins = append(origins, b.origin)
		}
	})
	z.mu.Unlock()
	z.releaseDirty()
	h.mu.Lock()
	for _, b := range bound {
		if b.page != nil {
			z.host.unaliasLocked(b)
		}
	}
	h.mu.Unlock()
	for _, origin := range origins {
		z.host.dropOrigin(origin)
	}
	h.mu.Lock()
	h.logical -= r.pageCount
	h.forgetExtents(r)
	delete(h.memoryRegions, r)
	if r.hasZeros {
		h.zeroMemoryRegions--
		r.hasZeros = false
	}
	h.signal()
	h.mu.Unlock()
	r.closed = true
	// The layer's own pages go back with it, and its page source, the
	// region's, closes with it: no fault reads after this, so it has no
	// request to end.
	z.layer.Destroy()
	h.mu.Lock()
	h.forgetFilesLocked(r)
	h.mu.Unlock()
	z.mu.Lock()
	z.beside = zirconvm.NewPageList[zbinding](h.pageSize)
	z.mu.Unlock()
	r.pageCount = 0
	return nil
}

// settlePrefetches returns once none of this memory region's prefetches is
// running, as the current core's does.
func (z *zirconRegion) settlePrefetches(ctx context.Context) error {
	return z.region.settlePrefetchesCounted(ctx)
}

// settlePrefetches returns once no prefetch is running.
func (z *zirconHost) settlePrefetches(ctx context.Context) error {
	return z.host.settlePrefetchesCounted(ctx)
}

// The operations this core does not serve yet. Each refuses, naming itself,
// and changes nothing.

// The loss window over the zircon core: one timestamp per region, the
// oldest write it holds that no checkpoint covers, which a seal hands to its
// checkpoint and an abandoned checkpoint hands back, as losswindow.go keeps
// it for the current core.

// takeDirtySince is MemoryRegion.takeDirtySince over the zircon core.
func (z *zirconRegion) takeDirtySince() time.Time {
	r := z.region
	z.mu.Lock()
	since := z.dirtySince
	z.dirtySince = time.Time{}
	z.mu.Unlock()
	r.bindingsMu.Lock()
	r.windowAsked = false
	r.bindingsMu.Unlock()
	return since
}

// restoreDirtySince is MemoryRegion.restoreDirtySince over the zircon core.
func (z *zirconRegion) restoreDirtySince(since time.Time) {
	r := z.region
	z.mu.Lock()
	z.dirtySince = older(z.dirtySince, since)
	z.mu.Unlock()
	r.bindingsMu.Lock()
	r.windowAsked = false
	r.bindingsMu.Unlock()
}

// setUnpublishedAge is MemoryRegion.SetUnpublishedAge: the older of age ago
// and the region's own oldest unpublished write stands.
func (z *zirconRegion) setUnpublishedAge(age time.Duration) {
	if age <= 0 {
		return
	}
	z.restoreDirtySince(z.region.host.clock.Now().Add(-age))
}

// oldestUnpublished is MemoryRegion.OldestUnpublished over the zircon core:
// the region's own dirty set and the checkpoint still draining out of it.
func (z *zirconRegion) oldestUnpublished() time.Time {
	z.mu.Lock()
	since := z.dirtySince
	z.mu.Unlock()
	if _, draining := z.region.sealState(); draining != nil {
		since = older(since, draining.since())
	}
	return since
}

// dirtyCount is MemoryRegion.dirtyCount: the pages of the dirty set, which
// the next seal takes.
func (z *zirconRegion) dirtyCount() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return len(z.dirtySet)
}

// noteDirtyLocked puts b in the dirty set, Dirty and writable where it is,
// and starts the region's loss window where it held none. Caller holds z.mu.
func (z *zirconRegion) noteDirtyLocked(b *zbinding) {
	if z.dirtySet == nil {
		z.dirtySet = make(map[uint64]*zbinding)
	}
	z.dirtySet[b.index] = b
	if z.dirtySince.IsZero() {
		z.dirtySince = z.region.host.clock.Now()
	}
	z.noteSealableLocked(b)
}

// noteSealableLocked records whether b is a page the next seal
// write-protects: the region's own dirty state, held by no checkpoint, and
// mapped, as MemoryRegion.noteSealableLocked does. Caller holds z.mu.
func (z *zirconRegion) noteSealableLocked(b *zbinding) {
	sealable := b.writable() && b.mapped
	if z.dirtyRuns.has(b.index) == sealable {
		return
	}
	if sealable {
		z.dirtyRuns.add(b.index, b.index+1)
	} else {
		z.dirtyRuns.remove(b.index)
	}
}

// writable reports whether the guest may store into b's page where it is:
// its own dirty state, which no checkpoint still holds.
func (b *zbinding) writable() bool { return b.dirty && b.checkpoint == nil }

func (z *zirconRegion) readResident(context.Context, uint64, []byte) (bool, bool, error) {
	return false, false, unsupported(CoreZircon, "read a resident page")
}

func (z *zirconRegion) resident() ([]uint64, error) {
	return nil, unsupported(CoreZircon, "list resident pages")
}

func (z *zirconRegion) handoff(context.Context) (time.Duration, error) {
	return 0, unsupported(CoreZircon, "hand a memory region off")
}

func (z *zirconRegion) unpublished() ([]uint64, error) {
	return nil, unsupported(CoreZircon, "list unpublished pages")
}
