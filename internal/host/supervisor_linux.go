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

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/resource"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

const (
	// rootVolume is the PMEM device the guest boots from, and ramVolume its
	// RAM. A VM's regions bind to volumes by these names.
	rootVolume = "root"
	// consoleWindowBytes bounds one console read, which is a window on a
	// disposable ring buffer rather than a stream.
	consoleWindowBytes = 256 << 10
	// drainPoll is how often a drain asks whether the pages it handed over have
	// arrived. A drain is finished when nothing is left to serve.
	drainPoll = 250 * time.Millisecond
	// drainTimeout bounds the whole drain and drainVMTimeout one VM's handover
	// inside it; drainConcurrency is how many are handed over at once. The
	// deployment's termination grace period is the preStop hook's own bound plus
	// the shutdown behind it — ninety and sixty, which is the manifest's hundred
	// and fifty — and this is under the hook's, so a drain that runs to its bound
	// still answers the hook and still leaves the process the whole of that
	// shutdown to publish a final checkpoint of every VM that did not move.
	//
	// The caller is a preStop hook and carries no deadline of its own, and the
	// orchestrator drives both halves of every migration, so an orchestrator
	// that is wedged answers none of these calls. Bounding each one is what
	// turns that into a drain that moved some of its VMs rather than a pod
	// killed with all of their pages still on it.
	drainTimeout     = 80 * time.Second
	drainVMTimeout   = 60 * time.Second
	drainConcurrency = 4
	// drainReportTimeout bounds one report to the orchestrator. A report is a
	// diagnostic, and it is sent on a context of its own so that the VM whose
	// handover just ran out of time is still reported as having done so.
	drainReportTimeout = 5 * time.Second
	// guestTimeout bounds one request to a guest's agent. It is longer than
	// the agent's own longest command, so a command that is killed is killed
	// by the guest, which can say so, rather than by a host that cannot.
	guestTimeout = 11 * time.Minute
	// guestCID is the context id every guest knows itself by on its own
	// virtio-vsock device. A VM's vsock is a private channel between it and the
	// process running it, so the name is the guest's alone and the same one
	// serves every VM on the host.
	guestCID = 3
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
	arena     *vmmemory.LinuxArena
	// connection is what every session this host opens is configured with: the
	// node's fault-worker and mapping-count bounds, which no VM varies.
	connection vmmemory.ConnectionConfig
	spill      platform.File
	pager      *vmmemory.Host
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
	closed    bool
}

var _ Service = (*supervisor)(nil)

