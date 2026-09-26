package simtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/testarena"
	"github.com/semistrict/sproutfs/internal/testpager"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// StoreAddress is the object store's endpoint on the simulated network. The
// store is not carried by that network, so a fault cannot block it by blocking
// a link; naming it as an endpoint and consulting the link state is what lets
// one campaign take the store and the page servers away together, which is the
// combination that puts two writers in the dark at once.
const StoreAddress platform.Address = "store"

// Config is one world: the runtime its dependencies are simulated by, the
// topology it runs, and the tunables every store, manager and pager is built
// with.
type Config struct {
	Runtime  *sim.Runtime
	Topology Topology
	Knobs    knobs.Knobs
	Prefix   platform.ObjectPrefix
	// Namespace separates one world's hosts from another's on a runtime several
	// of them share: their processes, their disks and the addresses their page
	// servers listen on. Empty is the only world on its runtime.
	Namespace string
	// Log is where the world says what it did. Nil discards it.
	Log func(format string, args ...any)
	// Admit is what every concurrent memory region seal of a capture passes through
	// before it happens, which is how a scheduled scenario orders two seals of
	// one pause against each other. Nil seals them in the world's own order.
	Admit func(ctx context.Context, id string) error
	// CheckpointInterval is what every host's checkpoint loop runs on. Zero
	// leaves the loop off, which is what a campaign that drives its own
	// checkpoints needs: a checkpoint arriving on its own would race its
	// assertions about exactly what is durable and when. A scenario about the
	// pager's pressure turns it on, because a pager that asks for a checkpoint
	// out of turn needs a loop to ask.
	CheckpointInterval time.Duration
	// ReverseMemoryRegions seals a capture's memory regions in the opposite order. It is the
	// creation order a recorded scenario reverses: the execution it produces
	// must not change, because the order those goroutines are created in is not
	// an input the simulation is allowed to depend on.
	ReverseMemoryRegions bool
}

// World is a running deployment of one topology: every host is a real
// host.Host, inside a simulated process, on a disk of its own, keeping its
// deadlines against a clock of its own and reaching the deployment's object
// store through a view a kill can take away; and every VM that exists has the
// guest that is storing into it and the bytes that guest believes it has.
//
// Nothing in here is concurrent with itself. The driver runs one operation at a
// time and the faults around it are ambient — a blocked link, an unavailable
// store, a host that is gone — so what the model holds is exact whenever an
// assertion looks at it, and a failure names the operation that produced it
// rather than a race between two of them.
type World struct {
	config    Config
	runtime   *sim.Runtime
	hosts     []*hostState
	instances map[string]*instance
	// order is the identities of the instances in the order they came into
	// existence, so everything this world iterates is in a seeded order rather
	// than a map's.
	order []string
	// published is every sequence a writer of this VM established: what a
	// create's root index published, and what every checkpoint that landed
	// published. A control record selecting anything else is a VM whose state
	// nobody wrote.
	published map[string]map[uint64]bool
	// kept is the pause every checkpoint a writer of the VM asked to keep
	// stands for, by sequence: the bytes it published, and the VMM state it
	// carries or not. Whether it is still kept is the control record's to say;
	// what a checkpoint the record keeps must read as is this.
	kept map[string]map[uint64]durableState
	// ctx is the world's own context, which every host process is started from.
	ctx context.Context
	// orphans are the identities a fork could not give back: a child whose root
	// never published and whose record the store would not let go of. They are
	// deleted again at every step, because a record nothing can open and
	// nothing can publish under is an identity burnt for good.
	orphans map[string]bool
	// takeovers counts the VMs a host opened again because whatever was running
	// them stopped: a lost host, a migration that could not be undone, a
	// post-copy that did not finish. It is what a test asserts to say that its
	// fault reached the takeover it is about rather than being absorbed.
	takeovers int
	// guestSeq numbers the VMM processes this world has started, which is what
	// names one apart from the next: a VM handed back to a host it already ran
	// on is a new process there, and a scheduler that saw the same identity
	// twice would be ordering two runs of one caller.
	guestSeq int
	// closed reports that Close has run, after which nothing may be driven.
	closed bool
	// mu guards what a kill and the operation it interrupts both touch: which
	// host runs which VM, the guest running it, and the checkpoints it may have
	// come back at. Everything else here is single-threaded — the driver runs
	// one operation at a time — and nothing holds this across an operation, so
	// a kill never waits for the checkpoint it is interrupting.
	mu sync.Mutex
}

// Takeovers is how many times a host has opened a VM again because whatever was
// running it stopped.
func (w *World) Takeovers() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.takeovers
}

// hostState is one host of the deployment: the process it runs inside, the disk
// that process keeps across every restart of itself, the clock its deadlines
// are measured against, and everything that would die with the machine.
type hostState struct {
	name    string
	address platform.Address
	// pages is where this host serves the pages a destination or a child
	// fetches.
	pages platform.Address

	process *sim.Process
	disk    *sim.Disk
	clock   *sim.Clock
	config  host.Config
	host    *host.Host
	pager   *pager
	// objects is this host's own view of the deployment's object store, which a
	// kill takes away before the process ends so that nothing it had in flight
	// can still land.
	objects *hostStore
	// dead is this host's own end: a process that is gone reaches the store no
	// more, so its shutdown publishes nothing.
	dead *atomic.Bool

	// incarnation counts how many times this host has been started, which names
	// its guests apart: a restarted host is a new process on the same machine.
	incarnation int
	// down reports a host the campaign has taken away. Nothing may be driven on
	// it until it is started again, and crashed reports that what took it away
	// was a crash rather than a close.
	down    bool
	crashed bool
	// gone closes when this incarnation of the host ends, however it ended. It
	// is what the deployment knows and a destination cannot: a host that was
	// holding the pages no checkpoint of a migrated VM has is a host whose loss
	// took those pages with it, and the migration waiting for them ends here.
	gone chan struct{}
	// faults is what this host's connections to a page server do while a fault
	// is on it.
	faults connFaults
	// refuseStart fails this host's half of a receive before the guest is
	// started, which is the destination that could not start the VMM it was
	// handed. startsBeforeRefusal is how many it still takes before it does,
	// which is what puts the refusal inside a fan-out rather than at its head:
	// a destination that took the first child and then could take no more.
	refuseStart         error
	startsBeforeRefusal int
	// started records the guest a receive built, so the world can adopt the
	// model of a VM this host took in, and guests every VMM process this
	// incarnation runs, which is what a kill ends: a machine whose host died is
	// a process whose pages are gone.
	started map[string]*guest
	guests  []*guest
	mu      sync.Mutex
}

// instance is one VM that exists: the host that owns it, the guest storing
// into it, and the bytes the last checkpoint that landed made durable.
type instance struct {
	spec  VMSpec
	host  int
	guest *guest
	// present reports a VM some host holds a handle on, and incarnation the
	// incarnation of that host it was placed on. A VM nobody is running — and a
	// VM placed on a host that has been lost and started again since, which is
	// what an operation racing a kill leaves behind — is one the next Settle
	// opens again.
	present     bool
	incarnation int
	// stopped reports a VM the deployment stopped on purpose. It is the one
	// reason for a VM to be running nowhere that is not something to repair, so
	// it is what keeps Settle from starting it again: a stop whose VM came back
	// by itself at the next step would be no stop at all.
	stopped bool
	// writes is when each store this VM and its ancestors have made happened, on the
	// clock of the host that took it, oldest first. It is what the loss window
	// is measured over: a recovery rewinds every write past the checkpoint it
	// came back at, and the window bounds how far apart the first and the last
	// of those may be.
	writes []time.Time
	// rewound is the writes the last recovery of this VM took back, which
	// VerifyLossWindow requires to span no more than the window allows.
	rewound []time.Time
	// durables are the checkpoints this VM may come back at, oldest first: the
	// last one that landed, plus every later one whose publication was
	// interrupted and may or may not have landed. A VM whose host is lost reads
	// back as exactly one of them — never a mixture, never bytes no guest wrote
	// — and the one it read as is the only one left afterwards.
	durables []durableState
	// sealed and unchanged are what the last checkpoint of this VM sealed and
	// what the settle behind its pause dropped: pages a write fault took
	// writable and the guest never stored into, which that checkpoint therefore
	// publishes none of.
	sealed, unchanged int
}

// durableState is the whole of one VM at one checkpoint: the sequence that
// checkpoint was published under, the bytes it published, and the guest's own
// store counter at that moment, which is what the VMM state it carries has to
// restore. The sequence is what says which of them a VM came back at — the
// control record names it — so the bytes are an assertion rather than a search.
//
// stateless is a checkpoint that carries no VMM state — the root a create
// publishes, a checkpoint of the disks alone, a cold start's — which a VM that
// comes back at it is cold booted over rather than restored from.
type durableState struct {
	sequence  uint64
	model     map[string][]byte
	writes    int64
	stateless bool
}

// Start builds the world one topology describes: every host, then every VM that
// is not a fork, on the host the topology puts it on. The forks are not created
// here — a fork is a pause of a running parent, so it happens when the
// schedule reaches it.
func Start(ctx context.Context, config Config) (*World, error) {
	return start(ctx, config)
}

// MustStart is Start for a test: it fails the test when the world cannot be
// built, and closes the world when the test ends, whether the test reached its
// own close or failed before it. A campaign that fails in the middle otherwise
// leaves its hosts running, and a synctest bubble whose test has exited while
// those hosts still wait on simulated time reports a deadlock in place of the
// failure — and takes every seed after it in the process down with it.
func MustStart(t testing.TB, ctx context.Context, config Config) *World {
	t.Helper()
	w, err := start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The test's own context is cancelled before its cleanups run; the
		// close still has to reach its hosts and let their disks finish.
		if err := w.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("closing the world at the end: %v", err)
		}
	})
	return w
}

