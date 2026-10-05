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
// layer, then a root, then Host.mu, then zirconRegion.mu.
//
// Eviction is step 12's: this core gives up only idle pages, pages no memory
// region maps, and an allocation that would evict a mapped page fails with
// ErrCapacity.

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
	// mu guards beside, the bindings beside the layer.
	mu sync.Mutex
	// beside is the page list of the bindings beside the layer: a slot holds
	// the binding of a page the region maps or maps from, and an Untracked
	// zero interval a compressed zero run, pages mapped to zero with no
	// binding at all, as the current core's page list does.
	beside *zirconvm.PageList[zbinding]
}

func newZirconHost(h *Host) *zirconHost {
	z := &zirconHost{host: h, roots: make(map[rootKey]*identityRoot), prefetches: make(map[*zprefetch]struct{})}
	z.pmm = &arenaPmm{z: z, zero: zirconvm.NewFramePage(nil)}
	// No compression: a page's dirty reservation is the pager's, kept beside
	// the layer, until the spill moves into this core with eviction (step 12).
	z.node = zirconvm.NewNode(z.pmm, h.pageSize, nil)
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
	zr := &zirconRegion{region: r, host: z, beside: zirconvm.NewPageList[zbinding](ps)}
	zr.resolver = &rootResolver{region: zr}
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
	for _, root := range roots {
		root.object.Destroy()
	}
	h.mu.Lock()
	h.idlePages -= int(z.rootPages)
	z.rootPages = 0
	h.mu.Unlock()
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
	// Every page this region mapped from a root is mapped by it no more, and
	// one nothing else maps is idle, kept for the next region that inherits
	// its identity.
	z.mu.Lock()
	var bound []*zbinding
	z.eachBoundLocked(0, uint64(r.pageCount), func(b *zbinding) { bound = append(bound, b) })
	z.mu.Unlock()
	h.mu.Lock()
	for _, b := range bound {
		if b.page != nil {
			z.host.unaliasLocked(b)
		}
	}
	h.mu.Unlock()
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

func (z *zirconRegion) seal(context.Context) error {
	return unsupported(CoreZircon, "seal a memory region")
}

func (z *zirconRegion) unseal(context.Context) error {
	return unsupported(CoreZircon, "unseal a memory region")
}

func (z *zirconRegion) giveBackColdCopies(context.Context) (int, error) {
	return 0, unsupported(CoreZircon, "give cold copies back")
}

// setUnpublishedAge and oldestUnpublished are the loss window's, which holds
// nothing while this core takes no store.
func (z *zirconRegion) setUnpublishedAge(time.Duration) {}

func (z *zirconRegion) oldestUnpublished() time.Time { return time.Time{} }

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
