package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory/internal/pageranges"
)

type failure struct{ err error }

type MemoryRegion struct {
	// live is held shared for the whole of a fault and exclusively by detach
	// alone. It is what keeps this memory region's bindings and pages in existence
	// across the part of a fault that holds no memory region lock — its backing read —
	// so that giving mu up there cannot race a teardown.
	live *ctxsync.RWMutex
	// mu is held shared by a fault's planning, metadata and page-table work and
	// exclusively by seal, retire, unseal, handoff and detach. A fault gives it
	// up across its backing read: a seal is a vCPU pause, and it must wait for
	// page-table work and never for bytes. stripes serialize faults within one
	// read-ahead window, and go on owning a window while its read runs.
	mu             *ctxsync.RWMutex
	stripes        []*ctxsync.Mutex
	readAheadPages int
	host           *Host
	backing        Backing
	// kind is what this memory region is to its guest, RAM or PMEM. Nothing about a
	// fault, a seal or a page depends on it: it is what the host's sharing
	// gauges are split by, and it is immutable for the memory region's life.
	kind MemoryRegionKind
	// peer marks a backing whose loads can return bytes no checkpoint holds, so
	// a page it serves enters this memory region as private dirty state. It decides
	// whether a fault has to reserve against the dirty budget before it loads.
	peer       bool
	mapping    Mapping
	pageCount  int
	bindingsMu sync.Mutex
	// changes is Config.MeasureChanges's state, empty unless it is on.
	changes       changes
	blocks        map[uint64]*bindingBlock
	zeroRanges    pageranges.Map
	dirtyBindings map[uint64]*binding
	// dirtyRuns is the same set as dirtyBindings less the pages no mapping of
	// this memory region covers, held as runs rather than as pages: it is what a seal
	// write-protects, and reading it is how a pause costs its commands rather
	// than the pages they cover. It is maintained by every transition that
	// changes whether a page is one the next seal would protect, all of which
	// hold bindingsMu.
	dirtyRuns pageranges.Map
	// dirtySince is when the oldest write this memory region holds that no checkpoint
	// covers landed, zero while it holds none. It is the loss window's own
	// bookkeeping and is guarded by bindingsMu, because the transitions that
	// put a page into the dirty set and take it out again are the transitions
	// that start and end it.
	dirtySince time.Time
	terminal   atomic.Pointer[failure]
	// pressed is set by the first mapping command this memory region's process
	// refused for want of mapping budget, and closes gaps from then on; see
	// rules.go.
	pressed atomic.Bool
	// heldReported marks the one line this memory region's unreclaimable pages are
	// worth; see heldPages.
	heldReported atomic.Bool
	closed       bool
	// handed marks a memory region whose volume belongs to another host now. It is
	// set and read under the memory region lock, like closed.
	handed   bool
	hasZeros bool // protected by Host.mu; contributes one zeroMemoryRegions reference
	// resident is how many resident pages this memory region's bindings map,
	// and wanted the pager's count of displaced pages when it last asked for a
	// page. Both
	// are protected by Host.mu, like the alias sets resident counts. An
	// eviction reads them to leave a memory region within its share of the
	// arena its pages: see Host.protectedLocked.
	resident int
	wanted   uint64
	// stopping marks a memory region whose owner has agreed to stop its VM for a
	// bound nothing else could relieve. It is protected by Host.mu. The owner is
	// asked once, and the pages the memory region holds come back when it
	// detaches, which is what a store waiting on the dirty budget waits for.
	stopping bool
	// endMu admits one retire or unseal at a time. The walk gives the memory region up
	// between batches, so the exclusive memory region lock is no longer what keeps two
	// of them apart.
	endMu *ctxsync.Mutex
	// protectMu keeps a seal's write-protect commands apart from the one thing
	// that can take a mapping away while the seal holds the memory region: a reclaim,
	// which revokes its victim's pages under that page's lock alone. The seal no
	// longer holds those locks — it reads the runs of the dirty set rather than
	// its pages — so this is what says that every revocation is either finished,
	// and out of the runs it reads, or has not begun. Revocations hold it
	// shared, the protection exclusively, and nothing holds it and then waits
	// for the memory region or for a page.
	protectMu *ctxsync.RWMutex
	// checkpointMu protects the pointer only; the checkpoint it names is immutable
	// from the seal that took it until the publication or the unseal that retires
	// it.
	checkpointMu sync.Mutex
	checkpoint   *MemoryRegionCheckpoint
	// populated is what this memory region's attach populate installed, written once
	// before its guest runs and read afterwards by whoever accounts for a
	// restore's phases.
	populated atomic.Pointer[PopulateStats]
	// sealing is set while a seal is taking the dirty set into a checkpoint
	// the memory region does not name yet, so a store asking what will relieve the
	// dirty budget at that moment is told a checkpoint is coming rather than
	// that nothing is.
	sealing bool
}

