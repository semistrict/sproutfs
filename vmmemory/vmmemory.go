// Package vmmemory manages bounded shared backing for mapped volume memory regions.
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
	"fmt"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// MemoryRegionKind is what one memory region is to the guest that maps it. A host runs one
// pager per kind, each with its own arena, its own spill file and its own page
// size, because what a deployment plans for is RAM and disk separately and the
// two no longer share a unit. It is set by whoever attaches the memory region, which
// is the only party that knows: a volume's name says nothing, and the pager
// must not read one.
type MemoryRegionKind uint64

const (
	Pmem MemoryRegionKind = 1
	Ram  MemoryRegionKind = 2
)

func (k MemoryRegionKind) String() string {
	switch k {
	case Ram:
		return "ram"
	case Pmem:
		return "pmem"
	}
	return "unknown"
}

// Pagers is one host's pager per kind of memory region. Nothing adds their page counts
// together — a RAM page and a PMEM page are different numbers of bytes — so
// everything a host reports across the two is in bytes.
//
// Ephemeral is a third pager, for the ephemeral disks no checkpoint holds
// (Config.Ephemeral). It is PMEM to the guest, and a pager of its own because
// its pages are a disk's only copy: they must not take the dirty budget or the
// arena of the disks checkpoints publish. A host that offers no ephemeral
// disks runs none.
type Pagers struct {
	Ram, Pmem, Ephemeral *Host
}

// For reports the pager a memory region of this kind attaches to when some
// checkpoint holds it, nil where this host runs none of that kind.
func (p Pagers) For(kind MemoryRegionKind) *Host {
	switch kind {
	case Ram:
		return p.Ram
	case Pmem:
		return p.Pmem
	}
	return nil
}

// Of reports the pager a memory region attaches to: the ephemeral pager for an
// ephemeral disk, and its kind's otherwise. It is nil where this host runs no
// such pager, and for ephemeral RAM, which does not exist.
func (p Pagers) Of(kind MemoryRegionKind, ephemeral bool) *Host {
	if !ephemeral {
		return p.For(kind)
	}
	if kind == Pmem {
		return p.Ephemeral
	}
	return nil
}

// All reports every pager, skipping any that is absent, so a caller that must
// reach every pager of a host cannot forget one.
func (p Pagers) All() []*Host {
	var all []*Host
	for _, h := range []*Host{p.Ram, p.Pmem, p.Ephemeral} {
		if h != nil {
			all = append(all, h)
		}
	}
	return all
}

// MemoryRegionBacking is the one memory region a session maps: what it is to the guest and
// the bytes it stands in front of.
type MemoryRegionBacking struct {
	Kind    MemoryRegionKind
	Backing Backing
}

var (
	ErrConfig   = errors.New("invalid managed-memory configuration")
	ErrCapacity = errors.New("managed-memory capacity exhausted")
	ErrClosed   = errors.New("managed-memory-region closed")
	ErrRange    = errors.New("invalid managed-memory page")
	// ErrContended reports a faulting page whose identity kept losing
	// publication races. It is retryable and never observed in practice.
	ErrContended = errors.New("managed-memory page could not be resolved")
	// ErrMappingRefused reports a mapping command the client refused: it checks
	// what a command would cost its mapping budget before it touches anything,
	// so a refusal is the one failure that is known to have changed nothing.
	// Nothing about the client's mappings moved, which makes it a failed
	// operation rather than a failed session — the pages are not mapped, the
	// memory region goes on serving, and the fault is served again once the budget it
	// ran out of has been freed. Every other mapping failure is ambiguous: the
	// command may have been applied, and the memory region is terminal.
	ErrMappingRefused = errors.New("managed-memory mapping refused")
	// ErrDirtyStalled reports a store the dirty budget cannot admit and no
	// checkpoint can make room for. It is deliberately not ErrCapacity: a store
	// that only has to wait waits, and this is the one case left, which the
	// memory region's owner is told about through Pressure.Stop so that it stops the VM
	// itself rather than letting a failed fault kill the VMM.
	ErrDirtyStalled = errors.New("managed-memory dirty budget stalled")
	// ErrWindowStalled reports a store held back by the loss window whose VM can
	// never be checkpointed: its loop is off, or its memory region belongs to no VM this
	// host runs. It is told apart from a budget stall because the two say
	// different things about a deployment — one that its guests dirty faster than
	// their checkpoints drain, this one that a guest's writes cannot be made
	// durable at all — and it ends the same way, with a deliberate stop.
	ErrWindowStalled = errors.New("managed-memory loss window stalled")
	// ErrTampered ends the session of a VMM that changed a page it holds
	// read-only: a page a checkpoint published, which another memory region
	// was about to inherit, no longer holds the bytes its upload read. A VMM
	// whose own stores went through its mapping never does this, because a
	// store into such a page traps and copies it.
	ErrTampered = errors.New("managed-memory page changed behind the pager")
	// ErrUncounted ends the session of a VMM whose private file holds more
	// memory than the pager put there: the VMM allocated it itself.
	ErrUncounted = errors.New("managed-memory file holds memory the pager did not put there")
)

