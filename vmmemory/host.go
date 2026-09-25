package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/latency"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
)

// Host accounts for shared backing under a short metadata lock. Each MemoryRegion
// orders faults per read-ahead window and flushes for the whole memory region;
// resident pages independently serialize alias changes and reclamation.
// Storage and mapping I/O never hold the host lock. Recency tracks faults and
// read-ahead, not accesses through already present PTEs.
type Host struct {
	// pageSize is this pager's unit, fixed by its configuration: its arena
	// slots, its spill slots, the numbers it faults and serves, and the
	// alignment every memory region it maps must have. Nothing here is ever the other
	// pager's page, and no count of these pages may be added to one of those.
	pageSize  uint64
	resources *resource.Budget
	// extents is the extent each range of each memory region owns, and extentPages how
	// many offsets one extent has — the pages of this pager one 2 MiB range
	// holds. A pager whose page is the whole range has one, which is a pager
	// that places nothing. Both are guarded by mu; see placement.go.
	extents     map[extentKey]*extent
	extentPages int
	// reservations is the dirty budget: the spill file's slots and what each
	// holds. See reservations.go.
	reservations *reservations
	mu           sync.Mutex
	cfg          Config
	// clock times the fault path. It is Config.Clock, or the wall clock.
	clock platform.Clock
	// files is every file this pager has made of its arena, by number. It
	// makes file 0 when it starts, and every page is in it.
	files        []*arenaFile
	spill        platform.File
	clean        map[pageKey]*resident
	cleanVersion uint64
	// memory regions is every attached memory region, which is what the dirty budget's
	// pressure is measured and acted on across: the budget is the host's, so
	// the checkpoint that relieves it need not be the waiting memory region's.
	memoryRegions map[*MemoryRegion]struct{}
	// pressure is who to ask for that checkpoint, highWater the dirty occupancy
	// at which the host asks without waiting to be empty, and asked whether it
	// has already asked since the budget last fell below that mark.
	pressure  Pressure
	highWater int
	asked     bool
	// flushed is what a guest's flush of a memory region is handed to. See SetFlushed.
	flushed           func(*MemoryRegion, func(error))
	zeroMemoryRegions int // attached memory regions retaining knowledge of explicit zeros
	lru               pageList
	// idle is the resident pages no memory region maps, oldest first: published
	// pages kept for the next memory region that inherits their identity, and given
	// up before any mapped page when a slot is short. See Host.idleLocked.
	idle pageList
	// unregisterIdle takes the idle pages out of the host budget's cache,
	// which Close does before it gives their memory back.
	unregisterIdle func()
	logical        int
	dirty          int
	// displaced counts the evictions of pages a memory region mapped, which is
	// the clock a memory region's protection is measured on: see
	// protectedLocked.
	displaced uint64
	changed   chan struct{}
	// revoked is closed, and replaced, whenever a revocation lands. A fault
	// whose mapping its client refused waits for it. See revocations.
	revoked chan struct{}
	// windows lends out the buffers window reads fill, as *[]byte so that
	// handing one back allocates nothing. See takeWindow.
	windows   sync.Pool
	io        chan struct{}
	writeback chan struct{}
	err       error
	stats     Stats
	// The UFFD reader increments these outside the metadata lock: an idle
	// descriptor read must not contend with page transitions.
	uffdReads   atomic.Uint64
	remapEvents atomic.Uint64
	// Latency histograms of the fault path, also written outside the metadata
	// lock. They are read-only instrumentation: nothing consults them and no
	// decision depends on them.
	faultLatency, faultQueueLatency latency.Histogram
	mappingLatency, resolveLatency  latency.Histogram
	loadLatency, revokeLatency      latency.Histogram
	protectLatency, sealLatency     latency.Histogram
	sealWalkLatency                 latency.Histogram
	// probe is the pager's audit of what it hands a guest, and is nothing at
	// all unless this build has the sproutfsprobe tag; see probe_on.go.
	probe probeState
	// changeSeed keys the block sums Config.MeasureChanges compares.
	changeSeed maphash.Seed
}