// Attach admits metadata and verifies writer authority before exposing a memory region.
// The mapping must initially consist entirely of armed missing-fault traps.
// Equal page identities share resident pages in this pager. A caller must
// not attach the same writable volume to two memory regions. Once the mapping can
// accept commands, Populate maps everything already resident.
//
// The backing carries the memory region's kind, which the caller states: a pager that
// guessed it from a volume's name would report memory as disk the first time a
// deployment named a volume something else.
func (h *Host) Attach(ctx context.Context, backing MemoryRegionBacking, mapping Mapping) (*MemoryRegion, error) {
	r, err := h.admit(ctx, backing, mapping)
	if err != nil {
		return nil, err
	}
	if err := r.Populate(ctx); err != nil {
		// Return the retained memory region on ambiguous mapping failure. Its owner
		// must stop memory users before detaching it.
		return r, err
	}
	return r, nil
}

func (h *Host) admit(ctx context.Context, memoryRegion MemoryRegionBacking, mapping Mapping) (*MemoryRegion, error) {
	backing := memoryRegion.Backing
	if backing == nil || mapping == nil || (memoryRegion.Kind != Pmem && memoryRegion.Kind != Ram) {
		return nil, ErrConfig
	}
	size := backing.Size()
	if size == 0 || size%h.pageSize != 0 {
		return nil, ErrConfig
	}
	// A volume published in another page size cannot be served here at all: a
	// page number of it means something else, so it is refused before a mapping
	// is armed rather than faulted in the wrong unit. It is also how a memory region
	// reaches the wrong one of a host's two pagers — RAM's volume attached to
	// the PMEM pager is exactly this mismatch.
	if paged, states := backing.(PagedBacking); states && paged.PageSize() != h.pageSize {
		return nil, fmt.Errorf("%w: the volume is published in %d-byte pages, this pager's page is %d",
			ErrConfig, paged.PageSize(), h.pageSize)
	}
	count := size / h.pageSize
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return nil, err
	}
	if count > uint64(h.cfg.LogicalPages-h.logical) {
		h.mu.Unlock()
		return nil, ErrCapacity
	}
	h.logical += int(count)
	h.mu.Unlock()
	if err := backing.Verify(ctx); err != nil {
		h.mu.Lock()
		h.logical -= int(count)
		h.mu.Unlock()
		return nil, err
	}
	_, peer := backing.(UnpublishedLoader)
	r := &MemoryRegion{live: ctxsync.NewRWMutex(), mu: ctxsync.NewRWMutex(), endMu: ctxsync.NewMutex(), protectMu: ctxsync.NewRWMutex(), host: h, backing: backing, kind: memoryRegion.Kind, peer: peer, mapping: mapping, pageCount: int(count), blocks: make(map[uint64]*bindingBlock), readAheadPages: h.cfg.ReadAheadPages}

	windows := (r.pageCount + r.readAheadPages - 1) / r.readAheadPages
	r.stripes = make([]*ctxsync.Mutex, min(windows, 1024))
	for i := range r.stripes {
		r.stripes[i] = ctxsync.NewMutex()
	}
	// The dirty budget is shared, so relieving it is a choice among all the
	// memory regions that hold it, not only the one whose store is waiting.
	h.mu.Lock()
	h.memoryRegions[r] = struct{}{}
	h.mu.Unlock()
	return r, nil
}

