//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

const (
	// rootVolume is the PMEM device the guest boots from, and ramVolume its
	// RAM. A VM's memory regions bind to volumes by these names.
	rootVolume = "root"
	// ephemeralVolume is the ephemeral disk a create gives a VM that asks for
	// one: a PMEM device no checkpoint holds, after the root.
	ephemeralVolume = "ephemeral"
	// consoleWindowBytes bounds one console read, which is a window on a
	// disposable ring buffer rather than a stream.
	consoleWindowBytes = 256 << 10
	// drainPoll is how often a drain asks whether the pages it handed over have
	// arrived. A drain is finished when nothing is left to serve.
	drainPoll = 250 * time.Millisecond
	// drainTimeout bounds the whole drain and drainVMTimeout one VM's handover
	// inside it; drainConcurrency is how many are handed over at once. A drain
	// is how an upgrade loses nothing, so it is given as long as a host full of
	// VMs needs, four at a time: thirty minutes. One handover stays short, under
	// the four intervals its source serves the pages for unreleased and the two
	// minutes the orchestrator believes its record of it. The deployment's
	// termination grace period is the preStop hook's own bound plus the shutdown
	// behind it — thirty-one minutes and one, the manifest's thirty-two — and
	// this is under the hook's, so a drain that runs to its bound still answers
	// the hook and still leaves the process the whole of that shutdown to publish
	// a final checkpoint of every VM that did not move.
	//
	// The caller is a preStop hook and carries no deadline of its own, and the
	// orchestrator drives both halves of every migration, so an orchestrator
	// that is wedged answers none of these calls. Bounding each one is what
	// turns that into a drain that moved some of its VMs rather than a pod
	// killed with all of their pages still on it.
	drainTimeout     = 30 * time.Minute
	drainVMTimeout   = 60 * time.Second
	drainConcurrency = 4
	// drainReportTimeout bounds one report to the orchestrator. A report is a
	// diagnostic, and it is sent on a context of its own so that the VM whose
	// handover just ran out of time is still reported as having done so.
	drainReportTimeout = 5 * time.Second
)

// machine is one VM this host runs: the handle that owns its volumes and
// publishes its checkpoints, and the VMM process that is its guest.
type machine struct {
	vm       *volume.VM
	process  *vmmachine.Process
	template string
}

// supervisor owns this host's VMM processes and its shared pager, which is what
// the hosting contract makes it responsible for closing, in that order, before
// the arena and the spill file.
type supervisor struct {
	config SupervisorConfig
	// orchestrator is asked, and only during a drain, where this host's VMs
	// should go.
	orchestrator *orch.Client

	resources *resource.Budget
	objects   *platform.MeteredObjectStore
	// arenas and spills are one per pager: a pager's arena and spill file are
	// its own, and the pagers of a host share neither.
	arenas map[pagerSlot]*vmmemory.LinuxArena
	spills map[pagerSlot]platform.File
	// cacheDisk is the file the page cache keeps what pulls copy in, nil where
	// the deployment gave it no space.
	cacheDisk platform.File
	// connection is what every session this host opens is configured with: the
	// node's fault-worker and mapping-count bounds, which no VM varies.
	connection vmmemory.ConnectionConfig
	pagers     vmmemory.Pagers
	scratch    *vmmachine.Scratch
	host       *Host
	// clock is what every duration this supervisor reports is measured on. It
	// is the same clock the host below it keeps its deadlines on.
	clock platform.Clock

	// templateMu admits one image import at a time: two creations of the first
	// VM would otherwise both import the same image.
	templateMu *ctxsync.Mutex
	// importErr is why this host is not ready yet, which is the last import
	// failure or the imports not being finished. It is nil once every
	// configured guest image is a template.
	importErr atomic.Pointer[error]

	mu        sync.Mutex
	machines  map[string]*machine
	templates map[string]*ImportedTemplate
	// byID is every template this host has imported or opened, by identity:
	// the configured images, and the ones imported on request here or on
	// another host.
	byID   map[string]*ImportedTemplate
	closed bool
}

var _ Service = (*supervisor)(nil)