// maximumReadAheadBytes is the largest run one fault may hold a buffer for,
// independently of the pager geometry.
const maximumReadAheadBytes = 16 << 20

// populationWindowBytes is the window a population walks metadata in. It is a
// variable only so a test can cross a window boundary without a memory region of
// production size.
var populationWindowBytes uint64 = 256 << 20

// New uses a dedicated scratch spill file. It is not crash recovery metadata
// and must not be shared with another Host, which includes the other pager of
// the same host: each owns its arena and its spill file alone. The file's
// maximum size is DirtyPages times this pager's page; acknowledged durability
// always goes through Backing, never spill. The caller retains ownership of
// Arena, and of every file it makes, and of spill until every MemoryRegion
// detaches.
func New(ctx context.Context, resources *resource.Budget, cfg Config, arena Arena, spill platform.File) (*Host, error) {
	if resources == nil {
		return nil, ErrConfig
	}
	// A pager's page is a volume's page: the geometry the store writes is the
	// one source of truth for which sizes exist, so a pager cannot be built on
	// a size no volume could be published in.
	if _, err := checkpoint.GeometryFor(cfg.PageSize); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	pageSize := cfg.PageSize
	if cfg.ResidentPages < 1 || cfg.LogicalPages < 1 || cfg.DirtyPages < 1 ||
		cfg.DirtyPages > cfg.LogicalPages || cfg.ResidentPages > cfg.LogicalPages ||
		uint64(cfg.LogicalPages) > math.MaxInt64/pageSize || arena == nil || spill == nil ||
		(cfg.Arena != ArenaShared && cfg.Arena != ArenaIsolated) {
		return nil, ErrConfig
	}
	// A pager that places nothing has one address per page, which is what PMEM
	// runs: its offsets and its pages are one number.
	cfg.ArenaOffsets = cfg.Offsets()
	if cfg.ArenaOffsets < cfg.ResidentPages || uint64(cfg.ArenaOffsets) > math.MaxInt64/pageSize {
		return nil, ErrConfig
	}
	if cfg.ConcurrentIO == 0 {
		cfg.ConcurrentIO = 16
	}
	if resources.Stats().Limit < int64(pageSize) {
		return nil, ErrConfig
	}
	if cfg.ReadAheadPages == 0 {
		cfg.ReadAheadPages = 1
	}
	if cfg.WriteAheadPages == 0 {
		cfg.WriteAheadPages = 1
	}
	if cfg.SettleWorkers == 0 {
		cfg.SettleWorkers = 1
	}
	if cfg.SettleWorkers < 1 || cfg.SettleWorkers > 1024 {
		return nil, ErrConfig
	}
	// The read-ahead bound is a buffer one fault may hold, so it is a number of
	// bytes; what this pager admits is that many of its own pages.
	if cfg.ConcurrentIO < 1 || cfg.ConcurrentIO > 1024 ||
		cfg.ReadAheadPages < 1 || uint64(cfg.ReadAheadPages) > maximumReadAheadBytes/pageSize || cfg.ReadAheadPages&(cfg.ReadAheadPages-1) != 0 ||
		cfg.WriteAheadPages < 1 || cfg.WriteAheadPages > 4096 {
		return nil, ErrConfig
	}
	if err := spill.Truncate(ctx, 0); err != nil {
		return nil, err
	}
	if err := spill.Truncate(ctx, int64(cfg.DirtyPages)*int64(pageSize)); err != nil {
		return nil, err
	}
	// An extent is one 2 MiB-aligned range's worth of this pager's pages: 512 at
	// 4 KiB, and one at 2 MiB, which is a pager with nothing to place.
	extentPages := int(rangeBytes / pageSize)
	file, err := arena.File(ctx, cfg.ArenaOffsets)
	if err != nil {
		return nil, fmt.Errorf("making file 0 of the arena: %w", err)
	}
	h := &Host{changeSeed: maphash.MakeSeed(), pageSize: pageSize, cfg: cfg, clock: platform.ClockOr(cfg.Clock), spill: spill, resources: resources, reservations: newReservations(cfg.DirtyPages),
		files:       []*arenaFile{newArenaFile(file, 0, cfg.ArenaOffsets, cfg.ResidentPages, extentPages)},
		extents:     make(map[extentKey]*extent),
		extentPages: extentPages,
		clean:       make(map[pageKey]*resident), changed: make(chan struct{}), revoked: make(chan struct{}),
		lru: pageList{links: recentLinks}, idle: pageList{links: idleLinks},
		memoryRegions: make(map[*MemoryRegion]struct{}), highWater: highWater(cfg.DirtyPages),
		io: make(chan struct{}, cfg.ConcurrentIO), writeback: make(chan struct{}, 1)}
	// Idle pages are the host budget's cache: any consumer short of memory
	// takes them before it waits, as it takes the checkpoint cache's.
	h.unregisterIdle = resources.RegisterCache(h.reclaimIdle)
	return h, nil
}