func start(ctx context.Context, config Config) (*World, error) {
	if config.Runtime == nil || len(config.Topology.Hosts) == 0 {
		return nil, errors.New("simtest: a world needs a runtime and a topology")
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	w := &World{config: config, runtime: config.Runtime, ctx: ctx,
		instances: map[string]*instance{}, published: map[string]map[uint64]bool{},
		kept: map[string]map[uint64]durableState{}}
	for index := range config.Topology.Hosts {
		id := config.Namespace + config.Topology.Hosts[index]
		h := &hostState{name: id, address: platform.Address(id),
			pages: platform.Address(id + "/pages"), dead: new(atomic.Bool),
			started: map[string]*guest{}}
		h.clock = w.runtime.NewClock(id)
		h.disk = w.runtime.NewDisk(id,
			// A killed host's disk comes back with its unsynced modifications
			// resolved rather than restored, so a restart that read across its
			// own crash would read bytes nobody wrote.
			sim.DiskConfig{PowerLossFaults: true})
		h.process = w.runtime.NewProcess(sim.ProcessConfig{ID: id, Disk: h.disk})
		h.objects = &hostStore{ObjectStore: w.runtime.ObjectStore(), network: w.runtime.Network(),
			from: h.address, dead: h.dead}
		w.hosts = append(w.hosts, h)
		h.config = w.hostConfig(h)
		if err := w.launch(h); err != nil {
			return nil, err
		}
	}
	for _, spec := range config.Topology.VMs {
		if spec.IsFork() {
			continue
		}
		if err := w.create(ctx, spec); err != nil {
			return nil, fmt.Errorf("creating %s: %w", spec.ID, err)
		}
	}
	return w, nil
}

// Runtime is the simulated world these hosts depend on.
func (w *World) Runtime() *sim.Runtime { return w.runtime }

// Topology is what this world runs.
func (w *World) Topology() Topology { return w.config.Topology }

// Hosts is how many hosts the deployment has.
func (w *World) Hosts() int { return len(w.hosts) }

// Host is the real host at an index, which is what a scenario that drives
// something this world has no operation for reaches through.
func (w *World) Host(index int) *host.Host { return w.hosts[index].host }

// Pages is where a host serves the pages a destination or a child fetches.
func (w *World) Pages(index int) platform.Address { return w.hosts[index].pages }

// Clock is the passage of time one host keeps its deadlines against, which is
// how a hold written in checkpoint intervals is reached by a decision rather
// than by waiting four minutes for it.
func (w *World) Clock(index int) *sim.Clock { return w.hosts[index].clock }

// Running is every VM that exists, in the order the VMs came into existence.
func (w *World) Running() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.order)
}

// Started is every VM a host is running right now, in the order the VMs came
// into existence. It is what Running is not: Running is every VM that exists,
// and a VM that was stopped, whose host was lost, or that is between two hosts
// exists without anything running it.
func (w *World) Started() []string {
	started := make([]string, 0, len(w.order))
	for _, id := range w.Running() {
		if w.live(id) != nil {
			started = append(started, id)
		}
	}
	return started
}

// Stopped is every VM the deployment stopped on purpose, in the same order.
// They are the ones a start applies to and nothing else will bring back.
func (w *World) Stopped() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	stopped := make([]string, 0, len(w.order))
	for _, id := range w.order {
		if in := w.instances[id]; in != nil && in.stopped {
			stopped = append(stopped, id)
		}
	}
	return stopped
}

// HostOf reports the host the named VM is running on, or -1. A VM that exists
// without anything running it — stopped, lost with its host, or between two
// hosts after a takeover that could not open it — is running nowhere, whatever
// host it was last at or was last tried on: a campaign that waits for a VM to
// reach a host must not be told it did by an attempt that failed.
func (w *World) HostOf(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if in, ok := w.instances[id]; ok && in.present {
		return in.host
	}
	return -1
}

// Exists reports whether the named VM is one this world knows: created and not
// deleted, whether or not anything is running it.
func (w *World) Exists(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.instances[id]
	return ok
}

// Address is one host's own endpoint on the simulated network, which is what a
// campaign that blocks every link among them names.
func (w *World) Address(index int) platform.Address { return w.hosts[index].address }

// Published is every sequence a writer of one VM established, which is the
// whole of what that VM's state may be built out of.
func (w *World) Published(id string) map[uint64]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.published[id])
}

func (w *World) logf(format string, args ...any) { w.config.Log(format, args...) }

// hostConfig is one host's configuration, which does not change across the
// incarnations of that host: a restart is a new process on the same machine,
// reaching the same store through the same view and serving the same address.
func (w *World) hostConfig(h *hostState) host.Config {
	k := w.config.Knobs
	return host.Config{
		Network:      &hostNetwork{Network: w.runtime.Network(), local: h.address, faults: &h.faults},
		Resources:    testresource.New(),
		ObjectStore:  h.objects,
		ObjectPrefix: w.config.Prefix,
		Clock:        h.clock,
		Entropy:      w.runtime.NewEntropy(h.name),
		Volumes:      host.VolumeConfig{MaxWriteBytes: k.MaxWriteBytes, MaxOpenVMs: k.MaxOpenVMs},
		Migration: host.MigrationConfig{Address: h.pages, PageSize: PMEMPage,
			DrainConcurrency: k.DrainConcurrency, StartVM: w.starter(h)},
		// A campaign drives every checkpoint itself and reaches every hold
		// deadline by advancing the clock, so neither loop arms anything of its
		// own: what fires on one of these clocks is a hold. A scenario about the
		// pager's pressure turns the checkpoint loop on, because the checkpoint
		// a waiting store asks for is one only a loop takes.
		CheckpointInterval: checkpointInterval(w.config.CheckpointInterval),
		EpochInterval:      -1,
		// The bound the pager keeps is the bound this host reports and schedules
		// its retries by, so both come from the one knob. Zero disables it there
		// and is the default here, which is why it crosses as a negative value.
		LossWindow: hostLossWindow(k.LossWindow),
	}
}

// checkpointInterval is what a world's hosts checkpoint on: the interval a
// scenario asked for, or the disabled loop a campaign needs.
func checkpointInterval(configured time.Duration) time.Duration {
	if configured <= 0 {
		return -1
	}
	return configured
}

// lossWindowOf is the window one of a simulated host's pagers keeps, which is
// what a real host gives it: its disks keep the knob's, and its RAM none, since
// the interval checkpoints disks alone and nothing it takes would end a RAM
// page's window.
func lossWindowOf(kind vmmemory.MemoryRegionKind, window time.Duration) time.Duration {
	if kind == vmmemory.Ram {
		return 0
	}
	return window
}

// hostLossWindow spells a knob's window the way a host reads one: zero is the
// knob turning the bound off, and zero is the host's own default, so the two
// meet through the negative value that disables it.
func hostLossWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return -1
	}
	return window
}

// starter is one host's StartVM: the guest a destination builds for a VM it
// takes in, in the arena of whatever incarnation of that host is running when
// the VM arrives.
func (w *World) starter(h *hostState) host.StartFunc {
	return func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (host.Machine, error) {
		h.mu.Lock()
		refused, p := h.refuseStart, h.pager
		if refused != nil && h.startsBeforeRefusal > 0 {
			// This one is still taken: the refusal is what the destination does
			// from the next guest on.
			h.startsBeforeRefusal--
			refused = nil
		}
		h.mu.Unlock()
		if refused != nil {
			return nil, refused
		}
		g, err := w.newGuest(h, p, vm, backings, state)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.started[vm.ID()] = g
		h.guests = append(h.guests, g)
		h.mu.Unlock()
		return g, nil
	}
}

// launch starts one host inside its own simulated process. The process's
// context is the host's, so a kill cancels every goroutine that host owns;
// whatever else the incarnation owns — its pager, its arena, the spill file on
// its own disk — is built and closed in there too, so a killed host's pages go
// with it.
func (w *World) launch(h *hostState) error {
	ready := make(chan error, 1)
	w.mu.Lock()
	// This incarnation has not ended: a host that is started again is a host
	// whose pages are its own once more.
	h.crashed, h.gone = false, make(chan struct{})
	w.mu.Unlock()
	err := h.process.Start(w.ctx, func(ctx context.Context) {
		w.mu.Lock()
		h.incarnation++
		w.mu.Unlock()
		pager, release, err := w.newPager(ctx, h)
		if err != nil {
			ready <- err
			return
		}
		h.mu.Lock()
		h.pager, h.started, h.guests = pager, map[string]*guest{}, nil
		h.mu.Unlock()
		// The pager is this incarnation's, so the host learns about it here
		// rather than in the configuration the machine keeps across restarts. A
		// host that was never given one answers none of its pager's pressure:
		// every store past the dirty budget or past the loss window would be
		// stalled for want of anything to ask.
		h.config.Pagers = pager.pagers
		started, err := host.StartHost(ctx, h.config)
		h.host, h.down = started, err != nil
		ready <- err
		if err == nil {
			<-ctx.Done()
			// A kill has already taken this host's store away, so the shutdown
			// its own cancellation drives publishes nothing: what the deployment
			// holds is what the crash left. An orderly end reaches the store and
			// publishes what it still holds.
			_ = started.Close(context.Background())
		}
		// A machine that died takes its memory with it: there is nothing to
		// give back, and nothing left to wait for. Closing the pager anyway
		// would hold its lock across a disk operation while whatever was still
		// faulting a page through it waits for that lock — which is not a wait
		// simulated time can pass, so the world would stop rather than the host.
		w.mu.Lock()
		crashed := h.crashed
		w.mu.Unlock()
		if !crashed {
			release()
		}
	})
	if err != nil {
		return err
	}
	return <-ready
}

// newPager builds one incarnation's three pagers over that host's own disk: RAM
// at its page, PMEM at its own, and the ephemeral disks' at PMEM's, each with an
// arena and a spill file of its own. The knobs describe one pager, so each is
// given what they say — a campaign that wants a tight arena gets a tight arena
// of each. The ephemeral pager's dirty budget is its logical one, as a real
// host's is. The spill files are the only local state a host keeps, and they
// are scratch by construction: a pager truncates its own at every start, so a
// restart reads none of what its crash left in it.
func (w *World) newPager(ctx context.Context, h *hostState) (*pager, func(), error) {
	k := w.config.Knobs
	p := &pager{arenas: map[*vmmemory.Host]*testpager.Arena{}, runtime: w.runtime}
	// Every pager of the run is built in the arena mode SPROUTFS_ARENA names.
	mode := testarena.MustMode()
	// RAM places a private page at the offset it has within its 2 MiB range, so
	// its arena has an address per logical page — one 512-offset extent per
	// range any memory region may write into — beside the pages it may hold at
	// once. PMEM places nothing, so its offsets and its pages are one number.
	// The arena is sparse either way: an offset costs nothing until a page is
	// put there.
	pagers := []struct {
		name      string
		into      **vmmemory.Host
		pageSize  uint64
		offsets   int
		dirty     int
		window    time.Duration
		ephemeral bool
	}{
		{"ram", &p.pagers.Ram, RAMPage, k.LogicalPages + k.ResidentPages, k.DirtyPages,
			lossWindowOf(vmmemory.Ram, k.LossWindow), false},
		{"pmem", &p.pagers.Pmem, PMEMPage, k.ResidentPages, k.DirtyPages,
			lossWindowOf(vmmemory.Pmem, k.LossWindow), false},
		{"ephemeral", &p.pagers.Ephemeral, PMEMPage, k.ResidentPages, k.LogicalPages, 0, true},
	}
	var spills []platform.File
	// They are released in a fixed order, because what they do on the way out
	// reaches this host's simulated disk: a release that walked a map would give
	// one seed two runs.
	release := func() {
		for index, spill := range spills {
			if memory := *pagers[index].into; memory != nil {
				_ = memory.Close(context.Background())
			}
			_ = spill.Close()
		}
	}
	for _, kind := range pagers {
		spill, err := h.disk.Open(ctx, "spill-"+kind.name, platform.OpenOptions{Create: true})
		if err != nil {
			release()
			return nil, nil, err
		}
		spills = append(spills, spill)
		a := testpager.NewArena(mode)
		memory, err := vmmemory.New(ctx, h.config.Resources, vmmemory.Config{
			PageSize: kind.pageSize, Arena: mode,
			ResidentPages: k.ResidentPages, ArenaOffsets: kind.offsets,
			LogicalPages: k.LogicalPages, DirtyPages: kind.dirty,
			ReadAheadPages: k.ReadAheadPages, WriteAheadPages: k.WriteAheadPages,
			ConcurrentIO: k.ConcurrentIO, LossWindow: kind.window, Ephemeral: kind.ephemeral,
			// The window is measured on this host's own clock, which the
			// simulation moves itself: a pager reading the wall clock would
			// measure a bound written in checkpoint intervals against a
			// machine's idle time.
			Clock: h.clock}, a, spill)
		if err != nil {
			release()
			return nil, nil, err
		}
		*kind.into = memory
		p.arenas[memory] = a
	}
	return p, release, nil
}

