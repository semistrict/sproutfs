package vmmemory

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/resource"
	"github.com/semistrict/sproutfs/internal/vmmemory/internal/latency"
	"github.com/semistrict/sproutfs/internal/vmmemory/internal/slots"
)

// Host accounts for shared backing under a short metadata lock. Each Region
// orders faults per read-ahead window and flushes for the whole region;
// resident pages independently serialize alias changes and reclamation.
// Storage and mapping I/O never hold the host lock. Recency tracks faults and
// read-ahead, not accesses through already present PTEs.
type Host struct {
	// pageSize is this pager's unit, fixed by its configuration: its arena
	// slots, its spill slots, the numbers it faults and serves, and the
	// alignment every region it maps must have. Nothing here is ever the other
	// pager's page, and no count of these pages may be added to one of those.
	pageSize       uint64
	resources      *resource.Budget
	residentLeases []*resource.Lease
	spillWritten   []bool
	// spillSum is the checksum each written reservation's bytes must have when
	// they come back. It is this process's own authority over a scratch file.
	spillSum []uint32
	mu       sync.Mutex
	cfg      Config
	// clock times the fault path. It is Config.Clock, or the wall clock.
	clock        platform.Clock
	arena        Arena
	spill        platform.File
	slots        *slots.Set
	freeSpill    []int
	clean        map[pageKey]*resident
	cleanVersion uint64
	// regions is every attached region, which is what the dirty budget's
	// pressure is measured and acted on across: the budget is the host's, so
	// the checkpoint that relieves it need not be the waiting region's.
	regions map[*Region]struct{}
	// pressure is who to ask for that checkpoint, highWater the dirty occupancy
	// at which the host asks without waiting to be empty, and asked whether it
	// has already asked since the budget last fell below that mark.
	pressure    Pressure
	highWater   int
	asked       bool
	zeroRegions int // attached regions retaining knowledge of explicit zeros
	lru         list.List
	logical     int
	dirty       int
	changed     chan struct{}
	io          chan struct{}
	writeback   chan struct{}
	err         error
	stats       Stats
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
}

// The byte bounds a page count is checked against, independently of the pager
// geometry: the largest run one fault may hold a buffer for, and the window a
// population walks metadata in.
const (
	maximumReadAheadBytes = 16 << 20
	populationWindowBytes = 256 << 20
)

// New uses a dedicated scratch spill file. It is not crash recovery metadata
// and must not be shared with another Host, which includes the other pager of
// the same host: each owns its arena and its spill file alone. The file's
// maximum size is DirtyPages times this pager's page; acknowledged durability
// always goes through Backing, never spill. The caller retains ownership of
// Arena and spill until every Region detaches.
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
		uint64(cfg.LogicalPages) > math.MaxInt64/pageSize || arena == nil || spill == nil {
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
	h := &Host{pageSize: pageSize, cfg: cfg, clock: platform.ClockOr(cfg.Clock), arena: arena, spill: spill, resources: resources, residentLeases: make([]*resource.Lease, cfg.ResidentPages), spillWritten: make([]bool, cfg.DirtyPages),
		spillSum: make([]uint32, cfg.DirtyPages),
		slots:    slots.New(cfg.ResidentPages),
		clean:    make(map[pageKey]*resident), changed: make(chan struct{}),
		regions: make(map[*Region]struct{}), highWater: highWater(cfg.DirtyPages),
		io: make(chan struct{}, cfg.ConcurrentIO), writeback: make(chan struct{}, 1)}
	for i := cfg.DirtyPages - 1; i >= 0; i-- {
		h.freeSpill = append(h.freeSpill, i)
	}
	return h, nil
}

// Resources returns the same host-wide budget used for resident pages.
func (h *Host) Resources() *resource.Budget { return h.resources }

// PageSize is this pager's unit. A region's size and a volume's page must be a
// multiple of it and equal to it respectively, and every page count this pager
// reports is counted in it — which is why a caller adding two pagers' numbers
// must convert to bytes first.
func (h *Host) PageSize() uint64 { return h.pageSize }

// LogicalHeadroom is how many more logical pages this pager would still admit.
// A region larger than this is refused at attachment, which is a VMM that has
// already started and a guest that has to be killed, so the thing that decides
// to run a VM asks this before it starts one.
func (h *Host) LogicalHeadroom() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.LogicalPages - h.logical
}

// Close releases any unpublished arena allocations left by failed cleanup.
// All regions must already be detached. The caller still owns the arena and
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
	var result error
	for slot, lease := range h.residentLeases {
		if lease == nil {
			continue
		}
		if err := h.arena.Release(ctx, slot); err != nil {
			result = errors.Join(result, err)
			continue
		}
		h.putFree(slot)
	}
	if err := h.spill.Truncate(ctx, 0); err != nil {
		result = errors.Join(result, err)
	} else {
		clear(h.spillWritten)
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

func (h *Host) unlock(pg *resident) {
	pg.mu.Unlock()
	h.mu.Lock()
	h.signal()
	h.mu.Unlock()
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
// it. A seal is what uses it: it owns the region exclusively, so a reclaim is the
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
