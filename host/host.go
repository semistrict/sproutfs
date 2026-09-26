// Package host is everything one host of the deployment does: it opens VMs from
// their control records, runs their VMM processes, checkpoints them on an
// interval, captures and forks them, hands them to other hosts and takes them
// over. It grants no writer authority of its own — a VM's authority is the
// epoch in its control record.
//
// Host is what the deployment runs on this machine: the object namespace, the
// shared page cache, the volume manager, the migration page server, and the
// loops that keep what it runs durable and fenced. Its Machine is one VMM
// process, whatever runs it. The supervisor around it —
// Start, which returns the Service a command serves — owns the pager and the
// Firecracker processes, boots a VM over vmmachine, imports guest
// images into the templates VMs are forked from, and reaches the agent inside a
// guest.
//
// Hosts reach each other over plain TCP on a trusted cluster network. A host
// holds no admitted identity and keeps no view of its peers: the only address
// it ever dials is the page-server address a handoff carries, and the page
// server serves whoever reaches it. Restricting that port to hosts is the
// cluster's network policy, not this package's.
package host

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

var ErrInvalidConfig = errors.New("host: invalid configuration")
var ErrClosed = errors.New("host: host closed")

// VolumeConfig configures local volume work without allowing a host to
// replace its network or shared backing store. Every field is a
// volume.Config budget and takes that package's default when it is zero.
type VolumeConfig struct {
	// MaxWriteBytes bounds one write or batched write, and MaxOpenVMs the
	// handles this host may hold at once.
	MaxWriteBytes int
	MaxOpenVMs    int
}