// Start assembles this host: the resource budget, the object store, the
// pager over a HugeTLB arena and a spill file, the VMM scratch, and the
// host that opens VMs and serves the pages of the ones it hands over.
func Start(ctx context.Context, config SupervisorConfig) (Service, error) {
	s := &supervisor{config: config, clock: platform.ClockOr(config.Clock), templateMu: ctxsync.NewMutex(),
		machines: map[string]*machine{}, templates: map[string]*ImportedTemplate{},
		byID:   map[string]*ImportedTemplate{},
		arenas: map[pagerSlot]*vmmemory.LinuxArena{},
		spills: map[pagerSlot]platform.File{},
		// The orchestrator's default client has no timeout of its own, and a
		// drain's requests are the only ones this host makes: a connection that
		// is never answered and never closed would hold one open past every
		// deadline the drain gives it.
		orchestrator: orch.NewClient(config.Orchestrator,
			&http.Client{Timeout: drainVMTimeout}, config.APIToken)}
	started := false
	defer func() {
		if !started {
			// Everything built so far is released in the same order an orderly
			// shutdown releases it.
			if err := s.Close(context.WithoutCancel(ctx)); err != nil {
				slog.ErrorContext(ctx, "host: releasing a failed startup", "error", err)
			}
		}
	}()

	// Huge pages are what the PMEM arena is made of; the RAM arena is ordinary
	// memory charged to the pod unless its page is 2 MiB. The pod's mount is
	// what the kubelet grants its HugeTLB allotment through, so its absence
	// means that arena cannot be allocated at all.
	if _, err := RAMPage(config.RAMPageSize); err != nil {
		return nil, err
	}
	if config.Starter == nil {
		return nil, fmt.Errorf("%w: a host needs a VMM starter", ErrInvalidConfig)
	}
	if _, err := os.Stat(config.HugepageDir); err != nil {
		return nil, fmt.Errorf("hugepage mount %s: %w", config.HugepageDir, err)
	}
	var err error
	// The allotment is RAM: the pager's resident pages. Disk is capped per concern, so
	// the spill file and the VMM scratch answer to their own bounds instead.
	s.resources, err = resource.New(config.MemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("resource budget: %w", err)
	}
	// Every object this host reads or writes goes through one store, so metering
	// it here is the whole of the deployment's object traffic. One checkpoint's
	// share of it is attributed separately, through the context its publication
	// carries.
	s.objects, err = platform.NewMeteredObjectStore(config.ObjectStore)
	if err != nil {
		return nil, fmt.Errorf("metered object store: %w", err)
	}
	// One pager per kind of memory region, each over an arena and a spill file of its
	// own. The two capacities sum to what the deployment gave this host, so
	// nothing is counted twice and the arenas never compete for a slot.
	ram, err := s.startPager(ctx, ramPager, pagerConfig(config, vmmemory.Ram))
	if err != nil {
		return nil, err
	}
	pmem, err := s.startPager(ctx, pmemPager, pagerConfig(config, vmmemory.Pmem))
	if err != nil {
		return nil, err
	}
	s.pagers = vmmemory.Pagers{Ram: ram, Pmem: pmem}
	// The ephemeral pager is the third, with an arena and a spill file of its
	// own too, and only where the deployment gave it a disk.
	if cfg := ephemeralPagerConfig(config); cfg != nil {
		if s.pagers.Ephemeral, err = s.startPager(ctx, ephemeralPager, *cfg); err != nil {
			return nil, err
		}
	}
	// One session's bounds are the node's too: how many faults it serves at a
	// time, and the mapping-count budget its replacements are admitted against.
	s.connection = vmmemory.ConnectionConfig{MaxVMAs: vmaBudget(), FaultWorkers: faultWorkers()}
	s.scratch, err = vmmachine.NewScratch(ctx, filepath.Join(config.ScratchDir, "vmm"), config.Disks)
	if err != nil {
		return nil, fmt.Errorf("vmm scratch: %w", err)
	}
	// The page cache's disk is scratch too: a restart is a host loss, so it
	// starts empty, and nothing on it is ever durable.
	if config.CacheDiskBytes > 0 {
		s.cacheDisk, err = config.Disk.Open(ctx, "cache",
			platform.OpenOptions{Create: true, Truncate: true, Permissions: 0o600})
		if err != nil {
			return nil, fmt.Errorf("the page cache's disk: %w", err)
		}
	}
	s.host, err = StartHost(ctx, Config{
		// The deployment's prefix is the object store's own, so nothing below
		// it carries the prefix a second time.
		// The pager pushes a full dirty budget back through the host: a
		// checkpoint out of the interval's turn, and a deliberate stop where
		// even that cannot admit the guest's stores.
		Resources: s.resources, ObjectStore: s.objects, Network: config.Network, Pagers: s.pagers,
		Clock:              s.clock,
		Entropy:            config.Entropy,
		CacheBytes:         config.CacheBytes,
		Cache:              checkpoint.CacheConfig{Disk: s.cacheDisk, DiskBytes: config.CacheDiskBytes},
		CheckpointInterval: config.CheckpointInterval,
		LossWindow:         config.LossWindow,
		FlushBound:         config.FlushBound,
		// The page server's budgets are sized against the largest page either
		// pager serves; what a reply is counted in is the page of the volume it
		// answers for.
		Migration: MigrationConfig{Address: s.pageAddress(),
			PageSize: int(max(ram.PageSize(), pmem.PageSize())),
			StartVM:  s.startReceived},
		MachineClosed: s.forgetClosed,
	})
	if err != nil {
		return nil, fmt.Errorf("assembling the host: %w", err)
	}
	// The guest images are imported before this host says it is ready: an
	// import is a read of the whole image and a checkpoint of it, and a create
	// that landed on a host still doing it would wait that out inside the
	// request. It runs behind the API so that a host that is starting can still
	// be asked what it is doing.
	s.notReady(errors.New("the guest images have not been imported yet"))
	go s.importing(ctx)
	started = true
	slog.InfoContext(ctx, "host: assembled", "host", config.PodName,
		"pages", s.pageAddress(),
		"fault_workers", s.connection.FaultWorkers, "max_vmas", s.connection.MaxVMAs)
	return s, nil
}