// Resources returns the same host-wide budget used for resident pages.
func (h *Host) Resources() *resource.Budget { return h.resources }

// PageSize is this pager's unit. A memory region's size and a volume's page must be a
// multiple of it and equal to it respectively, and every page count this pager
// reports is counted in it — which is why a caller adding two pagers' numbers
// must convert to bytes first.
func (h *Host) PageSize() uint64 { return h.pageSize }

// LogicalHeadroom is how many more logical pages this pager would still admit.
// A memory region larger than this is refused at attachment, which is a VMM that has
// already started and a guest that has to be killed, so the thing that decides
// to run a VM asks this before it starts one.
func (h *Host) LogicalHeadroom() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.LogicalPages - h.logical
}

// Close releases any unpublished arena allocations left by failed cleanup.
// All memory regions must already be detached. The caller still owns the arena and
// spill handles and closes them after this succeeds. A failed punch retains
// its reservation and can be retried; new attachments are no longer accepted.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.logical != 0 {
		return errors.New("managed-memory regions still attached")
	}
	if !errors.Is(h.err, ErrClosed) {
		h.err = errors.Join(ErrClosed, h.err)
	}
	h.signal()
	h.unregisterIdle()
	var result error
	for _, f := range h.files {
		for slot, entry := range f.leases {
			if entry.lease == nil {
				continue
			}
			if err := f.Release(ctx, slot); err != nil {
				result = errors.Join(result, err)
				continue
			}
			h.putFree(fileSlot{f, slot})
		}
	}
	if err := h.spill.Truncate(ctx, 0); err != nil {
		result = errors.Join(result, err)
	} else {
		h.reservations.forget()
	}
	return result
}

// Changed is closed only under the host lock. Allocators waiting on busy pages
// wake after progress, rather than treating temporary lock contention as OOM.
func (h *Host) signal() { close(h.changed); h.changed = make(chan struct{}) }

// changes reports the signal the host's next progress closes: a page taken or
// released, a reservation moved, a mapping revoked. It is what waits for work
// only some other transition can make possible.
func (h *Host) changes() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

// revocations reports the signal the host's next revocation closes. A client
// refuses a mapping command for want of mapping budget, and a revocation is the
// only work of this pager's that gives a client budget back. Any other change
// does not: a fault that waited for any change would be woken by the pages it
// takes and gives back itself, and two refused faults would wake each other
// for as long as their client refuses them.
func (h *Host) revocations() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revoked
}

// revokedLocked records that a revocation landed. Caller holds h.mu.
func (h *Host) revokedLocked() {
	close(h.revoked)
	h.revoked = make(chan struct{})
}

func (h *Host) unlock(pg *resident) {
	found := h.probe.stable(context.Background(), h, pg, "unlock")
	pg.mu.Unlock()
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
	if found != "" {
		panic(found)
	}
}

// unlockAll releases a batch of pages and wakes waiters once rather than once
// per page. A seal, a revoke and a plan each release many at a time, and a
// waiter rechecks everything the batch changed whichever wake reaches it.
func (h *Host) unlockAll(pages []*resident) {
	if len(pages) == 0 {
		return
	}
	for _, pg := range pages {
		pg.mu.Unlock()
	}
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
}