// Config supplies the deployment's object namespace. A host owns no durable
// local state: every VM's authority is its control record, written with
// conditional writes in ObjectStore.
type Config struct {
	// Network carries migration pages between hosts. The process chooses the
	// adapter: plain TCP on a trusted cluster network, and a simulation its
	// own.
	Network platform.Network
	// Resources is the RAM allotment this host's pager takes its pages from.
	Resources *resource.Budget
	// Clock is the passage of time this host's deadlines are measured against:
	// the checkpoint interval, the epoch watch, and the holds that retire a
	// handover nothing ever released. Nil is the wall clock, which is what a
	// deployment runs on; a simulation passes one it advances itself, so a hold
	// written in checkpoint intervals is reached by a decision rather than by
	// waiting four minutes for it.
	Clock platform.Clock
	// Entropy is where the jitter that keeps this host's VMs from checkpointing
	// in lockstep is drawn from. Nil is the operating system's. A simulation
	// passes one keyed to its seed, so a run's interval spread is reproducible.
	Entropy platform.Entropy
	// Pagers are the shared pagers the VMs this host runs map their memory
	// through, one per kind of memory region: each has its own arena, its own spill
	// file and its own page. A host given them answers the dirty-budget
	// pressure of both: a checkpoint of the VM holding the largest dirty set,
	// taken out of the interval's turn, and a deliberate stop of a VM whose
	// stores no checkpoint can admit. Either pager's pressure seals the whole
	// VM, once, because one pause seals every memory region it maps. A host without
	// them leaves its guests to stall, so a supervisor that runs VMs passes its
	// pagers here.
	Pagers       vmmemory.Pagers
	ObjectStore  platform.ObjectStore
	ObjectPrefix platform.ObjectPrefix
	// CacheBytes caps the page cache, which owns that many bytes of its own
	// rather than competing with the pager for one allotment. Zero selects
	// DefaultCacheBytes.
	CacheBytes int64
	Cache      checkpoint.CacheConfig
	Volumes    VolumeConfig
	// CheckpointInterval is how often every VM this host runs is checkpointed: the
	// vCPUs pause for the VMM state capture and the seal, the guest resumes, and
	// the sealed pages upload behind it. It is the only thing that makes a
	// running VM durable, so it bounds what a host loss rewinds the VM by. Each
	// wait is jittered by up to an eighth either side so VMs do not checkpoint
	// in lockstep. Zero selects DefaultCheckpointInterval; a negative value disables
	// the loop, which is what a test that drives its own captures wants.
	CheckpointInterval time.Duration
	// LossWindow is how long a VM this host runs may hold a write no checkpoint
	// covers. It is the interval's companion: the interval says how often a VM is
	// made durable when everything works, and this says what happens when it does
	// not — past the window the pager admits no further dirty page for that VM,
	// so what a host loss can cost one guest spans at most this window plus one
	// checkpoint attempt's pause. The host's own part of it is the loop: a
	// publication that failed while the window is exceeded is retried at an eighth
	// of the interval, doubling to the interval, rather than an interval later.
	// Zero selects DefaultLossWindow; a negative value disables the bound, which
	// is what a deployment that would rather lose writes than ever stall a guest
	// asks for. The same value belongs in the pager's own Config: this host
	// reports the window and hurries its retries, and the pager is what holds the
	// guest back.
	LossWindow time.Duration
	// FlushBound is how old a VM's oldest unpublished disk write may be for a
	// guest's flush to complete at once. A flush of a VM holding an older one
	// waits until a checkpoint covers it, and the host takes that checkpoint
	// out of the interval's turn; a flush never takes one otherwise. Zero
	// selects twice the checkpoint interval (FlushBoundIntervals); a negative
	// value completes every flush at once.
	FlushBound time.Duration
	// EpochInterval is how often this host re-reads the control record of every
	// VM it holds, which is how it learns that a later writer has taken one
	// over. The other place that is learned is a checkpoint, and not every VM
	// reaches one: a running VM is up to CheckpointInterval from its next, and a
	// VM a fork point has sealed is never checkpointed at all. Until the host
	// knows, its guest goes on writing into pages nothing can ever publish and
	// its page server goes on serving them. Zero selects DefaultEpochInterval; a
	// negative value disables the timer, which only a test that drives the check
	// itself wants.
	EpochInterval time.Duration
	// Migration turns on live migration: the endpoint this host serves the
	// pages of a VM it has handed over on, and how it starts one it receives.
	// A host without it neither drains nor receives.
	Migration MigrationConfig
	// MachineClosed reports a VM this host has given up on its own: a later
	// writer took its control record, so the host stopped its VMM and released
	// its volumes rather than leave a guest running whose writes can never be
	// published; the dirty budget could no longer admit its stores; or its VMM
	// process ended and there is no guest left to run. It is the supervisor's
	// cue to forget that VM. It is called from one of the host's own
	// goroutines, so it must not block or call back into the host.
	MachineClosed func(vmID string)
}

// Host assembles the deployment's object namespace, the shared page cache, the
// volume manager that opens VMs from their control records, and the migration
// page server.
type Host struct {
	resources   *resource.Budget
	ctx         context.Context
	cancel      context.CancelCauseFunc
	network     platform.Network
	cache       *checkpoint.Cache
	control     *control.Client
	checkpoints *checkpoint.Store
	volumes     *volume.Manager
	// pages serves the memory of every VM this host has handed to another, and
	// machines is what it runs, which is what a drain moves.
	pages     *vmmigrate.PageSource
	migration MigrationConfig
	// closeMachine tells the supervisor about a VM this host closed on its own,
	// which is what a fencing checkpoint leads to.
	closeMachine func(vmID string)
	// pagers are the shared pagers whose dirty-budget pressure this host
	// answers, one per kind of memory region. Nothing adds their page counts together:
	// what this host reports across the two is always bytes.
	pagers vmmemory.Pagers
	// clock is every deadline this host keeps and entropy the jitter that
	// spreads the intervals, both filled in from Config.
	clock      platform.Clock
	entropy    platform.Entropy
	cacheBytes int64
	// holdTimeout is how long one handover's pages are served for before this
	// host gives them up on its own, when the configuration names a bound of its
	// own rather than taking it from the checkpoint interval.
	holdTimeout        time.Duration
	checkpointInterval time.Duration
	epochInterval      time.Duration
	// lossWindow is how old a VM's oldest unpublished write may get before this
	// host reports its stores as waiting and stops spacing its retries out by
	// the interval. Zero or less is the bound turned off.
	lossWindow time.Duration
	// flushBound is how stale a VM's disks may be for a flush of them to
	// complete at once. Zero or less completes every flush at once.
	flushBound time.Duration
	machines   machines
	closeOnce  sync.Once
	done       chan struct{}
	closeErr   error
}

