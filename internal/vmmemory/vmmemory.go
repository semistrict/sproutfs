// Package vmmemory manages bounded shared backing for mapped volume regions.
// The Linux adapter implements mapping changes; the same ownership machine is
// exercised with simulated mappings and storage in ordinary Go tests.
//
// Resident pages are keyed by the page identity the volume reports for a
// page: every page has one name, the checkpoint that published it, and two VMs
// share a resident page because they inherited the same checkpoint object.
package vmmemory

import (
	"context"
	"errors"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// PageSize is the production pager unit, backed by explicit 2 MiB HugeTLB pages.
const PageSize = 2 << 20

// RegionKind is what one region is to the guest that maps it. The pager treats
// the two alike — same arena, same page, same ownership — and reports them
// apart, because what a deployment plans for is RAM and disk separately. It is
// set by whoever attaches the region, which is the only party that knows: a
// volume's name says nothing, and the pager must not read one.
type RegionKind uint64

const (
	Pmem RegionKind = 1
	Ram  RegionKind = 2
)

// RegionBacking is the one region a session maps: what it is to the guest and
// the bytes it stands in front of.
type RegionBacking struct {
	Kind    RegionKind
	Backing Backing
}

var (
	ErrConfig   = errors.New("invalid managed-memory configuration")
	ErrCapacity = errors.New("managed-memory capacity exhausted")
	ErrClosed   = errors.New("managed-memory region closed")
	ErrRange    = errors.New("invalid managed-memory page")
	// ErrContended reports a faulting page whose identity kept losing
	// publication races. It is retryable and never observed in practice.
	ErrContended = errors.New("managed-memory page could not be resolved")
	// ErrMappingRefused reports a mapping command the client refused: it checks
	// what a command would cost its mapping budget before it touches anything,
	// so a refusal is the one failure that is known to have changed nothing.
	// Nothing about the client's mappings moved, which makes it a failed
	// operation rather than a failed session — the pages are not mapped, the
	// region goes on serving, and the fault is served again once the budget it
	// ran out of has been freed. Every other mapping failure is ambiguous: the
	// command may have been applied, and the region is terminal.
	ErrMappingRefused = errors.New("managed-memory mapping refused")
	// ErrDirtyStalled reports a store the dirty budget cannot admit and no
	// checkpoint can make room for. It is deliberately not ErrCapacity: a store
	// that only has to wait waits, and this is the one case left, which the
	// region's owner is told about through Pressure.Stop so that it stops the VM
	// itself rather than letting a failed fault kill the VMM.
	ErrDirtyStalled = errors.New("managed-memory dirty budget stalled")
	// ErrWindowStalled reports a store held back by the loss window whose VM can
	// never be checkpointed: its loop is off, or its region belongs to no VM this
	// host runs. It is told apart from a budget stall because the two say
	// different things about a deployment — one that its guests dirty faster than
	// their checkpoints drain, this one that a guest's writes cannot be made
	// durable at all — and it ends the same way, with a deliberate stop.
	ErrWindowStalled = errors.New("managed-memory loss window stalled")
)

// Pressure is how the pager pushes a full dirty budget back to whoever owns its
// regions, which is the only party that can checkpoint one or stop its VM. A
// host with no Pressure installed can do neither, so every store past the
// budget stalls.
//
// Both callbacks run on the goroutine of the store that is waiting, so neither
// may block or call back into the host: they signal, and the work happens
// elsewhere. Both may be called repeatedly while one store waits.
type Pressure struct {
	// Checkpoint asks for an immediate checkpoint of one region, out of the
	// interval's turn, and reports whether one will be taken. The host asks for
	// the region holding the largest dirty set first and works down; a false
	// answer for every region is what makes a store a stall.
	Checkpoint func(*Region) bool
	// Stop reports a region whose store could not be admitted, with the cause
	// to log. Its owner stops that VM deliberately: a last checkpoint of what
	// it can still capture, and a reason on the record, where the killed VMM
	// the fault path produces leaves neither.
	Stop func(*Region, error)
	// Oldest reports the oldest unpublished write of the whole VM one region
	// belongs to, zero where that VM holds none. The loss window is a VM's,
	// because the checkpoint that ends it is: one region's pages are published
	// by the same pause as its siblings'. The pager holds no idea of a VM, so
	// the owner that already answers Checkpoint per region answers this too;
	// a host that installs none leaves each region answering for itself, which
	// bounds every region and therefore the VM, only less sharply.
	Oldest func(*Region) time.Time
}

// Backing is satisfied by *volume.Volume. The pager never writes to it: a
// region's dirty pages reach storage through the checkpoint that reads its
// sealed checkpoint, not through this interface. Load and Locate serve the
// handle's current view without a writer-authority check and without network
// I/O; Verify checks that this host still owns the VM, and Region.Verify runs
// it on the supervisor's schedule.
type Backing interface {
	Size() uint64
	Load(context.Context, uint64, []byte) error
	Verify(context.Context) error
	Locate(context.Context, uint64, uint64) ([]control.Extent, error)
}

// PagedBacking is a Backing that states the page its volume is published in,
// which *volume.Volume does. This pager has one page — PageSize — and every
// number it faults, keys and serves is in it, so a backing whose volume is
// published in another page size is refused when it is attached rather than
// read in the wrong unit. A backing that states nothing is taken to be this
// pager's page, which is what a test mapping and a peer backing over a volume
// of this page are.
type PagedBacking interface {
	PageSize() uint64
}

// UnpublishedLoader is a Backing whose loads can return bytes its volume does
// not hold. A migration destination's peer backing is one: the pages the source
// host serves out of its own dirty pages are the guest's state since the
// source's last checkpoint, and no checkpoint has them.
//
// A page reported unpublished is loaded as this region's private dirty state —
// a private page under a spill reservation — rather than as clean state of the
// checkpoint the volume reports for it, because the checkpoint's identity names
// different bytes. The next checkpoint is what publishes it, which on a
// destination is the interval checkpoint that follows the migration.
type UnpublishedLoader interface {
	// LoadUnpublished fills dst exactly as Load does and reports one element
	// per pager page of the range, true where the pager must hold that page
	// privately. A nil result means every page is the volume's own.
	LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error)
}