// hostNetwork names the host a dial comes from. Real TCP takes an ephemeral
// source port and a host passes no source address at all; the simulator models
// a link between two named endpoints, so a world supplies the name its host
// would have on the wire, and wraps every connection in whatever fault is on
// this host's links.
type hostNetwork struct {
	*sim.Network
	local  platform.Address
	faults *connFaults
}

func (n *hostNetwork) Dial(ctx context.Context, _, to platform.Address) (platform.Conn, error) {
	conn, err := n.Network.Dial(ctx, n.local, to)
	if err != nil || n.faults == nil {
		return conn, err
	}
	return n.faults.wrap(conn), nil
}

// hostStore refuses every object-store operation while the link from its host
// to the store is blocked, while a fault has taken the store from this host
// alone, or while the process that reaches it is gone. The store is shared by
// the whole deployment, so this is how one host loses it while another still
// has it.
type hostStore struct {
	platform.ObjectStore
	network *sim.Network
	from    platform.Address
	// dead is the host's process being gone, and failed a fault that took the
	// store from this host alone whatever the links say.
	dead   *atomic.Bool
	failed bool
	mu     sync.Mutex
}

func (s *hostStore) setFailed(failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = failed
}

func (s *hostStore) blocked() error {
	s.mu.Lock()
	failed := s.failed
	s.mu.Unlock()
	if failed || s.dead.Load() || s.network.Clogged(s.from, StoreAddress) {
		return platform.ErrUnavailable
	}
	return nil
}

func (s *hostStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if err := s.blocked(); err != nil {
		return platform.ObjectMetadata{}, err
	}
	return s.ObjectStore.Head(ctx, key)
}

func (s *hostStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if err := s.blocked(); err != nil {
		return platform.GetResult{}, err
	}
	return s.ObjectStore.Get(ctx, request)
}

func (s *hostStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := s.blocked(); err != nil {
		return platform.PutResult{}, err
	}
	return s.ObjectStore.Put(ctx, request)
}

func (s *hostStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if err := s.blocked(); err != nil {
		return err
	}
	return s.ObjectStore.Delete(ctx, request)
}

func (s *hostStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := s.blocked(); err != nil {
		return platform.ListResult{}, err
	}
	return s.ObjectStore.List(ctx, request)
}

// create makes one VM and starts a guest on it. Every page is written before
// anything else happens, so a missing replay or a bogus zero fallback cannot
// accidentally satisfy the model later.
func (w *World) create(ctx context.Context, spec VMSpec) error {
	h := w.hosts[spec.Host]
	vm, err := h.host.Volumes().Create(ctx, spec.ID, spec.Volumes)
	if err != nil {
		return err
	}
	g, err := w.newGuest(h, h.pager, vm, nil, nil)
	if err != nil {
		return err
	}
	h.running(g)
	in := &instance{spec: spec}
	w.adopt(in)
	w.place(in, spec.Host, g)
	if err := h.host.AddMachine(spec.ID, g); err != nil {
		return err
	}
	// The root index a create publishes is a checkpoint of this VM like any
	// other: it is what the record selects until something else is published,
	// and what it published is a VM whose every page is still zero.
	root := vm.Status().Checkpoint.Sequence
	w.notePublished(spec.ID, root)
	zeros := map[string][]byte{}
	for _, name := range g.names {
		zeros[name] = make([]byte, g.pages[name]*g.pageBytes[name])
	}
	w.offer(in, durableState{sequence: root, model: zeros, stateless: true})
	for _, name := range g.names {
		for page := range uint64(g.pages[name]) {
			before := g.stored()
			if err := g.store(name, page); err != nil {
				return err
			}
			w.noteWrites(in, g, before)
		}
	}
	// The first checkpoint makes those pages durable, so the VM has something
	// to rewind to from its very first moment.
	return w.Checkpoint(ctx, spec.ID)
}

func (w *World) adopt(in *instance) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, known := w.instances[in.spec.ID]; !known {
		w.order = append(w.order, in.spec.ID)
	}
	w.instances[in.spec.ID] = in
}

func (w *World) notePublished(id string, sequence uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sequence == 0 {
		return
	}
	if w.published[id] == nil {
		w.published[id] = map[uint64]bool{}
	}
	w.published[id][sequence] = true
}

// live reports the VM that is running right now: it exists, a host holds it,
// and that host is up. Everything the schedule drives goes through it, so an
// operation on a VM that is between hosts is a step that does nothing rather
// than a nil dereference.
func (w *World) live(id string) *instance {
	in, _ := w.runningVM(id)
	return in
}

// runningVM is live with the VMM process it found. The guest is handed back
// rather than read again through the instance, because a kill may take it away
// between the two reads and what an operation is about is the process it
// started with.
func (w *World) runningVM(id string) (*instance, *guest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	in := w.instances[id]
	if in == nil || !in.present || in.guest == nil {
		return nil, nil
	}
	h := w.hosts[in.host]
	if h.down || in.incarnation != h.incarnation {
		return nil, nil
	}
	return in, in.guest
}

// nextGuest numbers one more VMM process, which is what makes every guest of
// this world a caller of its own to the scheduler.
func (w *World) nextGuest() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.guestSeq++
	return w.guestSeq
}

// eachGuest runs fn against every VMM process the world is running, in the
// order the VMs came into existence.
func (w *World) eachGuest(fn func(*guest)) {
	for _, id := range w.Running() {
		if _, g := w.runningVM(id); g != nil {
			fn(g)
		}
	}
}

// up is the host at an index while it is running, and nil while it is not.
// Everything an operation does to another host goes through it, because the
// host it is about may be being taken away underneath it.
func (w *World) up(index int) *host.Host {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.hosts[index]
	if h.down || h.host == nil {
		return nil
	}
	return h.host
}

// place records where a VM is and what is running it. It is the one write a
// kill and the operation it interrupts both make.
func (w *World) place(in *instance, index int, g *guest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	in.host, in.guest, in.present = index, g, g != nil
	in.incarnation = w.hosts[index].incarnation
}

// vm is the handle the host running one VM holds on it.
func (w *World) vm(in *instance) *volume.VM {
	running := w.up(in.host)
	if running == nil {
		return nil
	}
	for _, open := range running.Volumes().VMs() {
		if open.ID() == in.spec.ID {
			return open
		}
	}
	return nil
}

// VM is the handle the host running one VM holds on it, or nil. It is what a
// scenario that drives a volume directly — a write, a discard, a read of the
// bytes a checkpoint made durable — reaches through.
func (w *World) VM(id string) *volume.VM {
	in := w.live(id)
	if in == nil {
		return nil
	}
	return w.vm(in)
}

// running records one more VMM process this incarnation of a host runs, which
// is the list a kill ends.
func (h *hostState) running(g *guest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.guests = append(h.guests, g)
}

// Store has the named VM's guest write a few pages, which is the work every
// operation of the schedule happens around. The pages are the caller's, drawn
// from the seed.
func (w *World) Store(ctx context.Context, id string, writes int, choose func(limit int) int) error {
	in, g := w.runningVM(id)
	if g == nil {
		return nil
	}
	// Every store is dated, because what a recovery rewinds is the writes past
	// the checkpoint it came back at and the loss window bounds how far apart
	// the first and the last of them may be.
	before := g.stored()
	defer func() { w.noteWrites(in, g, before) }()
	for range writes {
		// Both volumes are written, so a migration has to move more than one
		// memory region's worth of pages on the seeds that have two.
		name := g.names[choose(len(g.names))]
		page := uint64(choose(g.pages[name]))
		// One access in four is a write fault the guest stores nothing
		// through, which is what a cold read and a cache maintenance reach the
		// pager as. The page is copied all the same, and the settle behind the
		// next checkpoint's pause is what decides it was never dirty.
		var err error
		if choose(4) == 0 {
			err = g.takeWritable(ctx, name, page)
		} else {
			err = g.store(name, page)
		}
		switch {
		case err == nil:
		case excused(err):
			// A store into a page this host does not hold has to fetch it
			// first, and a fault has taken away whatever holds it. The store did
			// not happen — the model records nothing — which is what the fault
			// means rather than a byte written wrong.
			w.logf("a page could not be stored into while a fault was on: %v", err)
		default:
			return err
		}
	}
	return nil
}