// cleanupTimeout bounds the control-plane writes a failed operation makes on its
// way out: releasing a handle, removing a record it created, retiring a point
// it took. They have to outlive the request's own cancellation — a client that
// hung up must not leave a record half written — and a context with neither a
// cancellation nor a deadline is one the object store can hold a goroutine on
// for as long as this process runs. It is generous, because what is at stake is
// a record nothing else will ever clean up.
const cleanupTimeout = 30 * time.Second

// cleanup is the context a failed operation undoes itself on: the caller's
// cancellation dropped, and a deadline of its own put back.
func cleanup(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// closing releases a VM handle on the way out of an operation that failed,
// which is a control-record write, on a cleanup context of its own.
func closing(ctx context.Context, vm *volume.VM) error {
	undo, cancel := cleanup(ctx)
	defer cancel()
	return vm.Close(undo)
}

// retiring gives up a fork point on the way out of an operation that failed,
// which may write the parent's control record, on a cleanup context of its own.
func retiring(ctx context.Context, point *volume.ForkPoint) error {
	undo, cancel := cleanup(ctx)
	defer cancel()
	return point.Retire(undo)
}

// DefaultCacheBytes is the page cache's own allotment when a host configures
// none.
const DefaultCacheBytes = int64(1) << 30

// DefaultCheckpointInterval is how often a host with no interval configured
// checkpoints every VM it runs. A host loss rewinds a VM by at most this much
// guest time, which the requirements accept in exchange for never blocking a
// guest write on the object store.
const DefaultCheckpointInterval = 60 * time.Second

// DefaultLossWindow is how long a host with no window configured lets a VM hold
// a write no checkpoint covers. It is five checkpoint intervals: long enough
// that a deployment whose store is merely slow never notices it, short enough
// that a guest is stopped from building for minutes on writes a host loss would
// take with it.
const DefaultLossWindow = 5 * time.Minute

// DefaultEpochInterval is how often a host with no interval configured re-reads
// the control record of every VM it holds. It bounds how long a host that has
// been taken over goes on running a guest whose writes can never be published,
// independently of the checkpoint interval, and it costs one small read per VM.
const DefaultEpochInterval = 2 * time.Second

// The checkpoint store and the page cache take their budgets from this host
// rather than from a package default: a machine with many cores and a large
// cache arena should fetch, decode and upload more at once than a small one,
// and none of these may be left at a package's idea of a safe minimum. Each is
// clamped, because every slot is memory a host took without being asked.
const (
	minimumUploadSlots, maximumUploadSlots   = 8, 64
	minimumPartBuilders, maximumPartBuilders = 2, 8
	minimumEncoders, maximumEncoders         = 2, 16
	minimumDecoders, maximumDecoders         = 4, 32
	minimumCacheLoads, maximumCacheLoads     = 16, 256
)

// uploadSlots is how many objects this host has in flight at a time, over every
// publication it runs.
func uploadSlots() int {
	return clampToCPUs(2, minimumUploadSlots, maximumUploadSlots)
}

// partBuilders is how many publications may be filling a part at a time. Each holds a
// part builder of up to the store's PartBytes, so this is publication's memory.
func partBuilders() int {
	return clampToCPUs(4, minimumPartBuilders, maximumPartBuilders) // one per four cores
}

// encodeWorkers sizes the publication path's compression pool, and
// decodeWorkers the fault path's: faults are many and small, encoding is as
// wide as the uploads it feeds.
func encodeWorkers() int { return clampToCPUs(2, minimumEncoders, maximumEncoders) }

func decodeWorkers() int { return clampToCPUs(1, minimumDecoders, maximumDecoders) }

// clampToCPUs is this machine's core count divided by per, held between low and
// high.
func clampToCPUs(per, low, high int) int {
	return min(max(runtime.NumCPU()/per, low), high)
}

// cacheSizing fills in the page cache's admission bound when the host has not
// been given one: enough fetches in flight for this machine's cores, and never
// more than the cache arena could hold at once.
func cacheSizing(config Config) checkpoint.CacheConfig {
	cache := config.Cache
	if cache.MaxConcurrentLoads != 0 {
		return cache
	}
	loads := clampToCPUs(1, minimumCacheLoads, maximumCacheLoads)
	if arena := int(config.CacheBytes / checkpoint.PageSize2MiB); arena > 0 {
		loads = min(loads, arena)
	}
	cache.MaxConcurrentLoads = max(loads, 1)
	return cache
}

// Resources is the allotment a supervisor must pass to this host's pager and
// any other resource owners it creates. The page cache has its own budget.
func (h *Host) Resources() *resource.Budget { return h.resources }

func StartHost(ctx context.Context, config Config) (*Host, error) {
	if config.Resources == nil || config.ObjectStore == nil || config.Network == nil || config.CacheBytes < 0 {
		return nil, ErrInvalidConfig
	}
	if config.Resources.Stats().Limit <= 0 {
		return nil, ErrInvalidConfig
	}
	if config.CacheBytes == 0 {
		config.CacheBytes = DefaultCacheBytes
	}
	interval := config.CheckpointInterval
	if interval == 0 {
		interval = DefaultCheckpointInterval
	}
	epochs := config.EpochInterval
	if epochs == 0 {
		epochs = DefaultEpochInterval
	}
	window := lossWindowOf(config.LossWindow)
	var err error
	hostCtx, cancel := context.WithCancelCause(ctx)
	h := &Host{resources: config.Resources, ctx: hostCtx, cancel: cancel, network: config.Network,
		migration: config.Migration, closeMachine: config.MachineClosed, pagers: config.Pagers,
		holdTimeout: config.Migration.HoldTimeout,
		clock:       platform.ClockOr(config.Clock), entropy: platform.EntropyOr(config.Entropy),
		cacheBytes: config.CacheBytes, checkpointInterval: interval, epochInterval: epochs,
		lossWindow: window, flushBound: flushBoundOf(config.FlushBound, interval),
		done: make(chan struct{}),
		machines: machines{running: make(map[string]*registration), migrated: make(map[string]*migratedHold),
			forked: make(map[string]*forkHold), fenced: make(map[string]bool),
			stopping: make(map[*registration]string), receiving: make(map[string]bool)}}
	started := false
	defer func() {
		if !started {
			_ = h.Close(context.Background())
		}
	}()
	// The cache has a cap of its own: disposable pages must never be able to
	// take the pages a guest needs, and the pager must never have to reclaim
	// across a concern to get them back.
	cacheBudget, err := resource.New(config.CacheBytes)
	if err != nil {
		return nil, err
	}
	h.cache, err = checkpoint.NewCache(cacheBudget, cacheSizing(config))
	if err != nil {
		return nil, err
	}
	// Publication encodes and the fault path decodes through pools of their
	// own, so a guest's page fault never waits behind a checkpoint's encoding.
	codecs, err := blob.NewCodecs(encodeWorkers(), decodeWorkers())
	if err != nil {
		return nil, err
	}
	// The records this host writes draw their writer nonces and the epoch a
	// creation takes from the host's own entropy, and date what they keep by
	// the host's own clock, which is what makes a simulated deployment's
	// object keys and records reproducible.
	h.control, err = control.NewClient(control.Config{ObjectStore: config.ObjectStore,
		ObjectPrefix: config.ObjectPrefix, Entropy: h.entropy, Clock: h.clock})
	if err != nil {
		return nil, err
	}
	h.checkpoints, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: config.ObjectStore,
		ObjectPrefix: config.ObjectPrefix, Cache: h.cache, Codecs: codecs,
		Concurrency: uploadSlots(), MaxBuilders: partBuilders()})
	if err != nil {
		return nil, err
	}
	v := config.Volumes
	h.volumes, err = volume.NewManager(volume.Config{Control: h.control, Store: h.checkpoints,
		MaxWriteBytes: v.MaxWriteBytes, MaxOpenVMs: v.MaxOpenVMs})
	if err != nil {
		return nil, err
	}
	if config.Migration.Address != "" {
		// The page server serves any peer that reaches it; restricting the port
		// to hosts is the cluster's network policy.
		h.pages, err = vmmigrate.NewPageSource(hostCtx, vmmigrate.SourceConfig{Network: config.Network,
			Address: config.Migration.Address, PageSize: config.Migration.PageSize})
		if err != nil {
			return nil, fmt.Errorf("migration address %q: %w", config.Migration.Address, err)
		}
	}
	if err := context.Cause(hostCtx); err != nil {
		return nil, err
	}
	for _, pager := range h.pagers.All() {
		// A pager can neither checkpoint a VM nor stop one; this host is what
		// each asks to do either when its dirty budget fills. Both answer into
		// the same three callbacks, which act on the VM a memory region belongs to
		// rather than on the pager the pressure came from: one pause seals every
		// memory region of that VM, in both pagers, exactly once.
		pager.SetPressure(vmmemory.Pressure{Checkpoint: h.checkpointNow, Stop: h.stopStalled,
			Oldest: h.oldestUnpublished})
		// A guest's flush waits on this host too: the pager answers it when
		// this says the VM's disks are fresh enough.
		pager.SetFlushed(h.flushed)
	}
	started = true
	go func() {
		select {
		case <-hostCtx.Done():
			_ = h.Close(context.Background())
		case <-h.done:
		}
	}()
	if h.epochInterval > 0 {
		go h.watching(hostCtx)
	}
	return h, nil
}