// startPager builds one of this host's pagers: its own arena — the HugeTLB
// pool's memory for a 2 MiB page, ordinary memory for RAM's 4 KiB one — its
// own spill file and its own configuration. A restart is a host
// loss, so the spill file starts empty; the pager sizes it to the dirty pages
// its cap allows. Each is logged with the bounds the node chose for it, so what
// a host gave each kind is on the record.
func (s *supervisor) startPager(ctx context.Context, kind pagerSlot, cfg vmmemory.Config) (*vmmemory.Host, error) {
	// The pager makes the arena's files. Each is sized to its addresses, not to
	// the memory it may hold: it is a sparse file, and a pager that places a
	// private page at the offset it has within its range owns far more of the
	// first than of the second.
	arena, err := vmmemory.NewLinuxArena(cfg.PageSize)
	if err != nil {
		return nil, fmt.Errorf("%s arena of %d-byte pages: %w", kind, cfg.PageSize, err)
	}
	s.arenas[kind] = arena
	spill, err := s.config.Disk.Open(ctx, "spill-"+string(kind),
		platform.OpenOptions{Create: true, Truncate: true, Permissions: 0o600})
	if err != nil {
		return nil, fmt.Errorf("%s spill file: %w", kind, err)
	}
	s.spills[kind] = spill
	pager, err := vmmemory.New(ctx, s.resources, cfg, arena, spill)
	if err != nil {
		return nil, fmt.Errorf("%s pager of %d offsets for %d pages of %d bytes: %w",
			kind, cfg.Offsets(), cfg.ResidentPages, cfg.PageSize, err)
	}
	slog.InfoContext(ctx, "host: a pager was assembled", "kind", string(kind),
		"page_bytes", cfg.PageSize, "resident_pages", cfg.ResidentPages,
		"arena", cfg.Arena.String(), "arena_offsets", cfg.Offsets(), "huge_pages", arena.HugePolicy(),
		"logical_pages", cfg.LogicalPages, "dirty_pages", cfg.DirtyPages,
		"loss_window", cfg.LossWindow.String(), "concurrent_io", cfg.ConcurrentIO,
		"read_ahead_pages", cfg.ReadAheadPages, "write_ahead_pages", cfg.WriteAheadPages,
		"settle_workers", cfg.SettleWorkers)
	return pager, nil
}

// notReady records why this host cannot take work yet.
func (s *supervisor) notReady(cause error) { s.importErr.Store(&cause) }