// UnpublishedInstaller is an UnpublishedLoader that is told which of the pages
// it reported unpublished this region went on to hold. A load is not an
// install: the bytes reach a buffer, and the pager may still drop the page — a
// read-ahead page on a full dirty budget is the ordinary way — after which the
// bytes are nowhere. A backing whose unpublished pages exist only on another
// host needs that difference, because it is the one thing that knows whether
// the page may still be asked for.
type UnpublishedInstaller interface {
	UnpublishedLoader
	// InstalledUnpublished reports, for the range one load filled starting at
	// offset, which pages this region now holds as its own dirty state. It is
	// called after the load's pages have been bound, and a page it does not
	// name is one the load did not leave behind.
	InstalledUnpublished(offset uint64, installed []bool)
}

// Arena holds exactly Config.ResidentPages slots. Release must punch the slot,
// not just forget its address. Only Host decides when it is safe to release.
// Arena calls must not retain the supplied buffers.
type Arena interface {
	Read(context.Context, int, []byte) error
	Write(context.Context, int, []byte) error
	Release(context.Context, int) error
}

// ZeroArena is an Arena that gives free slots zero contents without writing
// them. Every free slot is punched and so already reads as zeros; Zero makes
// count consecutive such slots mappable, which on Linux allocates their pages
// without copying anything into them. An Arena without it is written zeros.
type ZeroArena interface {
	Zero(ctx context.Context, slot, count int) error
}

// EqualArena is an Arena that compares two of its own slots without copying
// their bytes out. A settle's whole cost is that comparison, and an arena whose
// slots are in this process's address space answers it with one bytes.Equal
// over the two of them. An Arena without it is read into two buffers instead,
// which is the same answer for twice the memory traffic.
type EqualArena interface {
	Equal(ctx context.Context, first, second int) (bool, error)
}