// Volumes opens the VMs this host serves, each from the checkpoint its control
// record selects.
func (h *Host) Volumes() *volume.Manager { return h.volumes }

// Checkpoints reads and publishes checkpoints in the deployment's object
// namespace, through this host's shared page cache.
func (h *Host) Checkpoints() *checkpoint.Store { return h.checkpoints }

// Control reads and writes the control records of the deployment's VMs, which
// is what selects a VM's checkpoint and fences its previous writer.
func (h *Host) Control() *control.Client { return h.control }

type Status struct {
	Resources resource.Stats
	Closed    bool
	// Cache is the page cache's occupancy and CacheLimit the cap of its own
	// it fills, which is separate from Resources.
	Cache      checkpoint.CacheStats
	CacheLimit int64
	Volumes    volume.Stats
	// Pages is what this host's migration page server has answered, and Serving
	// every handover this host still holds pages for: the VMs it migrated away
	// and the children of every fork point it took, wherever those children
	// landed. A drain is not finished while Serving is not empty: those pages
	// include pages no checkpoint has, so a host that exits with them loses the
	// guest's writes since its last checkpoint — a migrated VM's, or a sealed
	// parent's.
	//
	// A child taken in on its parent's own host is served nothing, because it
	// maps the pages the seal froze rather than fetching them, so the page
	// server knows nothing about it. Its hold is a handover all the same, and
	// the same shape: it holds the parent sealed, it owes pages until the child
	// is taken in, and its release is refused until then. A host reporting only
	// what its page server holds would say a parent nothing can checkpoint is a
	// parent nothing is waiting on.
	Pages   vmmigrate.SourceStats
	Serving []string
	// Outstanding is, per VM in Serving, how many pages this host holds that no
	// checkpoint has and that no destination has fetched yet. Zero is a
	// handover whose pages are all on the other host and that only the
	// orchestrator's word is keeping open; anything else is what a drain is
	// actually waiting for. A VM whose volumes could not be listed reports -1,
	// because what it still holds is unknown. A child on this host maps the
	// pages rather than fetching them, so it owes every page the point holds
	// for it until it is taken in, and none after.
	Outstanding map[string]int
	// LogicalPagesFree is what each pager's per-memory-region metadata cap still has
	// left, in that pager's own pages. It is what admits a VM: the memory regions of
	// one that needs more than this cannot all be mapped, and a host that
	// started it anyway would find out at the attachment of whichever memory region ran
	// into the cap, with the VMM already running and the guest already lost. The
	// two are reported apart because they are counted in different pages and
	// cannot be added. A host with no pagers of its own reports nothing here.
	LogicalPagesFree KindPages
}