func (s *supervisor) Ready(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if cause := s.importErr.Load(); cause != nil {
		return *cause
	}
	return nil
}

// Live reports whether this process can still serve: it has not closed, and
// neither has the host it assembled. A host closes itself when its own context
// is cancelled, so a process whose work is over answers the liveness probe with
// the reason rather than with the mux's unconditional yes.
func (s *supervisor) Live(context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if s.host != nil && s.host.Status().Closed {
		return ErrClosed
	}
	return nil
}

func address(ip string, port int) platform.Address {
	return platform.Address(net.JoinHostPort(ip, strconv.Itoa(port)))
}

func (s *supervisor) pageAddress() platform.Address {
	return address(s.config.PodIP, s.config.PagePort)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func (s *supervisor) Status(ctx context.Context) (hostapi.Status, error) {
	status := s.host.Status()
	ram, err := s.pagerReport(ctx, ramPager, status.LogicalPagesFree.RAM)
	if err != nil {
		return hostapi.Status{}, err
	}
	pmem, err := s.pagerReport(ctx, pmemPager, status.LogicalPagesFree.PMEM)
	if err != nil {
		return hostapi.Status{}, err
	}
	ephemeral, err := s.pagerReport(ctx, ephemeralPager, status.LogicalPagesFree.Ephemeral)
	if err != nil {
		return hostapi.Status{}, err
	}
	records, err := s.records(ctx)
	if err != nil {
		return hostapi.Status{}, err
	}
	resources := status.Resources
	report := hostapi.Status{
		Host: s.config.PodName, PageAddress: string(s.pageAddress()),
		Running: s.host.Machines(), Serving: status.Serving,
		Outstanding: status.Outstanding, VMs: records,
		Templates: s.templateReport(),
		Pager: hostapi.Pager{RAM: ram, PMEM: pmem, Ephemeral: ephemeral,
			CommittedBytes: s.committed()},
		Pages: hostapi.Pages{Requests: status.Pages.Requests, Served: status.Pages.Served,
			Absent: status.Pages.Absent, Refused: status.Pages.Refused},
		Resources: hostapi.Resources{MemoryLimit: resources.Limit, MemoryUsed: resources.Used,
			CacheLimit: status.CacheLimit, CacheUsed: status.Cache.ResidentBytes,
			CacheDiskLimit: status.Cache.Disk.LimitBytes, CacheDiskUsed: status.Cache.Disk.UsedBytes},
		Store: apiStore(s.objects.Traffic()),
	}
	if report.Running == nil {
		report.Running = []string{}
	}
	if report.Serving == nil {
		report.Serving = []string{}
	}
	if report.Outstanding == nil {
		report.Outstanding = map[string]int{}
	}
	return report, nil
}

// Stored lists what one tenant's VMs hold in the object store. The store is
// already scoped to the deployment's prefix, so the report is given none of its
// own, and the listing counts in this host's store traffic like any other call.
func (s *supervisor) Stored(ctx context.Context, tenant string) (hostapi.Stored, error) {
	vms, err := volume.StoredBytes(ctx, s.objects, platform.ObjectPrefix{}, tenant)
	if err != nil {
		return hostapi.Stored{}, err
	}
	return hostapi.Stored{Tenant: tenant, VMs: vms}, nil
}

func (s *supervisor) Kept(ctx context.Context, id string) (hostapi.KeptResult, error) {
	return s.host.Kept(ctx, id)
}

func (s *supervisor) Release(ctx context.Context, id string, checkpoint uint64) error {
	return s.host.Volumes().Release(ctx, id, checkpoint)
}

// committed is the guest RAM the VMs this host runs have between them, which is
// what a placement measures this host by. It is each VM's RAM volume, which is
// the size its template fixed and which a fork inherits, whether or not a byte
// of it is resident: the arena is a cache, and a page of a VM that has gone
// stays in it until the memory is needed, so residency says what this host has
// touched rather than what it has promised.
func (s *supervisor) committed() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total uint64
	for _, m := range s.machines {
		if ram := m.vm.Volume(vmmachine.RAMVolume); ram != nil {
			total += ram.Size()
		}
	}
	return total
}