// ready reports whether this memory region may still use its volume. serving is the
// weaker question a page server asks: a handed-off memory region answers for its own
// pages long after its volume became another host's.
// Resources identifies the host allotment backing this memory region. Supervisors use
// it to verify that every mapped memory region shares the storage host's budget.
func (r *MemoryRegion) Resources() *resource.Budget {
	if r == nil || r.host == nil {
		return nil
	}
	return r.host.Resources()
}

// PageSize is the page of the pager holding this memory region, which is the unit of
// every page number it takes and reports. A caller serving this memory region's pages
// to another host reads it here rather than from a host-wide constant: the two
// kinds of memory region are two pagers and need not agree.
func (r *MemoryRegion) PageSize() uint64 { return r.host.pageSize }

// Kind is what this memory region is to its guest, RAM or PMEM, as whoever attached it
// stated.
func (r *MemoryRegion) Kind() MemoryRegionKind { return r.kind }

func (r *MemoryRegion) ready() error {
	if err := r.serving(); err != nil {
		return err
	}
	if r.handed {
		return ErrHandedOff
	}
	return nil
}

func (r *MemoryRegion) serving() error {
	if r.closed {
		return ErrClosed
	}
	if failed := r.terminal.Load(); failed != nil {
		return failed.err
	}
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	return r.host.err
}

// fail makes this memory region terminal, which is the end of the machine that maps
// it: nothing may take a mapping away from it again, so nothing may reuse the
// pages it still holds. The session that ends with it is what says so — see
// heldPages for the part of it nothing else reports.
func (r *MemoryRegion) fail(err error) error {
	r.terminal.CompareAndSwap(nil, &failure{fmt.Errorf("managed mapping terminal: %w", err)})
	return r.terminal.Load().err
}

// heldPages says once that this memory region's pages cannot be taken back. It is
// the one part of a memory region's end that nothing else reports: the eviction that
// discovers it goes on to another victim without a word, and by then the
// session that failed may have ended minutes ago, so a host short of memory has
// no other way to learn that some of its arena belongs to a machine that is
// finished and is not coming back until the memory region is closed.
func (r *MemoryRegion) heldPages(ctx context.Context, err error) {
	if r.heldReported.Swap(true) {
		return
	}
	slog.WarnContext(ctx, "vmmemory: a memory region's pages cannot be taken back until it is closed",
		"pages", r.pageCount, "error", err)
}

func (r *MemoryRegion) window(index uint64) (start, end uint64) {
	size := uint64(r.readAheadPages)
	start = index - index%size
	return start, min(start+size, uint64(r.pageCount))
}

func (r *MemoryRegion) stripe(index uint64) *ctxsync.Mutex {
	return r.stripes[int(index/uint64(r.readAheadPages))%len(r.stripes)]
}

// The pager reaches its mapping only through these, so every command a fault
// issues is timed in one place. They add a monotonic reading and one atomic
// increment each and change nothing else: the command, its arguments, its
// error and its ordering are exactly the interface's.
func (r *MemoryRegion) mapPages(ctx context.Context, page uint64, slot, count int, writable bool) error {
	start := r.host.clock.Now()
	err := r.mapping.Map(ctx, page, slot, count, writable)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	return err
}

func (r *MemoryRegion) mapZeroPages(ctx context.Context, page uint64, count int) error {
	start := r.host.clock.Now()
	err := r.mapping.MapZero(ctx, page, count)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	return err
}

func (r *MemoryRegion) mapBatch(ctx context.Context, batch BatchMapping, runs []MapRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, mappingRuns, err := batch.MapBatch(ctx, runs)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	return commands, mappingRuns, err
}

// mappingFailed reports what a failed mapping command means for this memory region. A
// refusal is the one failure that is known to have changed nothing: the client
// admits a command against its mapping budget before it touches anything, so
// the pages are not mapped, the record the pager made of them is taken back by
// undo, and the fault fails rather than the memory region. Every other failure may
// have been applied, so that record stands — memory the guest can still read
// through must never be reachable from a binding that says it is unmapped,
// which a revocation would skip — and the memory region is terminal.
func (r *MemoryRegion) mappingFailed(err error, undo func()) error {
	if !errors.Is(err, ErrMappingRefused) {
		return r.fail(err)
	}
	undo()
	return err
}

