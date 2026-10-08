package vmmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/latency"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
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
	// prefetchRunning counts this memory region's prefetches whose goroutines
	// have not ended. It is guarded by Host.mu; a detach waits for it to reach
	// zero. See prefetch.go.
	prefetchRunning int
	// history is the windows of this memory region's latest faults, which
	// tell a guest reading forwards from one reading at random.
	history faultHistory
	// reads is the page source the READ requests of this memory region's
	// faults go to. See pagerequests.go.
	reads   *requestSource
	host    *Host
	backing Backing
	// kind is what this memory region is to its guest, RAM or PMEM. Nothing about a
	// fault, a seal or a page depends on it: it is what the host's sharing
	// gauges are split by, and it is immutable for the memory region's life.
	kind MemoryRegionKind
	// peer marks a backing whose loads can return bytes no checkpoint holds, so
	// a page it serves enters this memory region as private dirty state. It decides
	// whether a fault has to reserve against the dirty budget before it loads.
	peer    bool
	mapping Mapping
	// tenant is the tenant the memory region's VM belongs to, as its host
	// stated it. Every identity its backing reports names it.
	tenant string
	// private is this memory region's own file in an isolated arena, which
	// only its process is given and only it may map writable, shared its
	// tenant's shared file, and public the file of the pages of public
	// templates. It may only read the last two. All three are nil in a shared
	// arena, whose one file is every memory region's. forks is the number each
	// fork point's file it maps from was given under, guarded by Host.mu.
	private *arenaFile
	shared  *arenaFile
	public  *arenaFile
	forks   map[*arenaFile]int
	// filesMu admits one fork point's file at a time to the process, so that no
	// map names a file before the process holds it.
	filesMu *ctxsync.Mutex
	// ended is closed when the memory region becomes terminal, which is how
	// its session learns of an end another memory region's fault found.
	ended     chan struct{}
	pageCount int
	// layer is the region's own pages: Zircon's VmObjectPaged over a
	// VmCowPages, pages, whose page source is the region's own (reads) and
	// whose lookup falls through to the identity root resolver names.
	layer    *zirconvm.ObjectPaged
	pages    *zirconvm.CowPages
	resolver *rootResolver
	// audit is what the mapping commands installed for each page, which the
	// package's tests check the pager's decisions against; nil in production
	// (mappingaudit.go).
	audit *mappingAudit
	// bindingsMu guards beside, the bindings beside the layer, the dirty set
	// and its age, and the cold copies.
	bindingsMu sync.Mutex
	// beside is the page list of the bindings beside the layer: a slot holds
	// the binding of a page the region maps or maps from, and an Untracked
	// zero interval a compressed zero run, pages mapped to zero with no
	// binding at all.
	beside *zirconvm.PageList[binding]
	// dirtySet is every page the region may store into where it is: its own
	// Dirty state, which no checkpoint holds, and which the next seal takes
	// whole. dirtyRuns is the same set less the pages the region does not
	// map, held as runs, which is what a seal's pause write-protects; every
	// transition that changes whether a page is one of those keeps it
	// (noteSealableLocked). dirtySince is when the oldest write the region
	// holds that no checkpoint covers was made, zero while it holds none: the
	// loss window's bookkeeping.
	dirtySet   map[uint64]*binding
	dirtyRuns  pageRuns
	dirtySince time.Time
	// journal is the durable flush's state: which pages' bytes may differ
	// from the region's last journal entry, and digests of what that entry
	// holds (journal.go).
	journal regionJournal
	// coldPages is every cold copy of the region, and coldCopies the ones its
	// session's worker has not taken yet (cold.go).
	coldPages  map[uint64]*binding
	coldCopies map[uint64]struct{}
	// windowMu guards windowAsked.
	windowMu sync.Mutex
	// changes is Config.MeasureChanges's state, empty unless it is on.
	changes changes
	// coldCopied wakes the worker of this memory region's session that gives
	// its cold copies back. See cold.go.
	coldCopied chan struct{}
	// windowAsked is set once a store has asked for the checkpoint that ends
	// this memory region's window, so the stores after it do not ask again. The
	// checkpoint that seals the window, and one that gives it back, clear it.
	windowAsked bool
	terminal    atomic.Pointer[failure]
	// pressed is set by the first mapping command this memory region's process
	// refused for want of mapping budget, and closes gaps from then on; see
	// rules.go.
	pressed atomic.Bool
	// repeats paces this memory region's repeated faults; see repeats.go.
	repeats repeatBudget
	// guestFaults counts the faults the guest waited for; see guestfaults.go.
	guestFaults faultTally
	// heldReported marks the one line this memory region's unreclaimable pages are
	// worth; see heldPages.
	heldReported atomic.Bool
	// detaching marks a memory region a detach has begun on, set before it
	// asks for live. An eviction takes no page of its own layer from then on
	// (usableVictimLocked): it would have to hold live shared to write the
	// page's bytes to the region's reservations, and that is not to be had.
	detaching atomic.Bool
	// unwindowed marks a memory region its owner holds to no loss window; see
	// HoldToNoWindow.
	unwindowed atomic.Bool
	closed     bool
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
	if err := r.giveFiles(ctx); err != nil {
		return r, err
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
	// An ephemeral disk's pages are its only copy, so it attaches to the pager
	// built for them and to no other, and that pager maps nothing else: a disk
	// some checkpoint holds would be sealed by nothing there.
	if h.cfg.Ephemeral && memoryRegion.Kind != Pmem {
		return nil, fmt.Errorf("%w: an ephemeral pager maps only PMEM, not %s", ErrConfig, memoryRegion.Kind)
	}
	if states, ok := backing.(EphemeralBacking); ok && states.Ephemeral() != h.cfg.Ephemeral {
		return nil, fmt.Errorf("%w: the volume's ephemeral is %v and this pager's is %v",
			ErrConfig, states.Ephemeral(), h.cfg.Ephemeral)
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
	// The logical pages are taken under that hold, and each later hold gives
	// them back or adds the region; nothing else read under it is relied on.
	// A pager that fails in between is found by the region's first fault
	// (serving), and Close refuses while the pages are counted. Until the
	// last hold the region is in no list: its files count it under holds of
	// their own (newFiles), and no budget is relieved from one holding none.
	if err := backing.Verify(ctx); err != nil {
		h.mu.Lock()
		h.logical -= int(count)
		h.mu.Unlock()
		return nil, err
	}
	_, peer := backing.(UnpublishedLoader)
	r := &MemoryRegion{live: ctxsync.NewRWMutex(), mu: ctxsync.NewRWMutex(), endMu: ctxsync.NewMutex(), protectMu: ctxsync.NewRWMutex(), filesMu: ctxsync.NewMutex(), ended: make(chan struct{}), coldCopied: make(chan struct{}, 1), host: h, backing: backing, kind: memoryRegion.Kind, peer: peer, mapping: mapping, tenant: memoryRegion.Tenant, pageCount: int(count), readAheadPages: h.cfg.ReadAheadPages}
	if h.isolated() {
		if err := h.newFiles(ctx, r); err != nil {
			h.mu.Lock()
			h.logical -= int(count)
			h.mu.Unlock()
			return nil, err
		}
	}

	windows := (r.pageCount + r.readAheadPages - 1) / r.readAheadPages
	r.stripes = make([]*ctxsync.Mutex, min(windows, 1024))
	for i := range r.stripes {
		r.stripes[i] = ctxsync.NewMutex()
	}
	// A memory region's own pages are a region layer of its own, however it
	// attaches: here, or a session's Connect.
	if err := r.newLayer(); err != nil {
		h.mu.Lock()
		h.logical -= int(count)
		h.forgetFilesLocked(r)
		h.mu.Unlock()
		return nil, err
	}

	// The dirty budget is shared, so relieving it is a choice among all the
	// memory regions that hold it, not only the one whose store is waiting.
	h.mu.Lock()
	h.memoryRegions[r] = struct{}{}
	h.mu.Unlock()
	return r, nil
}

// newLayer makes the region's layer, which admit does for every region however
// it attaches. A migration destination's peer backing, whose loads can return
// bytes no checkpoint holds, takes a page it serves as the region's own dirty
// state (peer.go).
func (r *MemoryRegion) newLayer() error {
	ps := r.host.pageSize
	r.beside, r.dirtyRuns = zirconvm.NewPageList[binding](ps), newPageRuns(ps)
	r.journal.unjournaled = newPageRuns(ps)
	r.resolver = &rootResolver{region: r}
	r.audit = newMappingAudit()
	// The layer's source is the region's own, and it traps dirty
	// transitions, as a VMO whose pager tracks its writes does: a page of
	// the layer becomes Dirty only when the pager says so (DirtyPages), which
	// a store does once it holds the page's dirty reservation.
	proxy := zirconvm.NewPagerProxy(true)
	r.reads = &requestSource{source: zirconvm.NewPageSource(proxy), proxy: proxy}
	layer, err := zirconvm.CreateRegionLayer(r.host.node, r.reads.source, uint64(r.pageCount)*ps, r.resolver)
	if err != nil {
		return err
	}
	r.layer, r.pages = layer, layer.CowPages()
	return nil
}

// inTenant refuses a checkpoint of a tenant other than this memory region's.
// A page's identity is the checkpoint that published it, whose VM names the
// tenant, and the sharing index is keyed by identity. So an identity of
// another tenant is the one way a memory region could be handed a resident
// page of that tenant, and the pager fails the fault rather than index it.
func (r *MemoryRegion) inTenant(ref control.Ref) error {
	if ref.IsZero() || control.TenantOf(ref.VM) == r.tenant {
		return nil
	}
	return fmt.Errorf("%w: a %s memory region of tenant %q was given checkpoint %s",
		ErrOtherTenant, r.kind, r.tenant, ref)
}

// mayRead refuses a checkpoint this memory region may not read: one of another
// tenant, unless it is a public template's (control.Public). A public page is
// loaded into the public file, never a tenant's, so reading one hands no tenant
// another's page. Naming pages is inTenant's alone: nothing of a memory region
// is ever named under a public identity.
func (r *MemoryRegion) mayRead(ref control.Ref) error {
	if control.Public(ref.VM) {
		return nil
	}
	return r.inTenant(ref)
}

// ready reports whether this memory region may still use its volume. serving is the
// weaker question a peer server asks: a handed-off memory region answers for its own
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

// OnInterval reports a memory region the host's interval checkpoints: a disk
// some checkpoint holds. It is what the loss window measures, what a flush
// waits on and what a checkpoint out of the interval's turn relieves. RAM is
// published only by a capture and an ephemeral disk by nothing, so none of the
// three applies to either.
func (r *MemoryRegion) OnInterval() bool { return r.kind == Pmem && !r.Ephemeral() }

// HoldToNoWindow exempts this memory region's stores from the loss window,
// however old its oldest unpublished write. Its owner calls it for a VM that
// asked for no interval checkpoints, whose writes only a stop, a move or the
// dirty budget ever publishes.
func (r *MemoryRegion) HoldToNoWindow() { r.unwindowed.Store(true) }

// Ephemeral reports an ephemeral disk: a memory region no checkpoint holds,
// whose seal takes nothing. See Config.Ephemeral.
func (r *MemoryRegion) Ephemeral() bool { return r.host.cfg.Ephemeral }

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
	if r.terminal.CompareAndSwap(nil, &failure{fmt.Errorf("managed mapping terminal: %w", err)}) {
		close(r.ended)
	}
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

// unlock gives the region up from an exclusive hold, a seal, retire,
// unseal, handoff or detach, once every page's mapping agrees with its
// binding (mappingaudit.go).
func (r *MemoryRegion) unlock() {
	r.agree(0, uint64(r.pageCount))
	r.mu.Unlock()
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
func (r *MemoryRegion) mapPages(ctx context.Context, run MapRun, writable bool) error {
	start := r.host.clock.Now()
	err := r.mapping.Map(ctx, run.Page, run.File, run.Slot, run.Count, writable)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	if r.audit != nil {
		if writable {
			r.audited(err, run.Page, run.Count, installedWritable, "map writable")
		} else {
			r.audited(err, run.Page, run.Count, installedReadOnly, "map read-only, "+r.describeBinding(run.Page))
		}
	}
	return err
}

func (r *MemoryRegion) mapZeroPages(ctx context.Context, page uint64, count int) error {
	start := r.host.clock.Now()
	err := r.mapping.MapZero(ctx, page, count)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	r.audited(err, page, count, installedReadOnly, "map zero")
	return err
}

func (r *MemoryRegion) mapBatch(ctx context.Context, batch BatchMapping, runs []MapRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, mappingRuns, err := batch.MapBatch(ctx, runs)
	r.host.mappingLatency.Observe(r.host.clock.Since(start))
	for _, run := range runs {
		r.audited(err, run.Page, run.Count, installedReadOnly, "map batch")
	}
	return commands, mappingRuns, err
}

// audited records what a command left the pages [first, first+count) as:
// state where it landed; nothing changed where it was refused, which the
// pages' histories still name; and unknown after any other failure.
func (r *MemoryRegion) audited(err error, first uint64, count int, state installed, what string) {
	if r.audit == nil {
		return
	}
	switch {
	case err == nil:
		r.audit.installed(first, count, state, what)
	case errors.Is(err, ErrMappingRefused):
		r.audit.refused(first, count, what)
	default:
		r.audit.fail()
	}
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
	r.audited(err, page, 1, notInstalled, "revoke")
	return err
}

func (r *MemoryRegion) revokeBatch(ctx context.Context, batch BatchRevocation, runs []PageRun) (int, int, error) {
	start := r.host.clock.Now()
	commands, revokedRuns, err := batch.RevokeBatch(ctx, runs)
	r.host.revokeLatency.Observe(r.host.clock.Since(start))
	for _, run := range runs {
		r.audited(err, run.Page, run.Count, notInstalled, "revoke batch")
	}
	return commands, revokedRuns, err
}

func (r *MemoryRegion) resolvePages(ctx context.Context, page uint64, count int, writable bool) error {
	r.audit.resolved(page, count, writable)
	start := r.host.clock.Now()
	err := r.mapping.Resolve(ctx, page, count, writable)
	r.host.resolveLatency.Observe(r.host.clock.Since(start))
	if err != nil && !errors.Is(err, ErrMappingRefused) {
		r.audit.fail()
	}
	return err
}

func (r *MemoryRegion) protectPages(ctx context.Context, page uint64, count int) error {
	r.audit.protecting(page, count)
	start := r.host.clock.Now()
	err := r.mapping.Protect(ctx, page, count)
	r.host.protectLatency.Observe(r.host.clock.Since(start))
	if err == nil {
		r.audit.protected(page, count)
	} else if !errors.Is(err, ErrMappingRefused) {
		r.audit.fail()
	}
	return err
}

// loadBacking is the memory region's only backing read, timed the same way. It reports
// which of the pages it filled hold bytes the volume itself does not have, which
// only a backing that fetches from somewhere else — a migration destination's
// peer backing — ever does; for every other backing the result is nil.
func (r *MemoryRegion) loadBacking(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	return r.loadBackingInto(ctx, offset, dst, &r.host.loadLatency)
}

// loadBackingInto is loadBacking timed into histogram: a prefetch's reads are
// kept apart from the reads a fault waits on.
func (r *MemoryRegion) loadBackingInto(ctx context.Context, offset uint64, dst []byte,
	histogram *latency.Histogram) ([]bool, error) {
	start := r.host.clock.Now()
	var unpublished []bool
	var err error
	if tracked, ok := r.backing.(UnpublishedLoader); ok {
		unpublished, err = tracked.LoadUnpublished(ctx, offset, dst)
	} else {
		err = r.backing.Load(ctx, offset, dst)
	}
	histogram.Observe(r.host.clock.Since(start))
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
	if err := lockAdmitted(ctx, "vmmemory/region", r.mu.TryRLock, r.mu.RLock, r.mu.RUnlock); err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		r.mu.RUnlock()
		return err
	}
	return nil
}

// lockAdmitted takes a lock, at once where try takes it and otherwise by
// lock, which waits. A caller that waited was woken by whoever gave the lock
// up, outside any turn of a controlled run, and that one goes on beside it:
// the caller then goes on only when the run chooses, as resource, so the two
// never race for what comes next (a free slot, a page's lock, a
// fault-injection site's next draw) in an order no seed decides. Where the
// run cancels it instead, the lock is given back.
func lockAdmitted(ctx context.Context, resource string, try func() bool, lock func(context.Context) error,
	unlock func()) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if try() {
		return nil
	}
	if err := lock(ctx); err != nil {
		return err
	}
	if err := sim.Admit(ctx, resource); err != nil {
		unlock()
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
	if lockErr := lockAdmitted(ctx, "vmmemory/region", r.mu.TryRLock, r.mu.RLock, r.mu.RUnlock); lockErr != nil {
		return errors.Join(err, lockErr, errMemoryRegionDropped)
	}
	if err != nil {
		return err
	}
	return r.ready()
}

// reclaimWith takes one slot with the memory region given up. A slot it took is
// given back if the memory region cannot be taken again, or is terminal once it
// is: the fault that wanted the slot is over, and nothing else would ever
// return it.
func (r *MemoryRegion) reclaimWith(ctx context.Context, take func() (fileSlot, error)) (fileSlot, error) {
	at := fileSlot{slot: -1}
	err := r.withoutMemoryRegion(ctx, func() error {
		taken, err := take()
		if err == nil {
			at = taken
		}
		return err
	})
	if err != nil && at.slot >= 0 {
		return fileSlot{slot: -1}, r.host.abandonSlots(context.WithoutCancel(ctx), at, 1, err)
	}
	return at, err
}

// reclaimSeam runs in a reclaim for a private page while the memory region is given
// up, which is the one window in which a seal and a retire can run inside a
// fault that has already decided what the page it is serving is. Production
// leaves it nil; a test installs one to end that page's dirty epoch there.
var reclaimSeam func(index uint64)

// histogram is what each backing read is timed into.
func (r *MemoryRegion) readRun(ctx context.Context, first uint64, wanted []bool, dst []byte,
	histogram *latency.Histogram) ([]bool, error) {
	ps := r.host.pageSize
	// A peer backing reports which pages the source still holds, which is a
	// second answer per page; it is read stretch by stretch until it can give
	// both at once.
	if sparse, ok := r.backing.(SparseLoader); ok && !r.peer {
		start := r.host.clock.Now()
		err := sparse.LoadPages(ctx, first*ps, dst, wanted)
		histogram.Observe(r.host.clock.Since(start))
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
		held, err := r.loadBackingInto(ctx, (first+uint64(at))*ps, dst[uint64(at)*ps:uint64(at+run)*ps], histogram)
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
	if err := r.countAllocated(); err != nil {
		return err
	}
	if r.handed {
		// There is no authority left to observe: the volume is another
		// host's, and this region only serves the pages it still holds.
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
//
// Its prefetches end, every page it maps is no longer mapped by it, its own
// pages go back, and its layer goes.
func (r *MemoryRegion) Detach(ctx context.Context) error {
	h := r.host
	// An eviction of one of the region's own pages holds live shared from
	// before it reads the page's reservations until its bytes are written to
	// them, so the reservations and the layer given back below are given back
	// only once no eviction is writing into them, and none starts after.
	r.detaching.Store(true)
	if err := r.live.Lock(ctx); err != nil {
		r.detaching.Store(false)
		return err
	}
	defer r.live.Unlock()
	if err := r.cancelPrefetches(ctx); err != nil {
		return err
	}
	if err := r.mu.Lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	if r.closed {
		return nil
	}
	// A sealed checkpoint goes with the region, whatever publication may still
	// be reading it: its memory users are gone, and its unpublished stores
	// with them.
	if checkpoint := r.currentCheckpoint(); checkpoint != nil {
		if err := r.discardCheckpoint(ctx, checkpoint); err != nil {
			return err
		}
		r.setCheckpoint(nil)
	}
	// Every page this region mapped from a root is mapped by it no more, and
	// one nothing else maps is idle, kept for the next region that inherits
	// its identity.
	// Its writes go with it, and their reservations back to the budget. The
	// pages they were copied from go too, where nothing else maps them.
	//
	// The bindings are read under one hold of bindingsMu and acted on after
	// it. That is safe: a binding stays in the list until the list is
	// replaced below, and nothing else adds one, since no fault runs while
	// live is held. What the list says of a binding is read again where it
	// is acted on: its page under h.mu, its origin by dropOrigin.
	r.bindingsMu.Lock()
	var bound []*binding
	var origins []*zirconvm.VmPage
	r.eachBoundLocked(0, uint64(r.pageCount), func(b *binding) {
		bound = append(bound, b)
		if b.origin != nil {
			origins = append(origins, b.origin)
		}
	})
	r.bindingsMu.Unlock()
	r.releaseDirty()
	h.mu.Lock()
	for _, b := range bound {
		if b.page != nil {
			r.host.unaliasLocked(b)
		}
	}
	h.mu.Unlock()
	// Between the hold above and the one below, a fault of another region
	// may count this one among the attached: it holds nothing and maps
	// nothing now, so it asks no checkpoint of it and is no victim's alias.
	for _, origin := range origins {
		r.host.dropOrigin(origin)
	}
	// The region counts as attached until its layer's pages are back: Close
	// refuses a pager with a region attached, and one that closed now would
	// release its files under the pages the layer is about to free into them.
	early := sim.Bug(ctx, "pager-count-detached-before-its-pages-go")
	h.mu.Lock()
	if early {
		h.logical -= r.pageCount
	}
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
	// request to end. Its files and its logical pages are its own until the
	// hold below: Close refuses while they are counted, and no other region
	// gives back a file this one still holds.
	r.layer.Destroy()
	h.mu.Lock()
	h.forgetFilesLocked(r)
	if !early {
		h.logical -= r.pageCount
		h.signal()
	}
	h.mu.Unlock()
	r.bindingsMu.Lock()
	r.beside = zirconvm.NewPageList[binding](h.pageSize)
	r.bindingsMu.Unlock()
	r.pageCount = 0
	return nil
}