// KindPages is a page count per kind of memory region. The two are never summed: a RAM
// page and a PMEM page are different numbers of bytes, so anything host-wide
// converts to bytes first.
//
// Ephemeral is the ephemeral pager's count, in its own pages like the others.
type KindPages struct {
	RAM, PMEM, Ephemeral int
}

// KindBytes is a byte budget divided between the two pagers. Bytes do add up,
// which is the whole reason a host's memory accounting is in them: Total is what
// the deployment gave this host, and the two shares are what it holds.
type KindBytes struct {
	RAM, PMEM int64
}

// Total is what the two shares cost this host together.
func (b KindBytes) Total() int64 { return b.RAM + b.PMEM }

func (h *Host) Status() Status {
	status := Status{Resources: h.resources.Stats(), CacheLimit: h.cacheBytes,
		Closed: context.Cause(h.ctx) != nil}
	if h.pagers.Ram != nil {
		status.LogicalPagesFree.RAM = h.pagers.Ram.LogicalHeadroom()
	}
	if h.pagers.Pmem != nil {
		status.LogicalPagesFree.PMEM = h.pagers.Pmem.LogicalHeadroom()
	}
	if h.pagers.Ephemeral != nil {
		status.LogicalPagesFree.Ephemeral = h.pagers.Ephemeral.LogicalHeadroom()
	}
	if h.cache != nil {
		status.Cache = h.cache.Stats()
	}
	if h.volumes != nil {
		status.Volumes = h.volumes.Stats()
	}
	if h.pages != nil {
		status.Pages = h.pages.Stats()
	}
	status.Serving = h.serving()
	status.Outstanding = h.outstanding(status.Serving)
	return status
}