// excused reports a failure a fault explains rather than a defect: whatever
// holds the page is not answering, so the operation did not happen and the
// model records nothing. A guest that read or wrote the wrong bytes is not one
// of these.
func excused(err error) bool {
	return errors.Is(err, platform.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// StoreAll writes one whole generation into every page of one VM's memory,
// which is what a campaign that reads a recovered VM back as one generation
// needs in it.
func (w *World) StoreAll(id string, value byte) error {
	in, g := w.runningVM(id)
	if g == nil {
		return nil
	}
	before := g.stored()
	defer func() { w.noteWrites(in, g, before) }()
	return g.storeAll(value)
}

// StorePages writes one named byte into each of the named pages of one volume,
// which is what a scenario about where a guest's stores land needs: a pattern
// the caller chose rather than pages drawn from a seed.
func (w *World) StorePages(id, name string, pages []uint64, value byte) error {
	in, g := w.runningVM(id)
	if g == nil {
		return nil
	}
	before := g.stored()
	defer func() { w.noteWrites(in, g, before) }()
	for _, page := range pages {
		if err := g.storeValue(name, page, value); err != nil {
			return err
		}
	}
	return nil
}

// CheckpointDisks publishes what the named VM's guest has written to its disks
// and waits for it to become durable, which is the checkpoint a host's interval
// takes: the guest pauses for the seal of its disks alone, and its memory and
// its VMM state are neither sealed nor published. A VM that comes back at it
// comes back cold — its memory gone, its disks exactly as this pause left them —
// because a checkpoint with no registers is one no guest can be resumed from.
//
// Under a fault that has taken the store away it fails, as Checkpoint does, and
// the pause it sealed stays one the VM may come back at.
func (w *World) CheckpointDisks(ctx context.Context, id string) error {
	return w.checkpointDisks(ctx, id, volume.Terms{})
}

// checkpointDisks is CheckpointDisks on terms: a checkpoint that is kept is
// noted as the pause it stands for, whether or not its publication reported
// landing, because the write that keeps it is the one that selects it.
func (w *World) checkpointDisks(ctx context.Context, id string, terms volume.Terms) error {
	in, g := w.runningVM(id)
	if in == nil {
		return nil
	}
	vm := w.vm(in)
	if vm == nil {
		return nil
	}
	at := durableState{model: g.checkpointed(), writes: g.stored(), stateless: true}
	ckpt, err := host.CaptureDisks(ctx, vm, g, w.hosts[in.host].clock, terms)
	if err != nil {
		return fmt.Errorf("%s: disk capture: %w", id, err)
	}
	at.sequence = ckpt.Ref().Sequence
	if terms.Keep {
		w.noteKept(id, at)
	}
	sealed, _ := ckpt.Sealed()
	w.noteSealed(in, sealed, ckpt.Unchanged())
	w.notePublished(id, at.sequence)
	if err := ckpt.Swept(ctx); err != nil {
		w.offer(in, at)
		return fmt.Errorf("%s: publication: %w", id, err)
	}
	if ckpt.State() != nil {
		return fmt.Errorf("%s: a checkpoint of the disks carries %d state bytes, want none",
			id, len(ckpt.State()))
	}
	w.landed(in, g, at)
	w.notePublished(id, vm.Status().Checkpoint.Sequence)
	return nil
}

// Checkpoint publishes everything the named VM's guest has written and waits
// for it to become durable. Under a fault that has taken the store away it
// fails, which is a checkpoint that did not happen rather than an error: the
// VM goes on running and the bytes it could not publish stay in its pages.
func (w *World) Checkpoint(ctx context.Context, id string) error {
	return w.checkpoint(ctx, id, volume.Terms{})
}

// checkpoint is Checkpoint on terms, and notes a kept one as checkpointDisks
// does.
func (w *World) checkpoint(ctx context.Context, id string, terms volume.Terms) error {
	in, g := w.runningVM(id)
	if in == nil {
		return nil
	}
	vm := w.vm(in)
	if vm == nil {
		return nil
	}
	// The model at the pause is what this checkpoint makes durable. Nothing
	// stores into this guest while the publication runs — the driver is the
	// only thing that stores at all — so the snapshot taken here is exactly
	// what the seal froze.
	at := durableState{model: g.checkpointed(), writes: g.stored()}
	ckpt, err := host.Capture(ctx, vm, g, w.hosts[in.host].clock, terms)
	if err != nil {
		return fmt.Errorf("%s: capture: %w", id, err)
	}
	at.sequence = ckpt.Ref().Sequence
	if terms.Keep {
		w.noteKept(id, at)
	}
	sealed, _ := ckpt.Sealed()
	w.noteSealed(in, sealed, ckpt.Unchanged())
	// The sequence is a writer's whatever the publication does with it: a VM
	// that comes back at it came back at state this writer sealed, and one that
	// comes back at a sequence nobody sealed came back at state nobody wrote.
	w.notePublished(id, at.sequence)
	// The sweep behind the publication runs on a goroutine of its own. A step
	// that returned before it would leave its deletes racing whatever the next
	// step does to the store: a fault that fails the store would take some of
	// them on one run and none on the next, and a seed would not reproduce its
	// work. So the checkpoint is waited for until its sweep has run too, and
	// Swept reports the publication's own outcome.
	if err := ckpt.Swept(ctx); err != nil {
		// A publication that did not report landing may have landed anyway: the
		// store may have taken every object and lost the reply, and the host may
		// have died between the parts and the index. The pause it sealed is
		// therefore one this VM may come back at, and stays one until a later
		// checkpoint of it lands.
		w.offer(in, at)
		return fmt.Errorf("%s: publication: %w", id, err)
	}
	// A capture that published nothing of the guest's state is a checkpoint
	// this VM cannot be restored from: the volume's bytes without the registers
	// that were running over them.
	if len(ckpt.State()) != stateBytes {
		return fmt.Errorf("%s: the checkpoint carries %d state bytes, want %d",
			id, len(ckpt.State()), stateBytes)
	}
	w.landed(in, g, at)
	w.notePublished(id, vm.Status().Checkpoint.Sequence)
	return nil
}

// noteSealed records what one capture's pause took and what the settle behind
// it gave back, which is what a scenario about write faults that store nothing
// asserts on.
func (w *World) noteSealed(in *instance, sealed, unchanged int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	in.sealed, in.unchanged = sealed, unchanged
}

// Sealed reports what the last checkpoint of the named VM sealed and how many
// of those pages the settle found the guest had never stored into, so that the
// checkpoint published neither them nor anything under them.
// PrivateExtents is how many 2 MiB-aligned ranges of one host's RAM memory regions own
// an extent of its arena's offset space, which is how many hold a private page.
// It is addresses and not memory: the pages such a range holds are whatever the
// guest stored into.
func (w *World) PrivateExtents(index int) int {
	w.mu.Lock()
	p := w.hosts[index].pager
	w.mu.Unlock()
	if p == nil {
		return 0
	}
	stats, err := p.pagers.Ram.Stats(context.Background())
	if err != nil {
		return 0
	}
	return stats.PrivateExtents
}

// ReadPage reads one page of one volume through the named VM's own guest, which
// is the only place the bytes of an ephemeral disk are: its volume reads as
// zeroes whatever the guest wrote. It is an error for a VM running nowhere.
func (w *World) ReadPage(ctx context.Context, id, name string, page uint64) ([]byte, error) {
	_, g := w.runningVM(id)
	if g == nil {
		return nil, fmt.Errorf("%s is running nowhere", id)
	}
	if g.memoryRegions[name] == nil {
		return nil, fmt.Errorf("%s has no volume %s", id, name)
	}
	return g.read(ctx, name, page)
}

// Mappings is how many mappings one volume's memory region is to the VMM of the named
// VM's guest, zero where that VM is not running here. It is what a VMM process
// holds VMAs for, and what the placement rule is measured by.
func (w *World) Mappings(id, name string) int {
	_, g := w.runningVM(id)
	if g == nil {
		return 0
	}
	mp := g.mappings[name]
	if mp == nil {
		return 0
	}
	return mp.Runs()
}

func (w *World) Sealed(id string) (sealed, unchanged int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if in := w.instances[id]; in != nil {
		return in.sealed, in.unchanged
	}
	return 0, 0
}

// TakeWritable has the named VM's guest take every page of every volume
// writable and store nothing into any of them, which is what a guest that only
// reads looks like where every fault claims to be a write: KVM finishes a cold
// read from a worker that always asks for the page writable.
func (w *World) TakeWritable(ctx context.Context, id string) error {
	_, g := w.runningVM(id)
	if g == nil {
		return nil
	}
	for _, name := range g.names {
		for page := range uint64(g.pages[name]) {
			if err := g.takeWritable(ctx, name, page); err != nil {
				return err
			}
		}
	}
	return nil
}

// landed is a checkpoint that reported durable: it supersedes every earlier
// pause, so it is the only one this VM can come back at from here.
func (w *World) landed(in *instance, g *guest, at durableState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if in.guest != g {
		// The host running this guest was lost between the seal and the
		// publication landing. Whatever it published is a checkpoint of this VM
		// like any other, so it is offered rather than made the only one.
		in.durables = append(in.durables, at)
		return
	}
	in.durables = []durableState{at}
}

// offer records a pause this VM may have come back at, without taking away
// the ones before it: a publication whose outcome nobody knows.
func (w *World) offer(in *instance, at durableState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	in.durables = append(in.durables, at)
}

// KillDuring runs one operation of the deployment on a goroutine of its own,
// takes a host away in the middle of it at a moment the caller drew, and
// reports how that operation ended. It is what a campaign that kills a host
// mid-checkpoint, mid-fork or mid-handover is made of: the kill lands inside
// the operation on the seeds whose moment falls inside it and after it on the
// rest, and what has to hold afterwards does not move between them.
func (w *World) KillDuring(ctx context.Context, index int, mode sim.FailureMode,
	at time.Duration, operation func(context.Context) error) (bool, error) {
	done := make(chan error, 1)
	var finished atomic.Bool
	go func() {
		err := operation(ctx)
		finished.Store(true)
		done <- err
	}()
	if err := ctxsync.Sleep(ctx, at); err != nil {
		return false, err
	}
	// Whether this kill landed inside the operation or after it had finished is
	// what says a campaign is testing anything an orderly close does not: the
	// requirements are the same either way, but a campaign whose kills always
	// arrive late has reached none of the moments it exists for.
	cut := !finished.Load()
	if err := w.Kill(ctx, index, mode); err != nil {
		return cut, err
	}
	return cut, <-done
}

// Reads names what a verification does about a page it cannot read at all.
type Reads int

const (
	// ReadsMayFail tolerates a page whose only copy is on a peer a fault has
	// taken away: a read that does not complete is what the fault means, not a
	// byte read wrong. The bytes of every page that did read are still
	// required to be the guest's own.
	ReadsMayFail Reads = iota
	// ReadsMustSucceed requires every page to read. It is what the end of a
	// campaign requires, once every fault has ended: a VM whose memory is
	// still unreachable when nothing is wrong with the world is a VM that was
	// lost rather than one that was rewound.
	ReadsMustSucceed
)

// Verify requires every page of every running VM to read back the bytes its own
// guest wrote, through that guest's own mappings. It is the campaign's first
// invariant, and the only one a fault could break without leaving a trace in
// the store.
func (w *World) Verify(ctx context.Context, reads Reads) error {
	var errs []error
	for _, id := range w.Running() {
		_, g := w.runningVM(id)
		if g == nil {
			continue
		}
		errs = append(errs, w.check(ctx, g, g.snapshot(), reads))
	}
	return errors.Join(errs...)
}

// check verifies one guest against a model. A page that could not be read while
// a fault was on is reported and not returned; every byte that was read is
// required to be the guest's own either way.
func (w *World) check(ctx context.Context, g *guest, model map[string][]byte, reads Reads) error {
	err := g.verify(ctx, model)
	if err != nil && reads == ReadsMayFail && errors.Is(err, errUnreadable) {
		w.logf("a page could not be read while a fault was on: %v", err)
		return nil
	}
	return err
}

// VerifyDurable requires every page of one VM to read back through its volume —
// rather than through the guest's mappings — as exactly the checkpoint that VM
// came back at. It is the other half of the rewind a host loss costs: the pager
// reconstructing a page correctly and the volume holding it are two different
// claims, and a campaign that only ever read through the guest would be making
// one of them.
func (w *World) VerifyDurable(ctx context.Context, id string) error {
	in, g := w.runningVM(id)
	if in == nil {
		return nil
	}
	vm := w.vm(in)
	if vm == nil {
		return nil
	}
	w.mu.Lock()
	durables := slices.Clone(in.durables)
	w.mu.Unlock()
	if len(durables) == 0 {
		return nil
	}
	read := map[string][]byte{}
	for _, name := range g.names {
		data := make([]byte, g.pages[name]*g.pageBytes[name])
		if err := vm.Volume(name).Read(ctx, 0, data); err != nil {
			return fmt.Errorf("%s: reading %s through its volume: %w", id, name, err)
		}
		read[name] = data
	}
	for _, state := range durables {
		if agrees(read, nil, state.model) {
			return nil
		}
	}
	return fmt.Errorf("%s reads through its volume as state no checkpoint of it published: %s",
		id, describeRead(read, durables))
}

// Migrate moves one VM to another host: the source stops its guest and keeps
// its pages, the destination opens the VM, starts a guest from the captured
// state and pulls the pages no checkpoint holds out of the source.
//
// A migration under a fault may fail at any phase, and the phase decides who
// owns the VM afterwards. A source that never released it goes on running it. A
// source that released it to a destination that could not take it, or a
// destination that took it and could not fetch the pages the source held,
// leaves a VM nobody runs: somebody opens it, and what it comes back as is the
// checkpoint its control record selects. That rewind is the post-copy exposure
// and not a defect, which is why the model follows it exactly rather than
// tolerating it.
func (w *World) Migrate(ctx context.Context, id string, to int) error {
	return w.MigrateWith(ctx, id, to, Handover{})
}

// Handover is one migration's terms. The zero value is what a schedule's
// migration is: one attempt at each half, and a destination that could not take
// the VM leaves it to whoever opens it next.
type Handover struct {
	// Attempts is how many times the destination's half is tried before the VM
	// is given up. The source has already stopped its guest and given its
	// volumes up by then, so a retry is of the receive alone — the source keeps
	// the pages the destination has not pulled until it is told it has them
	// all, or until the handoff is given up. Zero is one attempt, which is what
	// a deployment's orchestrator makes; more is a campaign's own retrying, and
	// once they run out the source is told to give the handoff up and the VM is
	// opened again from its checkpoint, which loses what those pages held.
	Attempts int
	// Pause is how long the world waits between those attempts. It is
	// simulated time.
	Pause time.Duration
	// Inspect is one look at the handoff before the destination is given it,
	// which is where a scenario asks what a destination does with a layout it
	// must refuse.
	Inspect func(vmmigrate.Handoff) error
}

// MigrateWith is Migrate on the caller's terms.
func (w *World) MigrateWith(ctx context.Context, id string, to int, terms Handover) error {
	in, g := w.runningVM(id)
	if in == nil || in.host == to || w.up(to) == nil {
		return nil
	}
	source, destination := w.hosts[in.host], w.hosts[to]
	sourceHost := w.up(in.host)
	if sourceHost == nil {
		return nil
	}
	// The guest is held rather than read again: a kill may land in the middle
	// of this, and what the source was running is what the model is about
	// whether or not it is still running it.
	from := in.host
	at, writes := g.snapshot(), g.stored()
	vm := w.vm(in)
	handoff, err := sourceHost.Migrate(ctx, id, destination.pages)
	if err != nil {
		if errors.Is(err, vmmigrate.ErrStopped) {
			// The source gave its volumes up and cannot resume the guest: the
			// VM is nobody's until somebody opens it.
			w.place(in, from, nil)
			return w.recover(ctx, in, to, "a migration that could not be undone")
		}
		// Nothing was released, so the VM is still running where it was, and it
		// is still the VM it was: a pause that failed owes the guest every
		// memory region unsealed and every page writable.
		w.logf("%s: the migration to %s was refused: %v", id, destination.name, err)
		// A host that was taken away in the middle of this owes the VM nothing:
		// what a refused migration owes is what it owes a host that is still
		// there, which is the guest it stopped, running again.
		if w.live(id) == nil || in.guest != g {
			return nil
		}
		if vm != nil && vm.Status().HandedOff {
			return fmt.Errorf("%s: a refused migration gave the VM up anyway: %+v", id, vm.Status())
		}
		if err := g.store(g.names[0], 0); err != nil && !excused(err) {
			return fmt.Errorf("%s cannot store after a refused migration: %w", id, err)
		}
		return nil
	}
	// The VM runs on the destination from here, so the handle the source kept
	// may never write again.
	if vm != nil {
		if err := vm.Volume(g.names[0]).Write(ctx, 0, []byte{255}); !errors.Is(err, volume.ErrHandedOff) {
			return fmt.Errorf("%s: the source's handle accepted a store after the handoff: %v", id, err)
		}
	}
	if terms.Inspect != nil {
		if err := terms.Inspect(handoff); err != nil {
			return err
		}
	}
	received, err := w.receive(ctx, source, destination, handoff)
	for attempt := 1; err != nil && attempt < terms.Attempts; attempt++ {
		// The source still holds every page the destination did not pull, so
		// the handoff is still good: what failed was this attempt at it.
		w.logf("%s: attempt %d at %s: %v", id, attempt, destination.name, err)
		if sleepErr := ctxsync.Sleep(ctx, terms.Pause); sleepErr != nil {
			return sleepErr
		}
		received, err = w.receive(ctx, source, destination, handoff)
	}
	if err != nil {
		w.logf("%s: %s could not receive it: %v", id, destination.name, err)
		w.abandonSource(ctx, source, id)
		w.place(in, from, nil)
		return w.recover(ctx, in, to, "a receive that failed")
	}
	next := destination.guestFor(id)
	if next == nil {
		// The destination took the VM in and is gone: its process died between
		// the receive and this, so the guest it started went with it. The VM is
		// nobody's until somebody opens it.
		w.logf("%s: %s has no guest for it any more", id, destination.name)
		received.Close()
		w.abandonSource(ctx, source, id)
		w.place(in, from, nil)
		return w.recover(ctx, in, to, "a destination that did not survive the handover")
	}
	// The destination restored the VMM state the source's pause captured, so
	// the guest it is running is the one that stopped rather than a new one.
	if got := next.stored(); got != writes {
		return fmt.Errorf("%s: the destination restored %d stores, want the source's %d", id, got, writes)
	}
	// The source is released as soon as the destination reports the pages no
	// checkpoint holds, which is when a deployment releases it: every page left
	// is in object storage as well. The bulk stream behind the running guest
	// then meets a source that has given the VM up and reads the rest from
	// there, which is the ordinary end of a migration's stream rather than a
	// fault.
	if released := w.up(from); released != nil {
		if err := released.ReleaseMigrated(id); err != nil {
			w.logf("%s: %s would not release what it handed over: %v", id, source.name, err)
			_ = released.Abandon(id)
		}
	}
	arrived := w.streamed(ctx, received)
	received.Close()
	if arrived != nil {
		// The destination is running a guest whose memory is part its own and
		// part missing. There is nothing to publish and nothing to keep: it
		// gives the VM back, and the next host to open it starts from the
		// checkpoint the record still selects.
		w.logf("%s: the post-copy into %s did not finish: %v", id, destination.name, arrived)
		w.abandonSource(ctx, source, id)
		w.place(in, from, nil)
		return w.recover(ctx, in, to, "a post-copy that did not finish")
	}
	next.adopt(at)
	w.place(in, to, next)
	// Nothing was published at the handoff, so what the destination must read
	// back is the source's last checkpoint plus the pages it served. This is
	// checked through the destination's own mappings before it writes anything
	// of its own.
	if err := w.check(ctx, next, at, ReadsMayFail); err != nil {
		return fmt.Errorf("the destination's first read after a migration: %w", err)
	}
	return nil
}

// lose ends this incarnation of a host for everything waiting on it. It is
// called under the world's lock, and closing twice is a host taken away twice.
func (h *hostState) lose() {
	select {
	case <-h.gone:
	default:
		close(h.gone)
	}
}

// ErrLostSource reports the host that was holding a handed-over VM's pages
// having gone while the destination was still fetching them. Those pages exist
// nowhere else, so what the destination is waiting for is never coming; it has
// no way to know that, and the deployment has, which is why ending the handover
// is the deployment's to do.
var ErrLostSource = errors.New("simtest: the host holding the VM's pages is gone")

// receive runs a destination's half of a handoff, including the fault that
// fails its start before the guest exists.
//
// It ends when the host that handed the VM over is lost. A destination asks
// that host for the pages no checkpoint holds until they arrive — reading its
// own volume for one would rewind the guest past its own write, and a source
// that stumbled looks exactly like a source that died — so the deployment is
// what says the pages are gone, exactly as the orchestrator does for a real
// one. The deadline behind it is the harness's own patience and nothing the
// rule rests on.
func (w *World) receive(ctx context.Context, source, destination *hostState,
	handoff vmmigrate.Handoff) (*vmmigrate.Received, error) {
	destination.mu.Lock()
	delete(destination.started, handoff.VMID)
	destination.mu.Unlock()
	taking := w.up(w.indexOf(destination))
	if taking == nil {
		return nil, platform.ErrProcessStopped
	}
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	ctx, lost := context.WithCancelCause(ctx)
	defer lost(nil)
	w.mu.Lock()
	gone := source.gone
	w.mu.Unlock()
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		select {
		case <-gone:
			lost(fmt.Errorf("%w: %s was holding the pages of %s that no checkpoint has",
				ErrLostSource, source.name, handoff.VMID))
		case <-watching:
		case <-ctx.Done():
		}
	}()
	received, err := taking.Receive(ctx, handoff)
	if err != nil {
		// A destination that could not read the control record must not be left
		// running the guest: a VMM running over a VM this host has no authority
		// for is one whose stores nothing can ever publish. A receive that got
		// as far as starting one and then gave it up — a fork's child whose root
		// the store refused — has closed it, which is the same thing said the
		// other way.
		if errors.Is(err, platform.ErrUnavailable) && destination.stillRunning(handoff.VMID) {
			return nil, fmt.Errorf("%s left a guest running for %s without the control record",
				destination.name, handoff.VMID)
		}
		return nil, err
	}
	return received, nil
}

// guestFor is the guest this host started for a VM it took in.
func (h *hostState) guestFor(id string) *guest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started[id]
}