func (r *MemoryRegion) revokePage(ctx context.Context, page uint64) error {
	start := r.host.clock.Now()
	err := r.mapping.Revoke(ctx, page)
	r.host.revokeLatency.Observe(r.host.clock.Since(start))
	return err
}

func (r *MemoryRegion) revokeBatch(ctx context.Context, batch BatchRevocation, runs []PageRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, revokedRuns, err := batch.RevokeBatch(ctx, runs)
	r.host.revokeLatency.Observe(r.host.clock.Since(start))
	return commands, revokedRuns, err
}

func (r *MemoryRegion) resolvePages(ctx context.Context, page uint64, count int, writable bool) error {
	start := r.host.clock.Now()
	err := r.mapping.Resolve(ctx, page, count, writable)
	r.host.resolveLatency.Observe(r.host.clock.Since(start))
	return err
}

func (r *MemoryRegion) protectPages(ctx context.Context, page uint64, count int) error {
	start := r.host.clock.Now()
	err := r.mapping.Protect(ctx, page, count)
	r.host.protectLatency.Observe(r.host.clock.Since(start))
	return err
}

// loadBacking is the memory region's only backing read, timed the same way. It reports
// which of the pages it filled hold bytes the volume itself does not have, which
// only a backing that fetches from somewhere else — a migration destination's
// peer backing — ever does; for every other backing the result is nil.
func (r *MemoryRegion) loadBacking(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	start := r.host.clock.Now()
	var unpublished []bool
	var err error
	if tracked, ok := r.backing.(UnpublishedLoader); ok {
		unpublished, err = tracked.LoadUnpublished(ctx, offset, dst)
	} else {
		err = r.backing.Load(ctx, offset, dst)
	}
	r.host.loadLatency.Observe(r.host.clock.Since(start))
	return unpublished, err
}

// installedUnpublished tells a backing which pages of one load this memory region now
// holds as its own dirty state. A page it loaded but did not bind is not one
// this host has: the backing keeps expecting to fetch it rather than letting a
// later read answer it from a volume whose bytes predate the guest's write.
func (r *MemoryRegion) installedUnpublished(offset uint64, installed []bool) {
	if held, ok := r.backing.(UnpublishedInstaller); ok {
		held.InstalledUnpublished(offset, installed)
	}
}

// lockPageAccess takes the shared memory region lock for a fault. Nothing here waits:
// a sealed checkpoint holds its own copies of the pages its publication is
// uploading, so the guest keeps faulting and storing for the whole of that
// upload.
func (r *MemoryRegion) lockPageAccess(ctx context.Context, index uint64, write bool) error {
	if err := r.mu.RLock(ctx); err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		r.mu.RUnlock()
		return err
	}
	return nil
}

// errMemoryRegionDropped reports a read whose memory region lock could not be retaken,
// because the fault it belongs to was cancelled while it waited. The memory region is
// not held when this is returned, and the fault that gets it releases
// everything else and reports it rather than going on.
var errMemoryRegionDropped = errors.New("managed-memory fault gave the memory region up")

// withoutMemoryRegion runs one read of bytes with the memory region lock given up, so that a
// seal, an unseal or a retire runs in full while it is in flight. The fault's
// stripe still owns this window and the plan still holds the pages it has
// taken, so nothing about the window changes meanwhile; a detach cannot run at
// all, because a fault holds the memory region live from end to end. The caller's
// invariant is that it holds the memory region shared, so the lock is retaken whatever
// the read did and what the memory region became while it ran is reported instead.
//
// Retaking it is a wait like any other: what holds it up is page-table work,
// but the seal, retire or unseal doing that work can itself be waiting on a VMM
// that is not answering, and a fault whose guest is already gone must not be
// held there. A cancelled one reports errMemoryRegionDropped, which says that the
// caller's invariant no longer holds.
func (r *MemoryRegion) withoutMemoryRegion(ctx context.Context, read func() error) error {
	r.mu.RUnlock()
	err := read()
	if lockErr := r.mu.RLock(ctx); lockErr != nil {
		return errors.Join(err, lockErr, errMemoryRegionDropped)
	}
	if err != nil {
		return err
	}
	return r.ready()
}