// outstanding is what each handover in serving still owes: what the page
// server has not answered for, and for a child this host takes in itself, what
// it has yet to take from the point.
func (h *Host) outstanding(serving []string) map[string]int {
	left := make(map[string]int, len(serving))
	if h.pages != nil {
		left = h.pages.Outstanding()
	}
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	for _, vmID := range serving {
		if hold := h.machines.forked[vmID]; hold != nil && hold.local {
			left[vmID] = hold.owed()
		} else if _, found := left[vmID]; !found {
			left[vmID] = 0
		}
	}
	return left
}

// serving is every handover this host still holds pages for, in ascending
// identity order: what its page server answers for, and the children of every
// fork point it took — a child on this host's own pages among them, which no
// page server ever hears of.
func (h *Host) serving() []string {
	var held []string
	if h.pages != nil {
		held = h.pages.Serving()
	}
	h.machines.mu.Lock()
	for child := range h.machines.forked {
		if !slices.Contains(held, child) {
			held = append(held, child)
		}
	}
	h.machines.mu.Unlock()
	slices.Sort(held)
	return held
}

// RAMVolume is the volume a VM's guest RAM is; every other volume it maps is a
// PMEM device. It is the deployment's naming convention and not something the
// pager reads — a memory region's kind is stated by whoever attaches it — but a host
// deciding which pager a VM's memory regions would be admitted against has no machine
// to ask yet, so it is stated here instead. It must be the name
// vmmachine.RAMVolume binds RAM to.
const RAMVolume = "ram0"

// MemoryRegion is one memory region a VM would map: what it is to the guest, and the
// size of the volume behind it. Which pager it is charged against is the kind,
// because the two hold their own metadata caps in their own pages.
//
// Ephemeral marks an ephemeral disk, which is charged to the ephemeral pager
// instead of its kind's.
type MemoryRegion struct {
	Kind      vmmemory.MemoryRegionKind
	Ephemeral bool
	Size      uint64
}