// stillRunning reports a guest this host started for one VM and has not given
// up. A guest a receive discarded is closed, so it is running no more.
func (h *hostState) stillRunning(id string) bool {
	g := h.guestFor(id)
	return g != nil && !g.isClosed()
}

// streamed waits for the rest of the source's resident set. The pages that
// exist nowhere else are already here — Receive does not return until they are
// — so what is left is a page the checkpoint holds too, which costs this host a
// read of object storage and nothing else. A bulk stream that ended early is
// therefore logged rather than returned.
//
// Every wait is bounded, because a fault that holds a page holds it until
// somebody gives up. Giving up is what a real destination does too.
func (w *World) streamed(ctx context.Context, received *vmmigrate.Received) error {
	stream, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	if err := received.Streamed(stream); err != nil {
		w.logf("the bulk stream ended early: %v", err)
	}
	return nil
}

// discardChild takes back the identity of a child its destination could not
// take in, which is what the orchestrator does for every child of a fan-out
// that did not happen: it is a name only that request ever knew, and a record
// left under it is an identity nothing can open and nothing can publish under.
// A delete the store refuses is retried by Settle, as an abandoned fork's is.
func (w *World) discardChild(ctx context.Context, destination *hostState, id string) {
	if running := w.up(w.indexOf(destination)); running != nil {
		if err := running.Delete(ctx, id); err == nil {
			return
		} else {
			w.logf("%s: deleting the child of a fork that did not happen: %v", id, err)
		}
	}
	if w.orphans == nil {
		w.orphans = map[string]bool{}
	}
	w.orphans[id] = true
}