// Start assembles this host: the resource budget, the object store, the
// pager over a HugeTLB arena and a spill file, the VMM scratch, and the
// host that opens VMs and serves the pages of the ones it hands over.
func Start(ctx context.Context, config SupervisorConfig) (Service, error) {
	s := &supervisor{config: config, clock: platform.ClockOr(config.Clock), templateMu: ctxsync.NewMutex(),
		machines: map[string]*machine{}, templates: map[string]*ImportedTemplate{},
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

	// Huge pages are what the pager's arena is made of. The pod's mount is what the kubelet
	// grants its HugeTLB allotment through, so its absence means the arena
	// cannot be allocated at all.
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
	s.arena, err = vmmemory.NewLinuxArena(int(config.ArenaBytes / vmmemory.PageSize))
	if err != nil {
		return nil, fmt.Errorf("hugetlb arena of %d bytes: %w", config.ArenaBytes, err)
	}
	// A restart is a host loss, so the spill file starts empty; the pager sizes
	// it to the dirty pages its cap allows.
	s.spill, err = config.Disk.Open(ctx, "spill", platform.OpenOptions{Create: true, Truncate: true, Permissions: 0o600})
	if err != nil {
		return nil, fmt.Errorf("spill file: %w", err)
	}
	pager := pagerConfig(config)
	s.pager, err = vmmemory.New(ctx, s.resources, pager, s.arena, s.spill)
	if err != nil {
		return nil, fmt.Errorf("pager: %w", err)
	}
	// One session's bounds are the node's too: how many faults it serves at a
	// time, and the mapping-count budget its replacements are admitted against.
	s.connection = vmmemory.ConnectionConfig{MaxVMAs: vmaBudget(), FaultWorkers: faultWorkers()}
	s.scratch, err = vmmachine.NewScratch(ctx, filepath.Join(config.ScratchDir, "vmm"), config.Disks)
	if err != nil {
		return nil, fmt.Errorf("vmm scratch: %w", err)
	}
	s.host, err = StartHost(ctx, Config{
		// The deployment's prefix is the object store's own, so nothing below
		// it carries the prefix a second time.
		// The pager pushes a full dirty budget back through the host: a
		// checkpoint out of the interval's turn, and a deliberate stop where
		// even that cannot admit the guest's stores.
		Resources: s.resources, ObjectStore: s.objects, Network: config.Network, Pager: s.pager,
		Clock:              s.clock,
		Entropy:            config.Entropy,
		CacheBytes:         config.CacheBytes,
		CheckpointInterval: config.CheckpointInterval,
		LossWindow:         config.LossWindow,
		Migration: MigrationConfig{Address: s.pageAddress(), PageSize: vmmemory.PageSize,
			StartVM: s.startReceived},
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
		"pages", s.pageAddress(), "resident_pages", pager.ResidentPages,
		"logical_pages", pager.LogicalPages, "dirty_pages", pager.DirtyPages,
		"loss_window", pager.LossWindow.String(),
		"concurrent_io", pager.ConcurrentIO, "read_ahead_pages", pager.ReadAheadPages,
		"write_ahead_pages", pager.WriteAheadPages,
		"fault_workers", s.connection.FaultWorkers, "max_vmas", s.connection.MaxVMAs)
	return s, nil
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
	stats, err := s.pager.Stats(ctx)
	if err != nil {
		return hostapi.Status{}, err
	}
	sharing, err := s.pager.Sharing(ctx)
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
		Pager: hostapi.Pager{PageBytes: vmmemory.PageSize,
			ArenaPages:     int(s.config.ArenaBytes / vmmemory.PageSize),
			ResidentPages:  stats.ResidentPages,
			CommittedBytes: s.committed(),
			DirtyPages:     stats.DirtyPages, LogicalPages: stats.LogicalPages,
			LogicalPagesFree: status.LogicalPagesFree,
			SharedPages:      stats.IdentityHits, Faults: stats.Faults,
			Evictions: stats.Evictions, Spills: stats.Spills,
			RAM: apiSharing(sharing.Ram), PMEM: apiSharing(sharing.Pmem)},
		Pages: hostapi.Pages{Requests: status.Pages.Requests, Served: status.Pages.Served,
			Absent: status.Pages.Absent, Refused: status.Pages.Refused},
		Resources: hostapi.Resources{MemoryLimit: resources.Limit, MemoryUsed: resources.Used,
			CacheLimit: status.CacheLimit, CacheUsed: status.Cache.ResidentBytes},
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

// templateReport describes the guest images this host can create VMs from, in
// name order. Every configured template is reported, imported or not: what a
// placement needs to know is what a VM created here would cost, which the
// configuration says before any image has been read.
func (s *supervisor) templateReport() []hostapi.Template {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := make([]hostapi.Template, 0, len(s.config.Templates))
	for _, name := range slices.Sorted(maps.Keys(s.config.Templates)) {
		report = append(report, hostapi.Template{Name: name,
			MemoryBytes: s.config.Templates[name].MemoryBytes,
			Imported:    s.templates[name] != nil})
	}
	return report
}

// apiSharing carries one kind's sharing gauge onto the wire.
func apiSharing(s vmmemory.Sharing) hostapi.Sharing {
	return hostapi.Sharing{UniqueBytes: s.UniqueBytes, MappedBytes: s.MappedBytes,
		SavedBytes: s.SavedBytes}
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
		// window is measured across every region the VM maps.
		window, waiting := s.host.LossWindow(id)
		// The same is true of what the VM holds that nothing shares: its regions
		// are the pager's and this host is what knows they are one VM's.
		private, err := s.host.PrivateBytes(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, hostapi.VM{ID: id, Template: m.template, Host: s.config.PodName,
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
	if s.pager != nil {
		if err := s.pager.Close(ctx); err != nil {
			// Unproven allocations stay charged; the arena is not closed under
			// a pager that may still hold it.
			errs = append(errs, fmt.Errorf("closing the pager: %w", err))
			return errors.Join(errs...)
		}
	}
	if s.arena != nil {
		if err := s.arena.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the arena: %w", err))
		}
	}
	if s.spill != nil {
		if err := s.spill.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the spill file: %w", err))
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
	return hostapi.VM{ID: m.vm.ID(), Template: m.template, Host: s.config.PodName,
		Checkpoint: status.Checkpoint.Sequence, Epoch: status.Epoch, DirtyBytes: status.DirtyBytes}
}

// since is how long ago this supervisor's clock says t was, as the API reports
// durations. Every duration in a response goes through here rather than through
// the standard library, so a simulated host's numbers come from the clock it
// was given.
func (s *supervisor) since(t time.Time) hostapi.Seconds { return hostapi.Of(s.clock.Since(t)) }