// templateReport describes the guest images this host can create VMs from: every
// configured one, in name order, imported or not — what a placement needs to
// know is what a VM created here would cost, which the configuration says
// before any image has been read — and then, by identity, every other template
// this host has imported on request or opened for a create.
func (s *supervisor) templateReport() []hostapi.Template {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := make([]hostapi.Template, 0, len(s.config.Templates)+len(s.byID))
	configured := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(s.config.Templates)) {
		entry := hostapi.Template{Name: name, MemoryBytes: s.config.Templates[name].MemoryBytes}
		if imported := s.templates[name]; imported != nil {
			entry.ID, entry.Imported = imported.ID(), true
			configured[imported.ID()] = true
		}
		report = append(report, entry)
	}
	for _, id := range slices.Sorted(maps.Keys(s.byID)) {
		if !configured[id] {
			report = append(report, templateEntry(s.byID[id], id))
		}
	}
	return report
}

// templateEntry reports one template by the name a create selects it with:
// the RAM a VM created from it starts with, which is its own RAM volume.
func templateEntry(template *ImportedTemplate, name string) hostapi.Template {
	return hostapi.Template{Name: name, ID: template.ID(),
		MemoryBytes: template.Point.Size(vmmachine.RAMVolume), Imported: true}
}

// pagerReport is one pager's half of the status: its own page, its own arena
// and budgets, and the sharing it holds. The gauges come from that pager alone,
// so the two halves are never a sum of readings taken at different moments of
// one pager.
func (s *supervisor) pagerReport(ctx context.Context, slot pagerSlot, free int) (hostapi.PagerKind, error) {
	pager := slot.of(s.pagers)
	if pager == nil {
		return hostapi.PagerKind{}, nil
	}
	stats, err := pager.Stats(ctx)
	if err != nil {
		return hostapi.PagerKind{}, err
	}
	sharing, err := pager.Sharing(ctx)
	if err != nil {
		return hostapi.PagerKind{}, err
	}
	gauge := sharing.Pmem
	arena := s.config.ArenaBytes.PMEM
	switch slot {
	case ramPager:
		gauge, arena = sharing.Ram, s.config.ArenaBytes.RAM
	case ephemeralPager:
		arena = s.config.Ephemeral.ArenaBytes
	}
	return hostapi.PagerKind{
		PageBytes:     int(pager.PageSize()),
		ArenaPages:    int(uint64(arena) / pager.PageSize()),
		ResidentPages: stats.ResidentPages,
		DirtyPages:    stats.DirtyPages, LogicalPages: stats.LogicalPages,
		LogicalPagesFree: free,
		Sharing: hostapi.Sharing{UniqueBytes: gauge.UniqueBytes, MappedBytes: gauge.MappedBytes,
			SavedBytes: gauge.SavedBytes},
		SharedPages: stats.IdentityHits, Faults: stats.Faults,
		Evictions: stats.Evictions, Spills: stats.Spills,
	}, nil
}

// records describes every VM this host holds a handle on.
func (s *supervisor) records(ctx context.Context) ([]hostapi.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]hostapi.VM, 0, len(s.machines))
	for _, id := range slices.Sorted(maps.Keys(s.machines)) {
		m := s.machines[id]
		status := m.vm.Status()
		// What this host would cost the VM in time, beside what it would cost it
		// in bytes: the host is the only thing that has both halves, since the
		// window is measured across every memory region the VM maps.
		window, waiting := s.host.LossWindow(id)
		// The same is true of what the VM holds that nothing shares: its memory regions
		// are the pager's and this host is what knows they are one VM's.
		private, err := s.host.PrivateBytes(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, hostapi.VM{ID: id, Template: m.template, Host: s.config.PodName,
			VCPUs:      m.vm.VCPUs(),
			Checkpoint: status.Checkpoint.Sequence, Epoch: status.Epoch, DirtyBytes: status.DirtyBytes,
			LossWindow: window, Waiting: waiting, PrivateBytes: private})
	}
	return records, nil
}

// ---------------------------------------------------------------------------
// Creating, opening and forking
// ---------------------------------------------------------------------------