// Pressure is how the pager pushes a full dirty budget back to whoever owns its
// memory regions, which is the only party that can checkpoint one or stop its VM. A
// host with no Pressure installed can do neither, so every store past the
// budget stalls.
//
// Both callbacks run on the goroutine of the store that is waiting, so neither
// may block or call back into the host: they signal, and the work happens
// elsewhere. Both may be called repeatedly while one store waits.
type Pressure struct {
	// Checkpoint asks for an immediate checkpoint of one memory region, out of the
	// interval's turn, and reports whether one will be taken. The host asks for
	// the memory region holding the largest dirty set first and works down; a false
	// answer for every memory region is what makes a store a stall.
	Checkpoint func(*MemoryRegion) bool
	// Stop asks the owner to stop the VM a memory region belongs to, with the
	// cause to log, and reports whether it will. It is asked when a bound runs
	// out that nothing else can relieve: the loss window of the memory region a
	// store is waiting in, or a full dirty budget, where the memory region is
	// the one holding the most of it. The owner stops that VM deliberately: a
	// last checkpoint of what it can still capture, and a reason on the record,
	// where the killed VMM the fault path produces leaves neither. An owner that
	// does not run the memory region's VM declines, and the pager asks about the
	// next one.
	Stop func(*MemoryRegion, error) bool
	// Oldest reports the oldest unpublished write of the whole VM one memory region
	// belongs to, zero where that VM holds none. The loss window is a VM's,
	// because the checkpoint that ends it is: one memory region's pages are published
	// by the same pause as its siblings'. The pager holds no idea of a VM, so
	// the owner that already answers Checkpoint per memory region answers this too;
	// a host that installs none leaves each memory region answering for itself, which
	// bounds every memory region and therefore the VM, only less sharply.
	Oldest func(*MemoryRegion) time.Time
}

// Backing is satisfied by *volume.Volume. The pager never writes to it: a
// memory region's dirty pages reach storage through the checkpoint that reads its
// sealed checkpoint, not through this interface. Load and Locate serve the
// handle's current view without a writer-authority check and without network
// I/O; Verify checks that this host still owns the VM, and MemoryRegion.Verify runs
// it on the supervisor's schedule.
type Backing interface {
	Size() uint64
	Load(context.Context, uint64, []byte) error
	Verify(context.Context) error
	Locate(context.Context, uint64, uint64) ([]control.Extent, error)
}

// PagedBacking is a Backing that states the page its volume is published in,
// which *volume.Volume does. A pager instance has one page — Config.PageSize —
// and every number it faults, keys and serves is in it, so a backing whose
// volume is published in another page size is refused when it is attached
// rather than read in the wrong unit. A backing that states nothing is taken to
// be this pager's page, which is what a test mapping and a peer backing over a
// volume of this page are.
type PagedBacking interface {
	PageSize() uint64
}

// EphemeralBacking is a Backing that states whether its volume is an ephemeral
// disk, one no checkpoint holds, which *volume.Volume does. Such a disk is
// served by a pager built for it (Config.Ephemeral) and by no other, and that
// pager serves nothing else, so a backing that disagrees with the pager is
// refused when it is attached. A backing that states nothing is taken to be
// what the pager serves, as it is for PagedBacking.
type EphemeralBacking interface {
	Ephemeral() bool
}

// SparseLoader is a Backing that can be asked for part of a range: the pages of
// it a mask marks, leaving the bytes of every other page alone. *volume.Volume
// is one.
//
// It is what makes a fault one read. A window is a run of pages of which the
// ones this memory region already holds resident need no bytes, and a volume groups
// the members it is asked for by the part they lie in, so a run with those
// pages left out costs one ranged read per part it spans — where asking for
// each stretch of the run separately costs a request per stretch, and the pages
// a guest has already faulted in are what cut a window into stretches. A
// backing that cannot leave a page out is read one stretch at a time.
type SparseLoader interface {
	// LoadPages fills the pages of [offset, offset+len(dst)) that wanted marks,
	// one element per pager page of the range, and leaves the rest of dst as it
	// was.
	LoadPages(ctx context.Context, offset uint64, dst []byte, wanted []bool) error
}