// reclaim takes one arena slot outside the memory region lock, reclaimNear takes one
// near the page's neighbours, and reclaimPrivate the one the placement rule
// gives a page this store is making private. Taking a slot can reclaim one,
// which revokes a victim's mappings and writes its bytes to the spill file: a
// vCPU pause must wait for neither, exactly as it must not wait for a backing
// read.
func (r *MemoryRegion) reclaim(ctx context.Context) (int, error) {
	return r.reclaimWith(ctx, func() (int, error) {
		return r.host.allocate(ctx, r, nil, sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5))
	})
}

func (r *MemoryRegion) reclaimNear(ctx context.Context, index uint64) (int, error) {
	return r.reclaimWith(ctx, func() (int, error) { return r.allocateNear(ctx, index) })
}

func (r *MemoryRegion) reclaimPrivate(ctx context.Context, index uint64) (int, error) {
	return r.reclaimWith(ctx, func() (int, error) {
		if reclaimSeam != nil {
			reclaimSeam(index)
		}
		return r.allocatePrivate(ctx, index)
	})
}

// reclaimWith takes one slot with the memory region given up. A slot it took is
// given back if the memory region cannot be taken again, or is terminal once it
// is: the fault that wanted the slot is over, and nothing else would ever
// return it.
func (r *MemoryRegion) reclaimWith(ctx context.Context, take func() (int, error)) (int, error) {
	slot := -1
	err := r.withoutMemoryRegion(ctx, func() error {
		taken, err := take()
		if err == nil {
			slot = taken
		}
		return err
	})
	if err != nil && slot >= 0 {
		return -1, r.host.abandonSlots(context.WithoutCancel(ctx), slot, 1, err)
	}
	return slot, err
}

// reclaimSeam runs in a reclaim for a private page while the memory region is given
// up, which is the one window in which a seal and a retire can run inside a
// fault that has already decided what the page it is serving is. Production
// leaves it nil; a test installs one to end that page's dirty epoch there.
var reclaimSeam func(index uint64)

// loadWindow is the plan's backing read, taken outside the memory region lock.
func (r *MemoryRegion) loadWindow(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	var unpublished []bool
	err := r.withoutMemoryRegion(ctx, func() error {
		var err error
		unpublished, err = r.loadBacking(ctx, offset, dst)
		return err
	})
	if err != nil {
		return nil, err
	}
	return unpublished, nil
}

// loadRun is one fault's whole backing read, taken outside the memory region lock: the
// pages of [first, first+len(wanted)) that wanted marks, into dst, which covers
// the run whole. The pages it leaves out are the ones this memory region already holds
// — nothing is read for them and the bytes of dst they cover are untouched.
//
// A backing that can be asked for part of a range is asked once, so what the
// run costs is what the volume makes of it. Every other backing is read one
// stretch of wanted pages at a time, which is what a fault used to cost for
// every backing: a request per stretch, and a window's resident pages are what
// cut it into stretches.
func (r *MemoryRegion) loadRun(ctx context.Context, first uint64, wanted []bool, dst []byte) ([]bool, error) {
	var unpublished []bool
	err := r.withoutMemoryRegion(ctx, func() error {
		var err error
		unpublished, err = r.readRun(ctx, first, wanted, dst)
		return err
	})
	if err != nil {
		return nil, err
	}
	return unpublished, nil
}

func (r *MemoryRegion) readRun(ctx context.Context, first uint64, wanted []bool, dst []byte) ([]bool, error) {
	ps := r.host.pageSize
	// A peer backing reports which pages the source still holds, which is a
	// second answer per page; it is read stretch by stretch until it can give
	// both at once.
	if sparse, ok := r.backing.(SparseLoader); ok && !r.peer {
		start := r.host.clock.Now()
		err := sparse.LoadPages(ctx, first*ps, dst, wanted)
		r.host.loadLatency.Observe(r.host.clock.Since(start))
		return nil, err
	}
	var unpublished []bool
	for at := 0; at < len(wanted); {
		if !wanted[at] {
			at++
			continue
		}
		run := 1
		for at+run < len(wanted) && wanted[at+run] {
			run++
		}
		held, err := r.loadBacking(ctx, (first+uint64(at))*ps, dst[uint64(at)*ps:uint64(at+run)*ps])
		if err != nil {
			return nil, err
		}
		if len(held) > 0 {
			if unpublished == nil {
				unpublished = make([]bool, len(wanted))
			}
			copy(unpublished[at:], held)
		}
		at += run
	}
	return unpublished, nil
}