// Close releases this host in the order the hosting contract requires: the VMM
// processes, then the VM handles and the page server, then the pager, and only
// then the arena and the spill file the pager was using.
func (s *supervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	running := slices.Collect(maps.Values(s.machines))
	clear(s.machines)
	s.mu.Unlock()

	var errs []error
	for _, m := range running {
		// The host stops running it before its process ends: an exit this
		// shutdown asked for is not one the host reports as a death.
		if s.host != nil {
			s.host.RemoveMachine(m.vm.ID())
		}
		if err := m.process.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the VMM of %s: %w", m.vm.ID(), err))
		}
	}
	if s.host != nil {
		// A VM this host handed over and is still serving pages for holds
		// pages no checkpoint has. Exiting loses them either way — that is
		// what a drain exists to prevent — so they are released here rather
		// than left attached to a pager that is about to close.
		if serving := s.host.Status().Serving; len(serving) > 0 {
			slog.WarnContext(ctx, "host: exiting while still serving migrated pages",
				"vms", serving)
			for _, id := range serving {
				if err := s.host.Abandon(id); err != nil {
					errs = append(errs, fmt.Errorf("abandoning the migrated %s: %w", id, err))
				}
			}
		}
		// Closing the host publishes a final checkpoint of every VM it still
		// holds, which is what makes an orderly shutdown lose nothing.
		if err := s.host.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("closing the host: %w", err))
		}
	}
	if s.scratch != nil {
		if err := s.scratch.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the VMM scratch: %w", err))
		}
	}
	// The host closed the page cache, and every pull with the VMs it ran.
	if s.cacheDisk != nil {
		if err := s.cacheDisk.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the page cache's disk: %w", err))
		}
	}
	// Every pager closes, each before its own arena and spill file. A pager that
	// would not close keeps its arena: unproven allocations stay charged, and an
	// arena must never be closed under a pager that may still hold it. The other
	// pagers are still released, because leaving them attached to a process that
	// is exiting helps nothing.
	for _, kind := range pagerSlots {
		pager := kind.of(s.pagers)
		if pager != nil {
			if err := pager.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("closing the %s pager: %w", kind, err))
				continue
			}
		}
		if arena := s.arenas[kind]; arena != nil {
			if err := arena.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing the %s arena: %w", kind, err))
			}
		}
		if spill := s.spills[kind]; spill != nil {
			if err := spill.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing the %s spill file: %w", kind, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// The machines this host holds
// ---------------------------------------------------------------------------

// running is the machine of a VM this host runs, and the error a client gets
// when it asks the wrong host.
func (s *supervisor) running(id string) (*machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.machines[id]; m != nil {
		return m, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotRunning, id)
}

// absent refuses an identity this host already runs, rather than replacing a
// live guest with another.
func (s *supervisor) absent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.machines[id] != nil {
		return fmt.Errorf("%w: %s", ErrRunning, id)
	}
	return nil
}

func (s *supervisor) remember(m *machine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if existing := s.machines[m.vm.ID()]; existing != nil {
		return fmt.Errorf("%w: %s", ErrRunning, m.vm.ID())
	}
	s.machines[m.vm.ID()] = m
	return nil
}

// forgetClosed drops a VM the host gave up on its own: a later writer took its
// control record, or its VMM process ended. The host has already stopped the
// VMM, released the volumes and said why, so this only removes the bookkeeping
// that would otherwise report a guest this process no longer runs.
func (s *supervisor) forgetClosed(id string) {
	s.forget(id)
	slog.Warn("host: forgetting a VM this host no longer runs", "vm", id)
}

func (s *supervisor) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.machines, id)
}

func (s *supervisor) record(m *machine) hostapi.VM {
	status := m.vm.Status()
	record := hostapi.VM{ID: m.vm.ID(), Template: m.template, Host: s.config.PodName, VCPUs: m.vm.VCPUs(),
		Checkpoint: status.Checkpoint.Sequence, Epoch: status.Epoch, DirtyBytes: status.DirtyBytes}
	if pulled, marked := s.host.Pulled(m.vm.ID()); marked {
		record.Pull = &hostapi.Pull{Bytes: pulled.Bytes, Pulled: pulled.Pulled, Done: pulled.Done}
		if pulled.Err != nil {
			record.Pull.Error = pulled.Err.Error()
		}
	}
	return record
}

// since is how long ago this supervisor's clock says t was, as the API reports
// durations. Every duration in a response goes through here rather than through
// the standard library, so a simulated host's numbers come from the clock it
// was given.
func (s *supervisor) since(t time.Time) hostapi.Seconds { return hostapi.Of(s.clock.Since(t)) }