// UnpublishedLoader is a Backing whose loads can return bytes its volume does
// not hold. A migration destination's peer backing is one: the pages the source
// host serves out of its own dirty pages are the guest's state since the
// source's last checkpoint, and no checkpoint has them.
//
// A page reported unpublished is loaded as this memory region's private dirty state —
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
// it reported unpublished this memory region went on to hold. A load is not an
// install: the bytes reach a buffer, and the pager may still drop the page — a
// read-ahead page on a full dirty budget is the ordinary way — after which the
// bytes are nowhere. A backing whose unpublished pages exist only on another
// host needs that difference, because it is the one thing that knows whether
// the page may still be asked for.
type UnpublishedInstaller interface {
	UnpublishedLoader
	// InstalledUnpublished reports, for the range one load filled starting at
	// offset, which pages this memory region now holds as its own dirty state. It is
	// called after the load's pages have been bound, and a page it does not
	// name is one the load did not leave behind.
	InstalledUnpublished(offset uint64, installed []bool)
}

// Arena is where a pager keeps its resident pages: a set of files it makes.
// Every page is in one slot of one file. The pager makes file 0 when it starts,
// and in both arena modes every page is in it; see ArenaMode.
type Arena interface {
	// File makes a file of offsets slots, each one page of the pager, and every
	// one of them punched.
	File(ctx context.Context, offsets int) (ArenaFile, error)
}

// ArenaFile is one file of an arena. It holds exactly the slots it was made
// with, and a slot costs nothing until a page is put there, because the file is
// sparse. Release must punch the slot, not just forget its address, so that
// the memory really leaves. Only Host decides when it is safe to release. File
// calls must not retain the supplied buffers.
type ArenaFile interface {
	Read(context.Context, int, []byte) error
	Write(context.Context, int, []byte) error
	Release(context.Context, int) error
}

// ZeroFile is an ArenaFile that gives free slots zero contents without writing
// them. Every free slot is punched and so already reads as zeros; Zero makes
// count consecutive such slots mappable, which on Linux allocates their pages
// without copying anything into them. A file without it is written zeros.
type ZeroFile interface {
	Zero(ctx context.Context, slot, count int) error
}

// EqualFile is an ArenaFile that compares two of its own slots without copying
// their bytes out. A settle's whole cost is that comparison, and a file whose
// slots are in this process's address space answers it with one bytes.Equal
// over the two of them. Two slots of different files, or of a file without it,
// are read into two buffers instead, which is the same answer for twice the
// memory traffic.
type EqualFile interface {
	Equal(ctx context.Context, first, second int) (bool, error)
}

// CountedFile is an ArenaFile that can say how much memory it really holds and
// give back what the pager did not put there. A process given a file can
// allocate pages in it behind the pager's back: in its private file by writing
// a hole, and in a file it may only read by reading one through a mapping of
// its own. An isolated arena counts both.
type CountedFile interface {
	// AllocatedBytes is the memory the file holds.
	AllocatedBytes() (uint64, error)
	// Punch gives back every page of the file at a slot held reports false for.
	Punch(held func(slot int) bool) error
}

// ClosableFile is an ArenaFile the pager gives back to its arena once no page
// is left in it: the private file of a memory region that has gone, and a fork
// point's file once its seal ends. A file without it is kept until the arena
// itself is closed.
type ClosableFile interface {
	Close() error
}

// ArenaMode is how a pager divides its resident pages between the files of its
// arena.
type ArenaMode int

const (
	// ArenaShared keeps every resident page in file 0, which every VMM that
	// attaches a memory region receives read-write. It is the default.
	ArenaShared ArenaMode = iota
	// ArenaIsolated splits the arena by who may read each page. Each memory
	// region has a private file, which only its VMM receives read-write. The
	// pages another memory region may map are in the shared file, which every
	// VMM receives read-only, and the pages a fork point lends to the children
	// on this host are in a fork file, which those children receive read-only.
	ArenaIsolated
)

// String is the mode as a deployment names it.
func (m ArenaMode) String() string {
	switch m {
	case ArenaShared:
		return "shared"
	case ArenaIsolated:
		return "isolated"
	}
	return fmt.Sprintf("ArenaMode(%d)", int(m))
}

// ParseArenaMode reads a mode as a deployment names it.
func ParseArenaMode(name string) (ArenaMode, error) {
	for _, m := range []ArenaMode{ArenaShared, ArenaIsolated} {
		if name == m.String() {
			return m, nil
		}
	}
	return 0, fmt.Errorf("%w: arena mode %q, want shared or isolated", ErrConfig, name)
}