// abandonSource gives a source's pages back once nothing can need them from
// there any more, and ends the guest that was holding them. The pages are going
// either way, so a release the source would refuse is a discard.
func (w *World) abandonSource(_ context.Context, source *hostState, id string) {
	running := w.up(w.indexOf(source))
	if running == nil {
		return
	}
	if err := running.Abandon(id); err != nil {
		w.logf("%s: abandoning what %s handed over: %v", id, source.name, err)
	}
}

// indexOf is a host's place in the deployment.
func (w *World) indexOf(h *hostState) int {
	return slices.Index(w.hosts, h)
}

// Fork starts the child of a running parent: the parent pauses, seals the pages
// no checkpoint of it holds and keeps running, and the child is created on the
// host the topology places it on, over the checkpoint the parent pinned.
//
// The fork is a handoff wherever the child lands, and the destination is what
// decides how the pages no checkpoint holds reach it: on another host it pulls
// them out of the parent's page server, and on the parent's own host it maps
// the pages the seal froze. The destination publishes the child's root as soon
// as it has them all, and until that lands the child is an identity nobody can
// open. A root that cannot be published under a fault is therefore not left
// behind: the child is closed, which deletes its record, and the topology's
// fork simply did not happen on this seed.
func (w *World) Fork(ctx context.Context, spec VMSpec) error {
	return w.FanOut(ctx, spec.Parent, []VMSpec{spec})
}

// FanOut forks one parent into every child of one fork point, which is what the
// deployment's own fork is: one pause of the parent, one handoff per child, and
// one hold on the point for each of them, onto one destination.
//
// A fan-out that fails part way is the whole of what makes it more than a fork
// repeated. The children after the failure are never offered to a destination,
// so nothing will ever fetch what their holds keep and nothing can release them:
// they are given up on the source, which is what takes the seal off the parent.
// The ones that did start are guests nobody asked for, under identities only the
// failed request ever knew, and they are deleted — the rollback the orchestrator
// runs, and the reason a fan-out that half happened leaves the parent exactly as
// a fork that never happened does.
func (w *World) FanOut(ctx context.Context, parent string, children []VMSpec) error {
	if len(children) == 0 {
		return nil
	}
	in, parentGuest := w.runningVM(parent)
	if in == nil {
		return nil
	}
	landing := children[0].Host
	ids := make([]string, 0, len(children))
	for _, spec := range children {
		if spec.Parent != parent || spec.Host != landing {
			return fmt.Errorf("%s: a fan-out is one fork point onto one host, and %s is neither",
				parent, spec.ID)
		}
		if w.Exists(spec.ID) {
			return nil
		}
		ids = append(ids, spec.ID)
	}
	if w.up(landing) == nil {
		return nil
	}
	source, destination := w.hosts[in.host], w.hosts[landing]
	sourceHost := w.up(in.host)
	if sourceHost == nil {
		return nil
	}
	// A child of this host's own is handed over to it without an address: its
	// pages never reach the wire.
	pages := destination.pages
	if landing == in.host {
		pages = ""
	}
	// A child starts from what the parent's fork point holds, which is its
	// memory with every ephemeral disk zeroed.
	at := parentGuest.checkpointed()
	handoffs, err := sourceHost.Fork(ctx, parent, ids, pages)
	if err != nil {
		w.logf("%s: the fork of %s was refused: %v", strings.Join(ids, ","), parent, err)
		return nil
	}
	if len(handoffs) != len(ids) {
		return fmt.Errorf("%s: a fork of %d children handed over %d", parent, len(ids), len(handoffs))
	}
	var taken []string
	for index, handoff := range handoffs {
		spec := children[index]
		if handoff.VMID != spec.ID {
			return fmt.Errorf("%s: the %s handoff names %s", parent, spec.ID, handoff.VMID)
		}
		started, err := w.forked(ctx, source, destination, spec, handoff, at)
		if err != nil {
			return err
		}
		if !started {
			// The set did not happen. Every child after this one is never
			// offered anywhere, so its hold is given up rather than released,
			// and the ones that did start are taken back.
			for _, rest := range children[index+1:] {
				w.abandonSource(ctx, source, rest.ID)
			}
			for _, id := range taken {
				if err := w.Delete(ctx, id); err != nil {
					w.logf("%s: deleting the child of a fan-out that did not happen: %v", id, err)
				}
			}
			return nil
		}
		taken = append(taken, spec.ID)
	}
	return nil
}

// forked takes one child of a fork point in on its destination and reports
// whether it started. A child that did not is one the destination gave up: its
// identity goes with it and the hold the point took for it is given up on the
// source, because nothing will ever fetch what that hold keeps.
func (w *World) forked(ctx context.Context, source, destination *hostState, spec VMSpec,
	handoff vmmigrate.Handoff, at map[string][]byte) (bool, error) {
	received, err := w.receive(ctx, source, destination, handoff)
	if err != nil {
		// The child could not get the pages only its parent had, or its root
		// would not publish: either way the destination gave the guest up. The
		// identity goes with it, and the parent takes its pages back here.
		w.logf("%s: %s could not receive the child: %v", spec.ID, destination.name, err)
		w.discardChild(ctx, destination, spec.ID)
		w.abandonSource(ctx, source, spec.ID)
		return false, nil
	}
	child := destination.guestFor(spec.ID)
	if child == nil {
		received.Close()
		w.abandonSource(ctx, source, spec.ID)
		return false, fmt.Errorf("%s: %s started no guest for the child", spec.ID, destination.name)
	}
	root := received.VM().Status().Checkpoint
	_ = w.streamed(ctx, received)
	received.Close()
	child.adopt(at)
	in := &instance{spec: spec}
	w.adopt(in)
	w.place(in, spec.Host, child)
	// The receive published the child's root, so the child is durable at the
	// point it inherited: that is the state it comes back at.
	w.notePublished(spec.ID, root.Sequence)
	w.landed(in, child, durableState{model: at, writes: child.stored(), sequence: root.Sequence})
	// The child starts at the point its parent was sealed at: the checkpoint
	// the parent published, plus the pages it has held since.
	if err := w.check(ctx, child, at, ReadsMayFail); err != nil {
		return false, fmt.Errorf("the child's first read: %w", err)
	}
	// The child has every page it inherited and a root of its own, so the parent
	// takes its sealed pages back here. A release the store refuses leaves the
	// point where it is, and the next step tries again: a parent that stays
	// sealed can never checkpoint again.
	if released := w.up(w.indexOf(source)); released != nil {
		if err := released.ReleaseMigrated(spec.ID); err != nil {
			w.logf("%s: the fork point could not be retired: %v", spec.Parent, err)
		}
	}
	return true, nil
}

// Settle finishes what a fault left half done: a VM no host is running because
// the takeover that should have picked it up was refused, and an identity a
// fork could not give back. It runs at every step of the schedule, which is
// what a host loop would do.
func (w *World) Settle(ctx context.Context) error {
	var errs []error
	// In identity order rather than the map's: what this world does must come
	// from the seed and not from where Go happened to put a key.
	for _, id := range slices.Sorted(maps.Keys(w.orphans)) {
		for index := range w.hosts {
			running := w.up(index)
			if running == nil {
				continue
			}
			if err := running.Volumes().Delete(ctx, id); err != nil {
				w.logf("%s: the identity of an abandoned fork is still not free: %v", id, err)
				break
			}
			delete(w.orphans, id)
			break
		}
	}
	for _, id := range w.Running() {
		if w.live(id) != nil {
			continue
		}
		w.mu.Lock()
		in := w.instances[id]
		stopped := in != nil && in.stopped
		w.mu.Unlock()
		if in == nil || stopped {
			// A VM the deployment stopped is running nowhere because it was
			// asked to be. Nothing here repairs that: only a start does.
			continue
		}
		for offset := range w.hosts {
			index := (in.host + offset) % len(w.hosts)
			if w.up(index) == nil {
				continue
			}
			errs = append(errs, w.recover(ctx, in, index, "the takeover before it was refused"))
			break
		}
	}
	return errors.Join(errs...)
}