// unlockRun is unlock for a batch of pages: each is checked as unlock checks it,
// and waiters are woken once for the lot.
func (h *Host) unlockRun(pages []*resident) {
	found := ""
	for _, pg := range pages {
		if f := h.probe.stable(context.Background(), h, pg, "unlock"); f != "" && found == "" {
			found = f
		}
	}
	h.unlockAll(pages)
	if found != "" {
		panic(found)
	}
}

// locked runs fn under the binding's current page lock and always releases
// that lock. A binding with no resident page is a no-op.
func (h *Host) locked(ctx context.Context, b *binding, fn func(pg *resident) error) error {
	pg, err := h.current(ctx, b)
	if err != nil || pg == nil {
		return err
	}
	err = fn(pg)
	h.unlock(pg)
	return err
}

// takeWindow lends a fault the bytes its window read fills, of the given pages
// of this pager. A window read covers the whole span of the pages it is
// fetching, holes and all, so a fault that wants two pages at opposite ends of
// a 2 MiB run still fills a 2 MiB buffer — and allocating one per fault is
// megabytes of garbage per guest page fault. A fault holds one only while its
// read runs, and an I/O permit for the whole of that, so what these cost a host
// at once is ConcurrentIO windows, which is the bound that permit already
// states. They come back dirty: only the pages a read asked for are ever taken
// out of one.
func (h *Host) takeWindow(pages uint64) *[]byte {
	size := pages * h.pageSize
	if held, ok := h.windows.Get().(*[]byte); ok {
		if uint64(cap(*held)) >= size {
			*held = (*held)[:size]
			return held
		}
		h.windows.Put(held)
	}
	buffer := make([]byte, max(size, uint64(h.cfg.ReadAheadPages)*h.pageSize))[:size]
	return &buffer
}

func (h *Host) putWindow(buffer *[]byte) { h.windows.Put(buffer) }

func (h *Host) beginIO(ctx context.Context) error {
	select {
	case h.io <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (h *Host) endIO() { <-h.io }

// A publication's read of a sealed checkpoint can borrow an ordinary permit
// or the reserved progress permit. Without the reserved one a guest dirtying
// faster than its checkpoint uploads would hold every ordinary permit
// waiting for the dirty budget the upload is about to release.
func (h *Host) beginCheckpointIO(ctx context.Context) (func(), error) {
	select {
	case h.writeback <- struct{}{}:
		return func() { <-h.writeback }, nil
	case h.io <- struct{}{}:
		return h.endIO, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// tryCurrent acquires the binding's resident page without waiting for it. It
// reports the locked page, or nil with reclaiming set when something else holds
// it. A seal is what uses it: it owns the memory region exclusively, so a reclaim is the
// only thing that can hold one of its pages, and a reclaim ends with the page
// nonresident and its bytes in the page's own reservation.
func (h *Host) tryCurrent(b *binding) (pg *resident, reclaiming bool) {
	for {
		h.mu.Lock()
		pg = b.resident
		h.mu.Unlock()
		if pg == nil {
			return nil, false
		}
		if !pg.mu.TryLock() {
			return nil, true
		}
		h.mu.Lock()
		same := b.resident == pg
		h.mu.Unlock()
		if same {
			return pg, false
		}
		h.unlock(pg)
	}
}

// current acquires the page lock without holding the host lock, then validates
// that eviction did not replace the binding while the caller waited.
func (h *Host) current(ctx context.Context, b *binding) (*resident, error) {
	for {
		h.mu.Lock()
		pg := b.resident
		h.mu.Unlock()
		if pg == nil {
			return nil, nil
		}
		if err := pg.mu.Lock(ctx); err != nil {
			return nil, err
		}
		h.mu.Lock()
		same := b.resident == pg
		h.mu.Unlock()
		if same {
			return pg, nil
		}
		h.unlock(pg)
	}
}