// Mapping controls one process memory region. Map installs already armed mappings for
// count consecutive pages backed by count consecutive slots of one arena file,
// which it names by the number it was given under; Revoke
// installs a missing-fault trap. Both wait for acknowledgement and drain
// transient kernel users before returning. Long-lived external pins are not
// permitted. Errors can be ambiguous, so Host retains all possibly mapped slots
// until Detach after process termination. Resolve installs the page tables for
// count consecutive mapped pages and completes any trapped access to them.
// Callbacks must not call Host or MemoryRegion methods recursively.
type Mapping interface {
	// GiveFile hands the process one file of the arena under the number its
	// maps name it by, before any map names it. File 0 is the memory region's
	// private file, which is the only one given writable and the only one a
	// writable map may name. Every other file is given read-only.
	GiveFile(ctx context.Context, number int, file ArenaFile, writable bool) error
	// DropFile takes a file back once nothing of it is mapped any more.
	DropFile(ctx context.Context, number int) error
	Map(ctx context.Context, page uint64, file, slot, count int, writable bool) error
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

// MapRun is Count pages from Page, mapped from Count consecutive slots from
// Slot of the arena file numbered File, or explicit zeros.
type MapRun struct {
	Page        uint64
	File        int
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
	// PageSize is this pager's unit: its arena slots, its spill slots, the
	// numbers it faults, keys and serves, and the alignment it requires of every
	// memory region. It is fixed for the pager's life, because a page number means
	// nothing without it, and it must be one of the page sizes a volume can be
	// published in — checkpoint.GeometryFor is the one place that says which
	// those are, so a pager and the volumes it maps cannot disagree about it.
	PageSize uint64
	// ConcurrentIO bounds ordinary page operations. Each can hold one read-ahead
	// or spill buffer. One additional permit reserves a checkpoint's reads of a
	// sealed checkpoint when cold faults saturate this budget, which is what keeps
	// a guest dirtying faster than its checkpoint uploads from deadlocking against
	// it. Zero selects 16.
	ConcurrentIO int
	// ReadAheadPages bounds the aligned run a fault loads and maps at once, in
	// pages of this pager. It is the host's one read-ahead policy for this kind
	// of memory region: no memory region overrides it. A deployment states the run in bytes
	// and each pager converts it into its own pages, because the run is a buffer
	// and a number of pages means different amounts of memory in the two.
	// Read-ahead only uses free arena slots; it never evicts. Zero selects one
	// page, and the range cannot exceed 16 MiB.
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
	// ResidentPages is the arena's capacity: how many of its offsets may hold
	// memory at once. It is what a deployment budgets, because it is the host
	// memory this pager owns.
	ResidentPages int
	// ArenaOffsets is how many addresses that arena has. It is at least
	// ResidentPages and may be far more: the arena is a sparse file, so an
	// offset holds memory only once a page is put there, and a pager that
	// places a private page at an offset of its own — so that pages adjacent in
	// a guest are adjacent in the arena and are one mapping — leaves most of the
	// offsets it owns empty. Zero selects ResidentPages, which is a pager whose
	// addresses and pages are one number.
	ArenaOffsets int
	// Arena is how this pager divides its resident pages between the files of
	// its arena. The zero value is ArenaShared.
	Arena ArenaMode
	// LogicalPages bounds all per-memory-region metadata, including never-faulted pages.
	LogicalPages int
	// DirtyPages bounds volatile private state on RAM and spill combined.
	DirtyPages int
	// Ephemeral makes this the pager of ephemeral disks: PMEM memory regions no
	// checkpoint holds. Their private pages are their only copy, in the arena
	// or spilled, until the memory region detaches. A seal of one takes
	// nothing, so no capture, fork or interval checkpoint reaches them; a
	// migration hands them over as the source's unpublished pages. Nothing
	// relieves the dirty budget here, so it must equal LogicalPages: a disk
	// admitted by its size may then make every page of it private without a
	// store ever waiting. LossWindow must be zero, because no checkpoint ends a
	// window of these pages.
	Ephemeral bool
	// LossWindow bounds a VM's unpublished writes in time as DirtyPages bounds
	// them in bytes. While the oldest unpublished write of a memory region's VM is
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
	// MeasureChanges counts, at every settle, how many 4 KiB blocks of each
	// checkpointed page the guest actually changed, into Stats.ChangedBlocks.
	// It sums a page's bytes when it becomes private and again behind the seal,
	// so it is a benchmark's instrument and not a deployment's.
	MeasureChanges bool
}

// Offsets is how many addresses this pager's arena has, which is what the arena
// is built with and what its FILE frame states: ArenaOffsets, or ResidentPages for a
// pager whose offsets and its pages are one number.
func (c Config) Offsets() int {
	if c.ArenaOffsets == 0 {
		return c.ResidentPages
	}
	return c.ArenaOffsets
}

// ProbeEvictionDuringPublication marks an eviction that punched a page of a
// memory region a publication was reading at that moment. The two hold different
// locks over the same bytes, so it is the overlap a pager that only ever had
// room for its guest never reaches.
const ProbeEvictionDuringPublication = "vmmemory/eviction-during-publication"