// Mapping controls one process region. Map installs already armed mappings for
// count consecutive pages backed by count consecutive arena slots; Revoke
// installs a missing-fault trap. Both wait for acknowledgement and drain
// transient kernel users before returning. Long-lived external pins are not
// permitted. Errors can be ambiguous, so Host retains all possibly mapped slots
// until Detach after process termination. Resolve installs the page tables for
// count consecutive mapped pages and completes any trapped access to them.
// Callbacks must not call Host or Region methods recursively.
type Mapping interface {
	Map(ctx context.Context, page uint64, slot, count int, writable bool) error
	// MapZero installs read-only, first-write-trapped zeros without arena slots.
	MapZero(ctx context.Context, page uint64, count int) error
	Revoke(ctx context.Context, page uint64) error
	Resolve(ctx context.Context, page uint64, count int, writable bool) error
	// Protect takes write access away from count consecutive mapped pages
	// without moving them: their memory, contents and page tables stay exactly
	// as they are, and the next store to one of them traps like a store to a
	// shared mapping. It costs one command per range whatever memory those
	// pages hold, which is what makes a seal proportional to the runs of a
	// dirty set rather than to its pages. Pages already protected are allowed.
	Protect(ctx context.Context, page uint64, count int) error
}

type MapRun struct {
	Page        uint64
	Slot, Count int
	Zero        bool // explicit sparse/discarded zero backing, never inferred from bytes
}

// BatchMapping installs disjoint read-only runs with bounded command overhead.
type BatchMapping interface {
	MapBatch(context.Context, []MapRun) (commands, runs int, err error)
}

// PageRun is a half-open logical range of Count whole pages.
type PageRun struct {
	Page  uint64
	Count int
}

// BatchRevocation waits for a bounded set of range revokes as one transition.
// An error is ambiguous for every submitted run.
type BatchRevocation interface {
	RevokeBatch(context.Context, []PageRun) (commands, runs int, err error)
}

type Config struct {
	// ConcurrentIO bounds ordinary page operations. Each can hold one read-ahead
	// or spill buffer. One additional permit reserves a checkpoint's reads of a
	// sealed checkpoint when cold faults saturate this budget, which is what keeps
	// a guest dirtying faster than its checkpoint uploads from deadlocking against
	// it. Zero selects 16.
	ConcurrentIO int
	// ReadAheadPages bounds the aligned run a fault loads and maps at once. It
	// is the host's one read-ahead policy: no region overrides it. Read-ahead
	// only uses free arena slots; it never evicts. Zero selects one page, and
	// the range cannot exceed 16 MiB.
	ReadAheadPages int
	// WriteAheadPages bounds the run of pages one store into fresh zeros, a
	// zero-mapped page or a hole the guest never touched, makes private
	// at once: the faulting page, the fresh zero pages after it and, where its
	// read-ahead run ends first, before it. Like read-ahead it uses only free
	// arena slots and free dirty reservations and never evicts or waits for
	// them. The whole run is mapped writable by one command, so no store into
	// it faults again, and every page of it is therefore charged a dirty
	// reservation and written back like a stored page. Zero selects one page;
	// one disables it.
	WriteAheadPages int
	// SettleWorkers is how many workers one settle divides a sealed set
	// between. Settling a page is a comparison of two resident pages and the
	// page-table work of re-sharing one, so it reads no disk and no store and
	// takes none of the pager's I/O permits: what bounds it is memory
	// bandwidth, and it is time the upload waits for, so it is divided rather
	// than queued. The workers share nothing but the count and the set the
	// checkpoint will list, so what a settle leaves does not depend on how many
	// there are. Zero selects one, which settles on the caller's own goroutine.
	SettleWorkers int
	ResidentPages int
	// LogicalPages bounds all per-region metadata, including never-faulted pages.
	LogicalPages int
	// DirtyPages bounds volatile private state on RAM and spill combined.
	DirtyPages int
	// LossWindow bounds a VM's unpublished writes in time as DirtyPages bounds
	// them in bytes. While the oldest unpublished write of a region's VM is
	// older than this, the pager admits no further dirty page for it: every
	// store that needs a reservation waits, and a checkpoint of that VM is asked
	// for out of the interval's turn. What a host loss can then cost one VM
	// spans at most this window plus one checkpoint attempt's pause. Zero
	// disables it, which leaves the dirty budget as the only bound.
	LossWindow time.Duration
	// Clock is what this pager's latency histograms and its client's command
	// deadlines are measured on. Nil is the wall clock. Nothing here decides
	// anything: the histograms are instrumentation, and injecting the clock is
	// what keeps a simulated run from reading the machine it happens to be on.
	Clock platform.Clock
}

// ProbeEvictionDuringPublication marks an eviction that punched a page of a
// region a publication was reading at that moment. The two hold different
// locks over the same bytes, so it is the overlap a pager that only ever had
// room for its guest never reaches.
const ProbeEvictionDuringPublication = "vmmemory/eviction-during-publication"