// memoryRegionOf is one volume as a memory region this host would admit.
func memoryRegionOf(name string, ephemeral bool, size uint64) MemoryRegion {
	kind := vmmemory.Pmem
	if name == RAMVolume {
		kind = vmmemory.Ram
	}
	return MemoryRegion{Kind: kind, Ephemeral: ephemeral, Size: size}
}

// AdmitMemoryRegions refuses a VM whose memory regions this host's pagers could not
// all map, before anything starts its VMM. The cap is per-memory-region metadata and a
// pager checks it one attachment at a time, so a VM that overruns it dies with
// its process already started and some of its memory regions already admitted — which
// is a guest killed for a decision that could have been made before it existed.
// Each memory region is charged to the pager of its kind, in that pager's pages,
// and an ephemeral disk to the ephemeral pager, whose logical cap is the disk it
// may fill: that cap is what keeps its every store from waiting. A host with
// pagers but no ephemeral pager refuses an ephemeral disk. A host with no
// pagers of its own admits everything; so does the race between this and the
// attachments, which is why this is a refusal and not a reservation.
func (h *Host) AdmitMemoryRegions(memoryRegions []MemoryRegion) error {
	needed := map[*vmmemory.Host]uint64{}
	var pagers []*vmmemory.Host
	for _, memoryRegion := range memoryRegions {
		pager := h.pagers.Of(memoryRegion.Kind, memoryRegion.Ephemeral)
		if pager == nil {
			if memoryRegion.Ephemeral && len(h.pagers.All()) > 0 {
				return fmt.Errorf("%w: this host runs no ephemeral pager, so it maps no ephemeral disk",
					vmmemory.ErrCapacity)
			}
			continue
		}
		page := pager.PageSize()
		if _, seen := needed[pager]; !seen {
			pagers = append(pagers, pager)
		}
		needed[pager] += (memoryRegion.Size + page - 1) / page
	}
	for _, pager := range pagers {
		free := pager.LogicalHeadroom()
		if free < 0 || needed[pager] > uint64(free) {
			return fmt.Errorf("%w: these memory regions need %d logical pages of %d bytes and that pager has %d left",
				vmmemory.ErrCapacity, needed[pager], pager.PageSize(), free)
		}
	}
	return nil
}

// Close closes the managed VM handles and releases the page server. Quiesce
// caller operations first. A canceled wait can be retried; shutdown keeps
// running.
//
// Closing a VM publishes a final checkpoint of whatever it still holds, so an
// orderly shutdown loses nothing. A process that dies instead loses every write
// since each VM's last checkpoint.
func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() { h.cancel(ErrClosed); go h.shutdown() })
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-h.done:
		return h.closeErr
	}
}

func (h *Host) shutdown() {
	var errs []error
	h.stopHolds()
	for _, pager := range h.pagers.All() {
		// Nothing here answers for either budget any more: the loops are
		// stopping and the VMs are being released.
		pager.SetPressure(vmmemory.Pressure{})
		// Nor for a flush. It is dropped rather than answered: a pager with
		// nothing installed answers every flush with success, and the disks a
		// closing host can no longer checkpoint are not durable. The guest's
		// device asks again wherever the VM is opened next.
		pager.SetFlushed(dropFlush)
	}
	// The checkpoint loops stop before the VM handles do: a checkpoint of a VM
	// whose handle is closing has nothing to select its index in.
	h.machines.mu.Lock()
	entries := slices.Collect(maps.Values(h.machines.running))
	h.machines.mu.Unlock()
	for _, entry := range entries {
		entry.end()
	}
	if h.pages != nil {
		// Serving stops before the VM handles do: nothing is left to serve once
		// the processes that own those pages are gone.
		errs = append(errs, h.pages.Close())
	}
	if h.volumes != nil {
		errs = append(errs, h.volumes.Close(context.Background()))
	}
	if h.cache != nil {
		h.cache.Close()
	}
	h.closeErr = errors.Join(errs...)
	close(h.done)
}