// readForCopy fills a store's private copy with the page's current bytes, and
// reports whether those bytes are ones no checkpoint of this VM has. Bytes that
// come from the backing are read outside the memory region lock; a resident page, a
// spill slot or a checkpoint's copy is this host's own and is read in place, and
// so is the page a store read in to copy away from, which the caller supplies
// as pg without binding it to anything.
//
// Only the backing read can answer unpublished, and only a backing that fetches
// from another host ever says yes: the store is then this memory region taking a page
// that existed nowhere but there, which the caller reports installed once the
// page is bound. Everything this host already holds is already its own.
func (r *MemoryRegion) readForCopy(ctx context.Context, b *binding, pg *resident, dst []byte) (unpublished bool, err error) {
	if pg != nil || b.zero || b.dirty {
		return false, r.host.read(ctx, b, pg, dst)
	}
	err = r.withoutMemoryRegion(ctx, func() error {
		fetched, err := r.loadBacking(ctx, b.index*r.host.pageSize, dst)
		unpublished = len(fetched) > 0 && fetched[0]
		return err
	})
	return unpublished, err
}

// Verify checks writer authority even when cached accesses never fault. It runs
// Backing.Verify, which confirms that this host still owns the VM. The
// supervisor must use a deadline and stop the VM on failure. It observes
// authority at the call; it is not an expiring execution or network lease.
// Faults continue while it runs. A memory region that has handed its volume off has no
// authority to observe and reports success without touching it.
func (r *MemoryRegion) Verify(ctx context.Context) error {
	if err := r.mu.RLock(ctx); err != nil {
		return err
	}
	defer r.mu.RUnlock()
	if err := r.serving(); err != nil {
		return err
	}
	if r.handed {
		// There is no authority left to observe: the volume is another host's,
		// and this memory region only serves the pages it still holds.
		return nil
	}
	if err := r.backing.Verify(ctx); err != nil {
		return r.fail(err)
	}
	return nil
}

// Detach requires all memory users stopped and KVM slots unregistered (or the
// process exited). It discards unpublished stores, including a sealed
// checkpoint a publication may still be reading, and permits reuse after failed
// mapping ACKs. The caller retains ownership of Backing and its lifetime.
func (r *MemoryRegion) Detach(ctx context.Context) error {
	// Faults in flight hold the memory region live, including across the backing reads
	// they give the memory region lock up for, so the teardown waits for them here
	// rather than meeting one halfway through.
	if err := r.live.Lock(ctx); err != nil {
		return err
	}
	defer r.live.Unlock()
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	h := r.host
	for _, b := range r.bindings() {
		if err := h.locked(ctx, b, func(pg *resident) error {
			b.mapped = false
			return h.unlink(ctx, b, pg)
		}); err != nil {
			return err
		}
		// A page this memory region's stores copied away from is reachable from no
		// binding but this one, so this is where it goes: nothing else would
		// ever release it, and detaching leaves no resident page behind.
		if origin := b.origin; origin != nil {
			b.origin = nil
			if err := h.releaseOrigin(ctx, origin); err != nil {
				return err
			}
		}
		b.dirty, b.checkpoint, b.ahead = false, nil, false
		if b.spillSlot >= 0 {
			h.releaseSpill(b.spillSlot)
			b.spillSlot = -1
		}
	}
	if checkpoint := r.currentCheckpoint(); checkpoint != nil {
		if err := r.discardCheckpoint(ctx, checkpoint); err != nil {
			return err
		}
		r.setCheckpoint(nil)
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
	r.blocks = nil
	r.dirtyBindings = nil
	r.zeroRanges = pageranges.Map{}
	r.dirtyRuns = pageranges.Map{}
	r.pageCount = 0
	return nil
}