// abandon takes a VM out of the world without publishing anything: the child of
// a fork that never published its root, whose close deletes the record it could
// never have used.
func (w *World) abandon(ctx context.Context, in *instance) {
	if running := w.up(in.host); running != nil {
		// Closing a fork that never published its root is what deletes the
		// record it could never have used. A close the store refused leaves an
		// identity nothing can open and nothing can publish under, so the delete
		// is repeated until it lands.
		if err := running.Delete(ctx, in.spec.ID); err != nil {
			w.logf("%s: closing an abandoned VM: %v", in.spec.ID, err)
			if w.orphans == nil {
				w.orphans = map[string]bool{}
			}
			w.orphans[in.spec.ID] = true
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	in.guest, in.present = nil, false
	delete(w.instances, in.spec.ID)
	w.order = slices.DeleteFunc(w.order, func(id string) bool { return id == in.spec.ID })
}

// Delete removes one VM: its guest stops, its handle closes and its record and
// objects go. What it leaves behind is the checkpoints a fork of it pinned,
// which no writer can reclaim.
func (w *World) Delete(ctx context.Context, id string) error {
	in := w.live(id)
	if in == nil {
		// A VM the deployment stopped is only its control record and its
		// objects, both of them in the store, so any host that is up can remove
		// them — there is no guest to close and no host to route to. Every other
		// VM nothing is running is one something is still doing: a host that was
		// lost and will be taken over, or a handover between two hosts.
		w.mu.Lock()
		stopped := w.instances[id]
		if stopped != nil && !stopped.stopped {
			stopped = nil
		}
		w.mu.Unlock()
		if stopped == nil {
			return nil
		}
		in = stopped
	}
	running := w.up(in.host)
	if running == nil {
		for index := range w.hosts {
			if running = w.up(index); running != nil {
				break
			}
		}
	}
	if running == nil {
		return nil
	}
	if err := running.Delete(ctx, id); err != nil {
		// The host gave the VM up either way — its guest is closed and its
		// handle released — and what a store outage cost is the record: the
		// identity is still there, so the delete is repeated at every step
		// until it lands.
		w.logf("%s: the delete was refused: %v", id, err)
		if w.orphans == nil {
			w.orphans = map[string]bool{}
		}
		w.orphans[id] = true
	}
	w.mu.Lock()
	in.guest, in.present = nil, false
	delete(w.instances, id)
	w.order = slices.DeleteFunc(w.order, func(other string) bool { return other == id })
	w.mu.Unlock()
	return nil
}

// Stop ends one VM deliberately and leaves the VM behind: the host publishes
// its guest's disks, closes the VMM process, gives the pages back and releases
// the handle. Nothing runs it afterwards and nothing starts it again on its own
// — that is the whole difference between a stop and every other way a VM stops
// running here, all of which are repairs waiting to happen. Its memory is not
// published, so a start boots it over its disks.
//
// The pause the stop publishes is the only one the VM can come back at, so it
// supersedes every earlier one exactly as a checkpoint that landed does. A stop
// the store refused is a stop that did not happen: the guest goes on running out
// of its own pages and the VM is worth what its last checkpoint was.
func (w *World) Stop(ctx context.Context, id string) error {
	return w.StopWith(ctx, id, hostapi.StopRequest{})
}

// Suspend is Stop with the guest's memory and VMM state published beside its
// disks, so a start resumes it where it was.
func (w *World) Suspend(ctx context.Context, id string) error {
	return w.StopWith(ctx, id, hostapi.StopRequest{Suspend: true})
}

// StopWith is Stop as a request asks: suspending the guest, keeping the
// checkpoint the stop publishes, or both.
func (w *World) StopWith(ctx context.Context, id string, request hostapi.StopRequest) error {
	in, g := w.runningVM(id)
	if in == nil {
		return nil
	}
	running := w.up(in.host)
	if running == nil {
		return nil
	}
	// The model at the pause is what this publishes. Nothing stores into this
	// guest while the stop runs — the driver is the only thing that stores at
	// all — so the snapshot taken here is exactly what the seal froze.
	at := durableState{model: g.checkpointed(), writes: g.stored(), stateless: !request.Suspend}
	stopped, err := running.Stop(ctx, id, request)
	if err != nil {
		w.logf("%s: the stop was refused: %v", id, err)
		return nil
	}
	at.sequence = stopped.Sequence
	if request.Keep {
		w.noteKept(id, at)
	}
	// The sequence is a writer's whatever the publication did with it: a VM that
	// comes back at it came back at state this writer sealed.
	w.notePublished(id, at.sequence)
	w.landed(in, g, at)
	h := w.hosts[in.host]
	h.mu.Lock()
	h.guests = slices.DeleteFunc(h.guests, func(other *guest) bool { return other == g })
	delete(h.started, id)
	h.mu.Unlock()
	w.mu.Lock()
	in.guest, in.present, in.stopped = nil, false, true
	w.mu.Unlock()
	w.logf("%s: stopped at %s, suspended=%t, kept=%t", id, stopped, request.Suspend, request.Keep)
	return nil
}

// Start opens a stopped VM on host index again and requires it to come back at
// the checkpoint its stop published: the control record has to select a sequence
// one of its own writers established, and every page has to read back as the
// bytes that checkpoint holds. It is the same open a takeover runs, which is
// what makes the assertion the same one — a start that returned other bytes
// would be a VM rewound by a stop that was supposed to lose nothing.
//
// A VM that is running already is left alone, and so is one that stopped running
// for any other reason: a start is the answer to a stop and to nothing else.
func (w *World) Start(ctx context.Context, id string, index int) error {
	w.mu.Lock()
	in := w.instances[id]
	stopped := in != nil && in.stopped
	w.mu.Unlock()
	if in == nil || !stopped {
		return nil
	}
	if w.up(index) == nil {
		return nil
	}
	if _, err := w.reopen(ctx, in, index, "a start"); err != nil {
		return err
	}
	w.mu.Lock()
	// A start whose open was refused leaves the VM where it was: stopped, and
	// startable again at a later step.
	if in.present {
		in.stopped = false
	}
	w.mu.Unlock()
	return nil
}

// StartCold opens a stopped VM on host index without its memory, which is what
// an operator asks for to reboot a guest: the host discards every page of the
// memory volume and the VMM state with it in one checkpoint, and the guest that
// starts over it boots rather than being restored.
//
// What it must come back as is what nothing else in this world produces: zeroes
// where its memory was, and everywhere else exactly the bytes its last
// checkpoint published. A cold start that left a byte of the old memory behind,
// or that took a byte of the disk with it, is a defect this is here to find.
//
// Like a start, it is the answer to a stop and to nothing else: a VM that is
// running, or one that stopped running for any other reason, is left alone.
func (w *World) StartCold(ctx context.Context, id string, index int) error {
	w.mu.Lock()
	in := w.instances[id]
	stopped := in != nil && in.stopped
	w.mu.Unlock()
	if in == nil || !stopped {
		return nil
	}
	if w.up(index) == nil {
		return nil
	}
	if _, err := w.reopenWith(ctx, in, index, "a cold start", true); err != nil {
		return err
	}
	w.mu.Lock()
	if in.present {
		in.stopped = false
	}
	w.mu.Unlock()
	return nil
}

// Shutdown ends one host the orderly way: it closes, publishing a final
// checkpoint of everything its handles still hold, its guests give their pages
// back, and only then does its process end, leaving its disk exactly as it is.
// It is what a drained host does, and the thing a kill is defined against.
func (w *World) Shutdown(ctx context.Context, index int) error {
	h := w.hosts[index]
	running := w.up(index)
	if running == nil {
		return nil
	}
	var errs []error
	errs = append(errs, running.Close(ctx))
	h.mu.Lock()
	guests := slices.Clone(h.guests)
	h.guests, h.started = nil, map[string]*guest{}
	h.mu.Unlock()
	for _, g := range guests {
		errs = append(errs, g.detach(ctx))
	}
	w.mu.Lock()
	h.down, h.host = true, nil
	h.lose()
	for _, id := range w.order {
		if in := w.instances[id]; in != nil && in.host == index {
			in.guest, in.present = nil, false
		}
	}
	w.mu.Unlock()
	if err := h.process.Stop(ctx); err != nil && !errors.Is(err, platform.ErrProcessStopped) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Takeover opens a VM another host is still running on host index. That is what
// a deployment does with a host it cannot reach: the open advances the writer
// epoch, which fences the handle that held the VM, and what the new writer
// inherits is the checkpoint the record selects. The old host goes on running
// its guest and does not know — it learns at its next checkpoint, or never —
// which is exactly the pair this campaign exists to keep apart.
func (w *World) Takeover(ctx context.Context, id string, index int) error {
	w.mu.Lock()
	in := w.instances[id]
	if in != nil {
		in.present = false
	}
	w.mu.Unlock()
	if in == nil {
		return nil
	}
	return w.recover(ctx, in, index, "a takeover")
}

// LoseHost takes one host away at a moment: its guests stop existing, the
// pages they held are gone, its page server stops answering and its handles
// publish nothing ever again. Everything it was running is now only what its
// last checkpoint published.
func (w *World) LoseHost(ctx context.Context, index int) error {
	return w.Kill(ctx, index, sim.CrashProcess)
}

// Kill takes one host away in the middle of whatever it was doing, in the mode
// the caller names. The store goes first, so nothing it had in flight can still
// land and its shutdown publishes nothing; its guests go next, because a
// machine whose host died is a VMM process whose pages are gone; then the
// process is crashed — every goroutine cancelled, every listener closed, every
// page released — and under PowerLoss the modifications its disk had not
// synced come back applied, dropped, torn or garbled rather than restored.
func (w *World) Kill(ctx context.Context, index int, mode sim.FailureMode) error {
	h := w.hosts[index]
	if w.up(index) == nil {
		return nil
	}
	h.dead.Store(true)
	w.mu.Lock()
	h.down, h.crashed = true, true
	h.lose()
	for _, id := range w.order {
		in := w.instances[id]
		if in == nil || in.host != index {
			continue
		}
		in.guest, in.present = nil, false
	}
	w.mu.Unlock()
	// The VMM processes go before the machine does: a page is durable nowhere,
	// so a guest whose host died is a process whose memory is gone. They are
	// this host's own list rather than the world's, which is what lets a kill
	// land in the middle of an operation without the two of them sharing a map.
	h.mu.Lock()
	running := slices.Clone(h.guests)
	h.guests, h.started = nil, map[string]*guest{}
	h.mu.Unlock()
	for _, g := range running {
		_ = g.Close()
	}
	if err := h.process.Crash(ctx, mode); err != nil {
		return err
	}
	w.mu.Lock()
	h.host = nil
	w.mu.Unlock()
	return nil
}

// RestartHost starts a lost host again, in this process, on the disk its own
// kill left behind, and gives it back everything it was running: each of those
// VMs is opened on it, which advances the writer epoch, and its guest starts
// from the checkpoint the record selects. That is the rewind a host loss costs,
// and it is what the model says it must be.
func (w *World) RestartHost(ctx context.Context, index int) error {
	if err := w.Restart(ctx, index); err != nil {
		return err
	}
	var errs []error
	for _, id := range w.Running() {
		if w.live(id) != nil {
			continue
		}
		w.mu.Lock()
		in := w.instances[id]
		// A VM the deployment stopped is not one this host was running when it
		// died: its handle was released and its pages given back before that,
		// so a host coming back has nothing of it to give back. Only a start
		// brings it back, exactly as it is the only thing a settle leaves alone.
		mine := in != nil && in.host == index && !in.stopped
		w.mu.Unlock()
		if !mine {
			continue
		}
		errs = append(errs, w.recover(ctx, in, index, "the host it ran on was lost"))
	}
	return errors.Join(errs...)
}

// Restart starts a lost host again and gives it back nothing. A host keeps no
// durable local state, so what it comes back with is the deployment's object
// store and the scratch disk its own kill left behind.
func (w *World) Restart(ctx context.Context, index int) error {
	h := w.hosts[index]
	if !h.down {
		return nil
	}
	h.dead.Store(false)
	if err := w.launch(h); err != nil {
		return err
	}
	h.down = false
	return nil
}

// Incarnation is how many times one host has been started, which is what says a
// kill was followed by a restart rather than by the same process carrying on.
func (w *World) Incarnation(index int) int { return w.hosts[index].incarnation }

// recover is reopen for a VM nobody meant to stop running: whatever was
// running it went away, so opening it again somewhere else is a takeover and is
// counted as one. A start is the same open and is not one — the count is what a
// campaign asserts on to say its fault reached the takeover it is about, and a
// VM the deployment stopped and started on purpose would satisfy that assertion
// without anything having gone wrong at all.
func (w *World) recover(ctx context.Context, in *instance, index int, why string) error {
	opened, err := w.reopen(ctx, in, index, why)
	if opened {
		w.mu.Lock()
		w.takeovers++
		w.mu.Unlock()
	}
	return err
}

// reopen opens a VM nobody is running on host index and starts a guest over
// it, reporting whether it came back there. Whatever happened to the host that
// held it, what it comes back as is the checkpoint its control record selects:
// the model rewinds to the bytes that checkpoint published, and every later
// read must be those bytes.
func (w *World) reopen(ctx context.Context, in *instance, index int, why string) (bool, error) {
	return w.reopenWith(ctx, in, index, why, false)
}

// reopenWith is reopen, warm or cold. A cold open discards the VM's memory and
// its VMM state in a checkpoint of its own before the guest starts, so what the
// VM comes back as is not the pause it went away at: it is that pause with
// its memory replaced by zeroes, under a checkpoint this writer published.
func (w *World) reopenWith(ctx context.Context, in *instance, index int, why string, cold bool) (bool, error) {
	h := w.hosts[index]
	running := w.up(index)
	if running == nil {
		return false, nil
	}
	vm, err := w.openFor(ctx, running, in.spec.ID, cold)
	if err != nil {
		// A VM that cannot be opened is not lost: its record and its checkpoint
		// are where they were, and a later step opens it. It is running
		// nowhere until then.
		w.logf("%s: %s could not take it over after %s: %v", in.spec.ID, h.name, why, err)
		in.host, in.present = index, false
		return false, nil
	}
	// The checkpoint the new writer inherits has to be one this VM published.
	// Anything else is state nobody wrote. A cold start publishes its own as it
	// opens — that checkpoint is what discarded the memory — so the sequence it
	// comes back at is this writer's rather than one that already existed, and
	// the pause it stands for is the one the world works out below.
	selected := vm.Status().Checkpoint
	var stopped durableState
	if cold {
		w.notePublished(in.spec.ID, selected.Sequence)
		stopped = w.latest(in)
		w.coldState(in, stopped, selected.Sequence)
	}
	if !w.published[in.spec.ID][selected.Sequence] {
		return false, fmt.Errorf("%s came back at checkpoint %s, which no writer of it published",
			in.spec.ID, selected)
	}
	// What this VM reads back has to be exactly one of the checkpoints it may
	// have come back at: never two of them mixed, and never a byte no guest
	// wrote. It is found by the sequence the record selects before the guest
	// starts, because a start over a checkpoint with no VMM state publishes one
	// of its own.
	came, ok := w.at(in, selected.Sequence)
	if !ok {
		return false, fmt.Errorf("%s came back at %s, which is not one of the checkpoints it may have come back at",
			in.spec.ID, selected)
	}
	// The VMM state that checkpoint carries is what this guest is restored
	// from. One that carries none is cold booted, which is the host's own
	// decision: it discards the memory in a checkpoint of its own, and the
	// guest boots over the disks, which here is a process that has written
	// nothing. A cold start has already discarded it.
	var state []byte
	if cold {
		state, err = host.State(ctx, running.Checkpoints(), selected)
		if errors.Is(err, checkpoint.ErrNoState) {
			state, err = nil, nil
		}
	} else {
		state, err = running.Starting(ctx, vm, MemoryVolume)
	}
	if err != nil {
		w.logf("%s: %s could not read the VMM state of %s: %v", in.spec.ID, h.name, selected, err)
		if err := vm.Close(ctx); err != nil {
			w.logf("%s: releasing a takeover that could not restore its guest: %v", in.spec.ID, err)
		}
		in.host, in.present = index, false
		return false, nil
	}
	if came.stateless != (state == nil) {
		return false, fmt.Errorf("%s came back at %s with %d bytes of VMM state, and that checkpoint was published stateless=%t",
			in.spec.ID, selected, len(state), came.stateless)
	}
	if !cold && came.stateless {
		// The host cold booted it: what it comes back as is the checkpoint it
		// opened with its memory replaced by zeroes, under the sequence the
		// discard published, and the writes that checkpoint rewound are the
		// ones past its own pause.
		booted := vm.Status().Checkpoint.Sequence
		w.notePublished(in.spec.ID, booted)
		stopped = came
		came = w.coldState(in, came, booted)
		cold = true
	}
	g, err := w.newGuest(h, h.pager, vm, nil, state)
	if err != nil {
		return false, err
	}
	h.running(g)
	read, missing, unreadable := g.readAll(ctx)
	if !agrees(read, missing, came.model) {
		return false, fmt.Errorf("%s came back at %s reading state that checkpoint did not publish: %s",
			in.spec.ID, selected, describeRead(read, []durableState{came}))
	}
	if got := g.stored(); got != came.writes {
		return false, fmt.Errorf("%s came back having made %d stores, and the checkpoint it came back at held %d",
			in.spec.ID, got, came.writes)
	}
	g.adopt(came.model)
	w.mu.Lock()
	in.durables = []durableState{came}
	// What this recovery cost the guest in time is the other half of what it
	// cost it: the writes past this checkpoint are gone, and the loss window is
	// what says how many of them there may be.
	if cold {
		// A booted guest counts its stores from none, so the writes that date
		// them start over with it.
		w.rewind(in, stopped)
		in.writes = nil
	} else {
		w.rewind(in, came)
	}
	windowErr := w.spanHolds(in)
	w.mu.Unlock()
	if windowErr != nil {
		return false, windowErr
	}
	w.place(in, index, g)
	if err := running.AddMachine(in.spec.ID, g); err != nil {
		return true, err
	}
	w.logf("%s: %s opened it at %s after %s", in.spec.ID, h.name, selected, why)
	if unreadable != nil {
		w.logf("a page could not be read while a fault was on: %v", unreadable)
	}
	return true, nil
}

// openFor opens one VM on a host, warm or cold. The cold open is the host's own
// operation: it publishes the checkpoint that discards the memory and the VMM
// state, and the handle it returns is at that checkpoint.
func (w *World) openFor(ctx context.Context, running *host.Host, id string, cold bool) (*volume.VM, error) {
	if cold {
		return running.OpenCold(ctx, id, host.ColdShape{Memory: MemoryVolume})
	}
	return running.Volumes().Open(ctx, id)
}

// coldState is the pause a cold boot brings a VM back at: the pause from, with
// its memory replaced by zeroes, under the sequence the boot's discard
// published. The guest that comes back has written nothing — it booted rather
// than being restored — so the stores that pause is worth are none.
//
// It supersedes every pause before it exactly as a checkpoint that landed
// does: the memory those pauses held is gone from the store.
func (w *World) coldState(in *instance, from durableState, sequence uint64) durableState {
	w.mu.Lock()
	defer w.mu.Unlock()
	model := make(map[string][]byte, len(from.model))
	for name, data := range from.model {
		if name == MemoryVolume {
			model[name] = make([]byte, len(data))
			continue
		}
		model[name] = bytes.Clone(data)
	}
	booted := durableState{sequence: sequence, model: model, stateless: true}
	in.durables = []durableState{booted}
	return booted
}

// latest is the last pause this VM may have come back at, which is the one a
// stop published.
func (w *World) latest(in *instance) durableState {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(in.durables) == 0 {
		return durableState{}
	}
	return in.durables[len(in.durables)-1]
}

// at is the checkpoint a VM came back at, found by the sequence its control
// record selects rather than by the bytes it reads: the record is what decides
// which pause this VM is at, so the bytes are then required to be that pause's
// rather than searched for among the pauses it might be.
func (w *World) at(in *instance, sequence uint64) (durableState, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, state := range in.durables {
		if state.sequence == sequence {
			return state, true
		}
	}
	return durableState{}, false
}

// agrees reports whether every byte that was read is the byte that checkpoint
// published.
func agrees(read map[string][]byte, missing map[string][]bool, model map[string][]byte) bool {
	for name, got := range read {
		want, found := model[name]
		if !found || len(got) != len(want) {
			return false
		}
		size := int(PageSizeOf(name))
		for page := 0; page*size < len(got); page++ {
			if page < len(missing[name]) && missing[name][page] {
				continue
			}
			at := page * size
			if !bytes.Equal(got[at:at+size], want[at:at+size]) {
				return false
			}
		}
	}
	return true
}

// describeRead is what a failure prints to say what the VM read instead: the
// first byte of every page it read, beside the first byte of every page each
// checkpoint it may have come back at published.
func describeRead(read map[string][]byte, durables []durableState) string {
	line := "read " + heads(read)
	for _, state := range durables {
		line += fmt.Sprintf("; checkpoint at %d stores holds %s", state.writes, heads(state.model))
	}
	return line
}

func heads(model map[string][]byte) string {
	line := ""
	for _, name := range slices.Sorted(maps.Keys(model)) {
		line += " " + name + "="
		size := int(PageSizeOf(name))
		for page := 0; page*size < len(model[name]); page++ {
			line += fmt.Sprintf("%d,", model[name][page*size])
		}
	}
	return line
}

// Close gives every VM a host is running back to the world in a state the
// deployment check can be run against: every guest detached, every host closed,
// every publication finished. It is the end of a campaign and nothing else.
func (w *World) Close(ctx context.Context) error {
	if w.closed {
		return nil
	}
	w.closed = true
	// A checkpoint's sweep runs behind its publication, and closing a VM
	// finishes it rather than cancelling it, so a world closed the moment
	// after a checkpoint landed leaves nothing behind that a running host
	// would have deleted.
	var errs []error
	for index, h := range w.hosts {
		running := w.up(index)
		if running == nil {
			continue
		}
		if err := running.Close(ctx); err != nil {
			w.logf("%s: closing at the end: %v", h.name, err)
		}
	}
	// Every VMM process of every host, not only the one each VM is running in:
	// a takeover leaves the superseded host running a guest of its own, and a
	// pager with a memory region still attached is one that cannot close.
	for _, h := range w.hosts {
		h.mu.Lock()
		running := slices.Clone(h.guests)
		h.mu.Unlock()
		for _, g := range running {
			errs = append(errs, g.detach(ctx))
		}
	}
	for _, id := range w.Running() {
		in, _ := w.runningVM(id)
		if in == nil {
			continue
		}
		w.place(in, in.host, nil)
	}
	for _, h := range w.hosts {
		if h.down {
			continue
		}
		if err := h.process.Stop(ctx); err != nil && !errors.Is(err, platform.ErrProcessStopped) {
			errs = append(errs, err)
		}
		h.down = true
	}
	return errors.Join(errs...)
}

// CheckSelected requires every VM's control record to select a checkpoint some
// writer of that VM published. It is the campaign's second invariant, and the
// one a takeover under a partition is most likely to break: a VM whose record
// selects a sequence nobody ever published is a VM whose state was invented.
func (w *World) CheckSelected(ctx context.Context) error {
	records, err := w.records()
	if err != nil {
		return err
	}
	var errs []error
	for _, spec := range w.config.Topology.VMs {
		record, err := records.Read(ctx, spec.ID)
		if errors.Is(err, platform.ErrNotFound) {
			// A VM the campaign deleted, or a fork that never published its
			// root. Neither has state for anything to disagree about.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: reading the record: %w", spec.ID, err))
			continue
		}
		if !w.published[spec.ID][record.Selected] {
			errs = append(errs, fmt.Errorf("%s selects checkpoint %d, which no writer of it published: %v",
				spec.ID, record.Selected, slices.Sorted(sequences(w.published[spec.ID]))))
		}
	}
	return errors.Join(errs...)
}

// records reads the deployment's control records directly, outside any host, so
// that what the end of a campaign asks the store is not filtered through a view
// a fault took away.
func (w *World) records() (*control.Client, error) {
	return control.NewClient(control.Config{ObjectStore: w.runtime.ObjectStore(),
		ObjectPrefix: w.config.Prefix})
}

// sequences yields the keys of a set, which is what a failure prints to say what was
// published instead.
func sequences(set map[uint64]bool) func(func(uint64) bool) {
	return func(yield func(uint64) bool) {
		for key := range set {
			if !yield(key) {
				return
			}
		}
	}
}

// Deadline is how long an operation of the schedule waits before it gives up
// and calls the fault it is running under the answer. It is simulated time: a
// blocked link costs a campaign microseconds of wall time.
const Deadline = 30 * time.Second
