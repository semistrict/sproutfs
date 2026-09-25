//go:build linux && (amd64 || arm64)

// Package vmmachine supervises Firecracker processes using host-managed volumes.
package vmmachine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// Pmem is one persistent-memory device of the VM. ID is both the Firecracker
// device identity and the name of the volume that backs it.
type Pmem struct {
	ID   string
	Root bool
}

// RAMVolume is the name of the volume backing the VM's RAM. A machine has one
// RAM memory region, so the name is fixed.
const RAMVolume = "ram0"

// Config supervises one VM over the volumes of one *volume.VM. RAM binds to the
// volume named "ram0" and each PMEM device binds to the volume named by its
// device id. Source data must already have been ingested before Start. The
// shared pagers outlive all their Process instances. SeccompFilter must include
// the VMM's explicit memory-worker policy. Cold boot uses
// KernelPath/InitrdPath/BootArgs; restore replays the VMM state bytes the caller
// read from the checkpoint.
type Config struct {
	Binary, SeccompFilter, KernelPath, InitrdPath, BootArgs string
	Scratch                                                 *Scratch
	// Pagers is the host's pager per kind of memory region: RAM attaches to one and
	// every PMEM device to the other, each with its own arena and its own page.
	// A machine takes both, because one VM maps both kinds.
	Pagers vmmemory.Pagers
	// VM owns every volume this machine maps. The pager is its only mutator
	// while the machine runs.
	VM           *volume.VM
	Pmem         []Pmem
	VCPUs        int
	RestoreState []byte
	// VsockCID is the guest context id of a virtio-vsock device, and zero is a
	// machine without one. The device's host end is a Unix socket this process
	// owns, under its own scratch directory, which VsockPath names: the host
	// reaches a guest by connecting to it and asking for a guest port.
	//
	// A restore carries the device in its VMM state and this configuration only
	// moves its socket, so a fork and a migrated VM are reachable at their own
	// new path without the guest noticing anything. The CID is the guest's own
	// name for itself and never leaves it, so every VM may use the same one.
	VsockCID   uint32
	Connection vmmemory.ConnectionConfig
	// Backings replaces, by volume name, the backing a memory region attaches with. The
	// volume stays the memory region's identity — its name, its size, its writer,
	// everything a seal and a checkpoint are made of — and only what the
	// pager loads through changes. That is what a migration's destination needs:
	// its memory regions read from the host that still holds the pages, and read their
	// own log for everything that host does not have.
	//
	// Every name must be a memory region this machine maps, and the backing must be
	// the size of the volume it stands in front of. A name this machine binds no
	// memory region for is refused rather than ignored: ignoring it would start a
	// destination that faults from its own volumes and never asks the host
	// holding its pages at all.
	Backings map[string]vmmemory.Backing
}

// memory region is one memory region a machine maps: the volume it takes its identity
// from, and the backing it attaches with, which is that volume unless the
// configuration overrode it.
type memoryRegion struct {
	name    string
	volume  *volume.Volume
	backing vmmemory.MemoryRegionBacking
	// root marks the PMEM device the guest boots from, which is what the VMM's
	// configuration file calls root_device. It is never set on RAM.
	root bool
}

// plan is the layout one configuration maps: the one RAM memory region, one memory region per
// PMEM device in configuration order, and the RAM the machine boots with.
// Computing it validates the configuration, so nothing is created before an
// invalid one is refused.
type plan struct {
	ram      memoryRegion
	pmem     []memoryRegion
	ramBytes uint64
}

func (c Config) plan() (plan, error) {
	if c.Pagers.Ram == nil || c.Pagers.Pmem == nil || c.Binary == "" || c.SeccompFilter == "" || c.VM == nil ||
		len(c.Pmem) > 63 || c.VCPUs < 1 || c.VCPUs > 32 || len(c.RestoreState) > MaxStateBytes {
		return plan{}, errors.New("vmmachine: invalid configuration")
	}
	if len(c.RestoreState) == 0 && c.KernelPath == "" {
		return plan{}, errors.New("vmmachine: cold boot needs a kernel")
	}
	// Zero is no vsock at all, and the three lowest context ids are the
	// hypervisor's, the loopback's and the host's.
	if c.VsockCID != 0 && c.VsockCID < 3 {
		return plan{}, fmt.Errorf("vmmachine: vsock CID %d is reserved", c.VsockCID)
	}
	var result plan
	// mapped is every memory region name this machine binds, which is what a backing
	// override must name and what keeps a PMEM device from colliding with RAM.
	mapped := map[string]bool{}
	ram := c.VM.Volume(RAMVolume)
	if ram == nil {
		return plan{}, fmt.Errorf("vmmachine: %s has no volume named %s", c.VM.ID(), RAMVolume)
	}
	ramPage := c.Pagers.Ram.PageSize()
	if ram.Size() == 0 || ram.Size()%ramPage != 0 || ram.Size() > 1<<40 {
		return plan{}, fmt.Errorf("vmmachine: RAM must be whole %d-byte pages of at most 1 TiB", ramPage)
	}
	ramBacking, err := c.backingOf(RAMVolume, ram)
	if err != nil {
		return plan{}, err
	}
	result.ram = memoryRegion{name: RAMVolume, volume: ram,
		backing: vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: ramBacking}}
	result.ramBytes = ram.Size()
	mapped[RAMVolume] = true
	roots := 0
	for _, d := range c.Pmem {
		v := c.VM.Volume(d.ID)
		// Firecracker requires 2 MiB PMEM alignment whatever the pager's page
		// is, so a device is checked against both.
		if d.ID == "" || len(d.ID) > 64 || mapped[d.ID] || v == nil || v.Size()%(2<<20) != 0 ||
			v.Size()%c.Pagers.Pmem.PageSize() != 0 {
			return plan{}, fmt.Errorf("vmmachine: invalid PMEM device %q", d.ID)
		}
		mapped[d.ID] = true
		backing, err := c.backingOf(d.ID, v)
		if err != nil {
			return plan{}, err
		}
		result.pmem = append(result.pmem, memoryRegion{name: d.ID, volume: v, root: d.Root,
			backing: vmmemory.MemoryRegionBacking{Kind: vmmemory.Pmem, Backing: backing}})
		if d.Root {
			roots++
		}
	}
	if roots > 1 {
		return plan{}, errors.New("vmmachine: multiple PMEM roots")
	}
	// A backing that names nothing this machine maps is a destination that would
	// fault from its own volumes without ever asking the host holding its pages,
	// so it is a configuration error rather than an unused entry.
	for name := range c.Backings {
		if !mapped[name] {
			return plan{}, fmt.Errorf("vmmachine: %s maps no memory region named %q", c.VM.ID(), name)
		}
	}
	return result, nil
}

// backingOf is what one memory region attaches with: its volume, or the backing the
// configuration put in front of that volume.
func (c Config) backingOf(name string, v *volume.Volume) (vmmemory.Backing, error) {
	override, overridden := c.Backings[name]
	if !overridden {
		return v, nil
	}
	if override == nil {
		return nil, fmt.Errorf("vmmachine: the backing of %q is nil", name)
	}
	if override.Size() != v.Size() {
		return nil, fmt.Errorf("vmmachine: the backing of %q is %d bytes, its volume is %d",
			name, override.Size(), v.Size())
	}
	return override, nil
}

type endpoint struct {
	listener *net.UnixListener
	path     string
	backing  vmmemory.MemoryRegionBacking
	// name is the volume the backing stands in front of, which is how a
	// migration addresses this machine's memory regions.
	name       string
	connection *vmmemory.Connection
}

// Process retains every pager attachment until waitpid proves all VMM memory
// users have stopped. Control loss kills the entire process, including vCPUs.
type Process struct {
	mu *ctxsync.Mutex
	// cancel ends the process-lifetime context every pager attachment runs
	// under, so a startup that is abandoned or a VMM that exits wakes them.
	cancel context.CancelCauseFunc
	dir    string
	// api is the VMM's API socket, which the VMM binds and this process's
	// client dials.
	api string
	// vsock is the host end of this machine's virtio-vsock device, empty for a
	// machine configured without one.
	vsock            string
	cmd              *exec.Cmd
	client           *http.Client
	endpoints        []*endpoint
	connectionsReady chan struct{}
	done             chan struct{}
	exitErr          error
	stdin            *os.File
	log              *consoleRing
	files            *stateFiles
	scratch          *Scratch
	closeOnce        sync.Once
	closeErr         error
	running          bool
	failure          atomic.Pointer[processFailure]
	// id is the VM this machine runs, which is what its diagnostics name it by.
	id string
	// closing is set by the owner that is stopping this process on purpose, so
	// the exit it then sees is not reported as a death.
	closing atomic.Bool
	// phases is what Start spent, filled as it goes and read once it returns.
	phases StartPhases
}

// StartPhases splits what starting one machine cost, so a restore that took
// seconds says which part of it did. They are the phases of Start in order, and
// they do not sum to it: a session is built by the VMM inside its own snapshot
// load, so AttachNS overlaps StateLoadNS.
//
//   - ProcessNS is the VMM process itself: the arguments and state files, the
//     exec, and the wait for the API socket it binds once it is past its own
//     seccomp filter. Nothing of this pager's is in it.
//   - StateLoadNS is the snapshot load request, which is where a restore's
//     memory sessions are built: the VMM asks for each memory region's descriptor
//     inside it, so the pager's attach and populate happen here.
//   - SessionsNS is what was left of building those sessions once the load
//     returned, which for a machine whose populate outlives the request is
//     where that shows.
//   - ReadyNS is the round trip that proves the machine is up.
//   - Attachments is what each memory region's session cost, by the volume it maps.
type StartPhases struct {
	ProcessNS   int64
	StateLoadNS int64
	SessionsNS  int64
	ReadyNS     int64
	Attachments map[string]vmmemory.AttachStats
}

// StartPhases reports how this machine's start divided up.
func (p *Process) StartPhases() StartPhases { return p.phases }

type processFailure struct{ err error }

func (p *Process) stop(err error) {
	p.cancel(err)
	select {
	case <-p.done:
		return
	default:
	}
	p.failure.CompareAndSwap(nil, &processFailure{err})
	_ = p.cmd.Process.Kill()
}

func (p *Process) result() error {
	if failure := p.failure.Load(); failure != nil {
		return errors.Join(p.exitErr, failure.err)
	}
	return p.exitErr
}

// Start builds one VMM process out of its configuration, in the order the
// parts depend on each other: the scratch directory and the state files that
// live in it, the pager endpoints the VMM will attach to, the arguments and
// boot configuration that name them, the process itself, and then the two
// things that have to be waited for — the API socket and the memory sessions.
// Every phase is below, one function each; this is only their order and what
// they hand each other.
func Start(ctx context.Context, c Config) (*Process, error) {
	layout, err := c.startable()
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	p := &Process{mu: ctxsync.NewMutex(), cancel: cancel, done: make(chan struct{}), connectionsReady: make(chan struct{}), running: len(c.RestoreState) == 0, id: c.VM.ID()}
	// The cleanup covers openScratch too: it takes this process's directory
	// from the shared scratch before anything below it can fail, and a process
	// left registered there is one the scratch counts as live for ever, so its
	// owner can never close and the host can never exit.
	started := false
	defer func() {
		if !started {
			_ = p.Close()
		}
	}()
	if err := p.openScratch(ctx, c); err != nil {
		cancel(err)
		return nil, err
	}
	pmem, overrides, err := p.listen(layout)
	if err != nil {
		return nil, err
	}
	args, err := p.arguments(ctx, c, layout, pmem)
	if err != nil {
		return nil, err
	}
	// Each phase is timed where it happens: a restore of seconds is otherwise one
	// number, and which of the VMM's own start, the pager's attach and the
	// snapshot load it was is the whole question.
	phase := time.Now()
	if err := p.spawn(c, args, cancel); err != nil {
		return nil, err
	}
	connectErrors := p.attach(ctx, lifetime, c)
	if err := limitStateFiles(p.cmd.Process.Pid); err != nil {
		return nil, err
	}
	if err := p.awaitAPI(ctx); err != nil {
		return nil, err
	}
	phase, p.phases.ProcessNS = time.Now(), int64(time.Since(phase))
	if len(c.RestoreState) > 0 {
		if err := p.restore(ctx, c, overrides); err != nil {
			return nil, p.withSessions(err, connectErrors)
		}
	}
	phase, p.phases.StateLoadNS = time.Now(), int64(time.Since(phase))
	if err := p.awaitSessions(ctx, connectErrors); err != nil {
		return nil, err
	}
	phase, p.phases.SessionsNS = time.Now(), int64(time.Since(phase))
	if err := p.request(ctx, http.MethodGet, "/", nil); err != nil {
		return nil, p.withSessions(err, connectErrors)
	}
	p.phases.ReadyNS = int64(time.Since(phase))
	p.phases.Attachments = p.attachments()
	for _, name := range []string{"config.json", "restore.state"} {
		if err := p.files.remove(ctx, name); err != nil {
			return nil, err
		}
	}
	started = true
	go p.watch()
	return p, nil
}

// startable plans the machine's memory regions and fills in the connection settings a
// caller left at zero. It reports the plan, so a caller that has one has a
// configuration Start can build from.
func (c *Config) startable() (plan, error) {
	layout, err := c.plan()
	if err != nil {
		return plan{}, err
	}
	if c.Connection.CommandTimeout == 0 {
		c.Connection.CommandTimeout = 30 * time.Second
	}
	if c.Connection.VerifyInterval == 0 {
		c.Connection.VerifyInterval = time.Second
	}
	if c.Connection.QueuePages == 0 {
		c.Connection.QueuePages = int(min(1024, layout.ramBytes/c.Pagers.Ram.PageSize()))
	}
	if c.Scratch == nil {
		return plan{}, errors.New("vmmachine: shared scratch owner is required")
	}
	return layout, nil
}

// openScratch takes this machine's directory from the shared scratch and opens
// the state files the VMM reads its configuration and its snapshot out of. It
// is the first phase that owns anything: a failure after it must go through
// Close.
func (p *Process) openScratch(ctx context.Context, c Config) error {
	dir, err := c.Scratch.create(ctx, p)
	if err != nil {
		return err
	}
	p.dir = dir
	p.api = filepath.Join(dir, "api.sock")
	if c.VsockCID != 0 {
		p.vsock = filepath.Join(dir, "vsock.sock")
	}
	stateDisk, err := c.Scratch.disks(filepath.Join(dir, "state"))
	if err != nil {
		return err
	}
	p.files, err = newStateFiles(stateDisk)
	return err
}

// listen opens one Unix socket per memory region for the VMM to attach to, and
// describes the PMEM devices twice over: pmem is what a fresh boot's
// configuration file declares, overrides what a restore is told the sockets of
// the devices its snapshot already describes are.
func (p *Process) listen(layout plan) (pmem, overrides []map[string]any, err error) {
	if _, err := p.endpoint("ram", layout.ram); err != nil {
		return nil, nil, err
	}
	pmem = make([]map[string]any, 0, len(layout.pmem))
	overrides = make([]map[string]any, 0, len(layout.pmem))
	for i, r := range layout.pmem {
		e, err := p.endpoint("pmem-"+strconv.Itoa(i), r)
		if err != nil {
			return nil, nil, err
		}
		pmem = append(pmem, map[string]any{"id": r.name, "root_device": r.root, "managed": map[string]any{"socket_path": e.path, "length": r.volume.Size()}})
		overrides = append(overrides, map[string]any{"id": r.name, "socket_path": e.path})
	}
	return pmem, overrides, nil
}

// endpoint opens one memory region's socket and records it. The first is the RAM
// memory region's, which is the one the machine configuration and a restore's memory
// backend both name.
func (p *Process) endpoint(socket string, r memoryRegion) (*endpoint, error) {
	path := filepath.Join(p.dir, socket+".sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: path})
	if err != nil {
		return nil, err
	}
	e := &endpoint{listener: l, path: path, backing: r.backing, name: r.name}
	p.endpoints = append(p.endpoints, e)
	return e, nil
}

// ramEndpoint is the RAM memory region's socket, which listen opens first.
func (p *Process) ramEndpoint() *endpoint { return p.endpoints[0] }

// arguments is the VMM's command line, and for a fresh boot the configuration
// file it names: the kernel, the machine size, and every device this process
// has opened a socket for. A restore carries all of that in its snapshot and
// takes none of it here.
func (p *Process) arguments(ctx context.Context, c Config, layout plan, pmem []map[string]any) ([]string, error) {
	args := []string{"--api-sock", p.api, "--seccomp-filter", c.SeccompFilter}
	if len(c.RestoreState) > 0 {
		return args, nil
	}
	boot := map[string]any{"kernel_image_path": c.KernelPath, "boot_args": c.BootArgs}
	if c.InitrdPath != "" {
		boot["initrd_path"] = c.InitrdPath
	}
	// The machine names no huge-page setting: managed RAM is the pager's own
	// backing, and the session states its page when it attaches, so a setting
	// here would be this VM claiming something about memory it does not own.
	// The VMM refuses one.
	config := map[string]any{"managed-memory": map[string]any{"socket_path": p.ramEndpoint().path}, "machine-config": map[string]any{"mem_size_mib": layout.ramBytes >> 20, "vcpu_count": c.VCPUs}, "boot-source": boot, "drives": []any{}, "pmem": pmem}
	if p.vsock != "" {
		config["vsock"] = map[string]any{"guest_cid": c.VsockCID, "uds_path": p.vsock}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if err := p.files.write(ctx, "config.json", raw); err != nil {
		return nil, err
	}
	return append(args, "--config-file", filepath.Join(p.dir, "state", "config.json")), nil
}

// spawn starts the VMM with its console captured, the watcher that records its
// exit, and the client that reaches its API socket. The pipe on its standard
// input is what a console write goes to; the read end belongs to the child.
func (p *Process) spawn(c Config, args []string, cancel context.CancelCauseFunc) error {
	p.log = newConsoleRing()
	input, output, err := os.Pipe()
	if err != nil {
		return err
	}
	p.stdin = output
	p.cmd = exec.Command(c.Binary, args...)
	p.cmd.WaitDelay = time.Second
	p.cmd.Stdin = input
	p.cmd.Stdout = p.log
	p.cmd.Stderr = p.log
	if err := p.cmd.Start(); err != nil {
		_ = input.Close()
		return err
	}
	_ = input.Close()
	go func() {
		p.exitErr = p.cmd.Wait()
		cancel(errors.New("vmmachine: process exited"))
		close(p.done)
	}()
	api := p.api
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api)
	}, MaxConnsPerHost: 1}
	p.client = &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	return nil
}

// attach accepts the VMM's connection on every memory region's socket and builds the
// memory session behind it. The sessions come up concurrently and the VMM
// builds them inside its own requests, so the result of each is reported on the
// returned channel rather than waited for here.
func (p *Process) attach(ctx, lifetime context.Context, c Config) chan error {
	connectErrors := make(chan error, len(p.endpoints))
	var wg sync.WaitGroup
	for _, e := range p.endpoints {
		wg.Go(func() {
			_ = e.listener.SetDeadline(time.Now().Add(2 * time.Minute))
			socket, err := e.listener.AcceptUnix()
			_ = e.listener.Close()
			if err == nil {
				err = checkPeer(socket, p.cmd.Process.Pid)
			}
			if err == nil {
				// Each session is named for the volume it stands in front of,
				// which is how the failure that ends one says which memory region of
				// this machine it was.
				cfg := c.Connection
				cfg.Name = e.name
				// A memory region attaches to the pager of its own kind: the two have
				// separate arenas, and a page number of one means nothing in
				// the other. The pending-fault queue is counted in that pager's
				// page too, so it is bounded by this memory region's own pages rather
				// than by a number that would be a whole disk in one pager and
				// a fraction of the guest's memory in the other.
				pager := c.Pagers.For(e.backing.Kind)
				cfg.QueuePages = min(cfg.QueuePages, int(e.backing.Backing.Size()/pager.PageSize()))
				e.connection, err = vmmemory.Connect(lifetime, pager, socket, e.backing, cfg)
			} else if socket != nil {
				_ = socket.Close()
			}
			if err != nil {
				// The VMM sees only the descriptor it never received, and it
				// reports that to whatever request built the session. This is
				// the one place the reason exists, so it is written down here
				// whether or not anything reads the result below.
				slog.ErrorContext(ctx, "vmmachine: a memory session never attached",
					"vm", p.id, "memory_region", e.name, "socket", filepath.Base(e.path), "error", err)
			}
			connectErrors <- err
			if err == nil {
				go func() {
					if err := e.connection.Wait(context.Background()); err != nil {
						p.stop(fmt.Errorf("pager %s: %w", filepath.Base(e.path), err))
					}
				}()
			}
		})
	}
	go func() { wg.Wait(); close(p.connectionsReady) }()
	return connectErrors
}

// withSessions is what a request to the VMM has to be reported through once the
// sessions exist. The VMM builds them inside its own requests, so a memory region the
// pager refused fails the request with the only thing that side has — the
// descriptor that never arrived — and the reason is here, in a connect result
// nothing would otherwise read. Closing the process first is what makes those
// results final: Close kills the VMM, closes the listeners and waits for every
// connect goroutine, so the channel then holds everything there will ever be
// and draining it blocks on nothing.
func (p *Process) withSessions(err error, connectErrors chan error) error {
	_ = p.Close()
	errs := []error{err}
	for range p.endpoints {
		select {
		case connectErr := <-connectErrors:
			if connectErr != nil {
				errs = append(errs, connectErr)
			}
		default:
		}
	}
	return errors.Join(errs...)
}

// apiDeadline bounds the wait for the VMM's API socket. A VMM that neither
// binds it nor dies is a process that is never going to finish starting — a
// binary that is not the VMM, a seccomp filter it hangs inside — and a caller
// with no deadline of its own would otherwise wait on it for ever, holding a
// create open and a scratch directory with it. It is the same two minutes the
// memory region listeners give the same process to connect.
var apiDeadline = 2 * time.Minute

// awaitAPI waits for the VMM to bind its API socket, which is the first sign it
// got past its own seccomp filter. A VMM that dies instead is reported as the
// startup failure it is rather than waited out, and one that does neither is
// given up on at apiDeadline.
func (p *Process) awaitAPI(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	bound := time.NewTimer(apiDeadline)
	defer bound.Stop()
	for {
		if _, err := os.Stat(p.api); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-p.done:
			return p.failed("startup")
		case <-bound.C:
			return fmt.Errorf("vmmachine: the VMM did not bind its API socket within %s: %w",
				apiDeadline, context.DeadlineExceeded)
		case <-ticker.C:
		}
	}
}

// restore loads this machine's snapshot: the VMM's own state from the state
// file, its guest memory from the RAM memory region's session, and each PMEM device
// from the socket this process opened for it.
func (p *Process) restore(ctx context.Context, c Config, overrides []map[string]any) error {
	if err := p.files.write(ctx, "restore.state", c.RestoreState); err != nil {
		return err
	}
	load := map[string]any{"snapshot_path": filepath.Join(p.dir, "state", "restore.state"), "mem_backend": map[string]any{"backend_type": "Sproutfs", "backend_path": p.ramEndpoint().path}, "pmem_overrides": overrides, "resume_vm": false}
	// The device is in the state; only its host socket is this process's, so
	// the restore is told where this one put it.
	if p.vsock != "" {
		load["vsock_override"] = map[string]any{"uds_path": p.vsock}
	}
	return p.request(ctx, http.MethodPut, "/snapshot/load", load)
}

// awaitSessions waits for every memory region's session to be built or to fail. A VMM
// that exited first is reported as the attachment failure it is.
func (p *Process) awaitSessions(ctx context.Context, connectErrors chan error) error {
	for range p.endpoints {
		select {
		case err := <-connectErrors:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-p.done:
			return p.failed("attachment")
		}
	}
	return nil
}

// watch is the account of a VMM that died. A process nothing asked to stop —
// killed because its memory session failed, killed by the kernel, or crashed —
// leaves only sockets that no longer answer, and its owner finds out at the next
// thing it tries. This is the one record of why: the VM, the process, the cause
// the kill carried, and the tail of a console that goes with the process.
func (p *Process) watch() {
	<-p.done
	if p.closing.Load() {
		return
	}
	slog.Error("vmmachine: the VMM exited", "vm", p.id, "pid", p.PID(),
		"error", p.result(), "console", string(p.log.tail(consoleTailBytes)))
}

// consoleTailBytes is how much of a dead machine's console its diagnostic
// carries: enough for a guest panic, bounded so one death is one record.
const consoleTailBytes = 8192

func checkPeer(c *net.UnixConn, pid int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var checkErr error
	if err := raw.Control(func(fd uintptr) {
		cred, checkErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if checkErr != nil {
		return checkErr
	}
	if int(cred.Pid) != pid {
		return errors.New("vmmachine: pager peer is not the supervised VMM")
	}
	return nil
}

func (p *Process) request(ctx context.Context, method, path string, value any) error {
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://vm"+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return refusal{fmt.Errorf("vmmachine: %s %s: %s: %s", method, path, response.Status, raw)}
	}
	return nil
}

// controlTimeout bounds one VMM control operation. A request to the VMM is not
// the caller's request: a pause, a capture, a resume or a release abandoned
// halfway leaves this process in a state only a kill resolves — a pause whose
// answer nobody read is a guest that may or may not be running — and killing it
// loses every write since the last checkpoint that landed. So the operations
// below run to their own end on a deadline of their own, and the caller's
// context bounds only the wait for this process's lock, where nothing has
// started yet. It is the API client's own timeout.
const controlTimeout = 2 * time.Minute

// operation is the context one control operation runs under: not the caller's,
// and bounded by controlTimeout. An HTTP client that disconnected, a request
// whose deadline passed, a caller that gave up — none of them may reach the VMM
// as a cancelled request.
func operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
}

// refusal is a request the VMM answered and refused. It is the difference
// between an operation that failed and a process whose state is unknown: the
// VMM replied, so it is running and is no longer doing whatever the request
// asked of it, and the caller may act on the refusal instead of killing it. A
// request that got no answer carries no refusal and must be treated as still in
// flight.
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// Prepare pauses the VM and returns its VMM state together with the sealed
// checkpoint of every memory region, by the name of the volume it maps. It implements
// the capture coordinator's runtime. Sealing every memory region is part of the
// snapshot request and moves no bytes, so the pause this opens ends at Resume
// rather than when the checkpoint is uploaded. Publication belongs to the
// capture coordinator, which reads these checkpoints and retires them.
func (p *Process) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	if err := p.mu.Lock(ctx); err != nil {
		return nil, nil, err
	}
	defer p.mu.Unlock()
	ctx, cancel := operation(ctx)
	defer cancel()
	state, err := p.prepare(ctx, capturedForCheckpoint)
	if err != nil {
		return nil, nil, err
	}
	named := p.namedMemoryRegions()
	sources := make(map[string]volume.DirtySource, len(named))
	for name, memoryRegion := range named {
		checkpoint := memoryRegion.Checkpoint()
		if checkpoint == nil {
			return nil, nil, fmt.Errorf("vmmachine: memory region %q was not sealed by the capture", name)
		}
		sources[name] = checkpoint
	}
	return state, sources, nil
}

// SealDisks pauses the VM and seals the memory regions of its disks, and returns their
// checkpoints by the name of the volume each maps. It is the pause of a disk
// checkpoint: nothing asks the VMM for its state and its RAM is left as it is,
// so the pause is the vCPUs stopping and the disks' write-protect commands.
// The vCPUs stay paused until Resume; Release unseals the disks and resumes.
func (p *Process) SealDisks(ctx context.Context) (map[string]volume.DirtySource, error) {
	if err := p.mu.Lock(ctx); err != nil {
		return nil, err
	}
	defer p.mu.Unlock()
	ctx, cancel := operation(ctx)
	defer cancel()
	if p.running {
		if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
			if errors.As(err, &refusal{}) {
				return nil, err
			}
			p.stop(err)
			<-p.done
			return nil, err
		}
		p.running = false
	}
	sources := map[string]volume.DirtySource{}
	for name, memoryRegion := range p.namedMemoryRegions() {
		if memoryRegion.Kind() != vmmemory.Pmem {
			continue
		}
		if err := memoryRegion.Seal(ctx); err != nil {
			return nil, fmt.Errorf("vmmachine: sealing disk %q: %w", name, err)
		}
		sources[name] = memoryRegion.Checkpoint()
	}
	return sources, nil
}

// captureKind is what one state capture is for. The VMM is told, because a
// device holding something on the guest's behalf acts on it: a vsock connection
// to a guest that will run again is left alone, and one to a guest that will not
// is closed.
type captureKind bool

const (
	// capturedForCheckpoint is a capture the same VM resumes from.
	capturedForCheckpoint captureKind = false
	// capturedForHandoff is a migration's stop: this guest is stopped for good
	// and a destination starts from the state.
	capturedForHandoff captureKind = true
)

// The kernel enforces the same maximum reserved for the external snapshot
// writer. Set it before any snapshot API request; stdout/stderr use pipes.
func limitStateFiles(pid int) error {
	limit := syscall.Rlimit{Cur: MaxStateBytes, Max: MaxStateBytes}
	_, _, errno := syscall.Syscall6(syscall.SYS_PRLIMIT64, uintptr(pid), syscall.RLIMIT_FSIZE, uintptr(unsafe.Pointer(&limit)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("vmmachine: bound snapshot file writes: %w", errno)
	}
	return nil
}

// prepare pauses the VM, drains its device completions and captures its VMM
// state. Which of the two things the capture is decides two more: a checkpoint
// takes the checkpoint of every memory region and leaves the guest's vsock connections
// alone, because the same guest resumes and whatever was running over one of
// them goes on running. A handoff seals nothing — the pages it would seal are
// the ones the destination is about to fault out of this host's pages — and
// drops those connections, because this guest is not coming back and a host
// waiting on a command in it would otherwise wait out its own timeout.
func (p *Process) prepare(ctx context.Context, kind captureKind) ([]byte, error) {
	handoff := kind == capturedForHandoff
	return p.files.capture(ctx, func() error {
		if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
			// A refusal is a pause that did not happen: the VMM answered, so it
			// is running and its vCPUs are where they were. The capture fails
			// and the guest carries on, which is what a checkpoint that could
			// not be taken costs. Anything else leaves the state unknown.
			if errors.As(err, &refusal{}) {
				return err
			}
			p.stop(err)
			<-p.done
			return err
		}
		p.running = false
		path := filepath.Join(p.dir, "state", "capture.state")
		// The state file is staging, never recovery authority: it is read back
		// and deleted before the guest resumes, and a host that restarts wipes
		// the whole directory. Syncing it would put a disk flush inside the
		// pause for bytes nothing will ever look for.
		if err := p.request(ctx, http.MethodPut, "/snapshot/create", map[string]any{"snapshot_type": "Full", "snapshot_path": path, "managed": true, "seal": !handoff, "handoff": handoff, "sync_snapshot_files": false}); err != nil {
			// A refusal is a capture that did not happen: the VMM answered, so
			// it is running and writing nothing, and every memory region it sealed is
			// unsealed by the Release its caller runs. A seal the host could
			// not finish in time arrives here, and killing the guest for it
			// would lose every write since the last checkpoint that landed.
			if errors.As(err, &refusal{}) {
				return err
			}
			// An uncertain response can leave the VMM writing. Wait for exit
			// before removing its file or returning the reserved working space.
			p.stop(err)
			<-p.done
			return err
		}
		return nil
	})
}

// Pause stops this machine's vCPUs and does nothing else: no state is captured,
// no memory region is sealed and nothing is written anywhere. Resume starts them
// again. It is the first step of a capture on its own, which is what tells a
// guest that cannot survive being stopped from one that cannot survive being
// captured.
func (p *Process) Pause(ctx context.Context) error {
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	defer p.mu.Unlock()
	ctx, cancel := operation(ctx)
	defer cancel()
	if !p.running {
		return nil
	}
	if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
		// A refusal is a pause that did not happen: the VMM answered, so it is
		// running and its vCPUs are where they were. Anything else leaves the
		// state unknown, and the machine is stopped rather than guessed about.
		if errors.As(err, &refusal{}) {
			return err
		}
		p.stop(err)
		<-p.done
		return err
	}
	p.running = false
	return nil
}

// Resume restarts the vCPUs once the capture's state has been read. The memory regions
// stay sealed and the checkpoint uploads their checkpoints behind the running
// guest, so the pause a capture costs is Prepare plus this call.
func (p *Process) Resume(ctx context.Context) error {
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	defer p.mu.Unlock()
	ctx, cancel := operation(ctx)
	defer cancel()
	return p.resume(ctx)
}

func (p *Process) resume(ctx context.Context) error {
	if p.running {
		return nil
	}
	if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Resumed"}); err != nil {
		p.stop(err)
		return err
	}
	p.running = true
	return nil
}

// Release unseals independent volumes concurrently and resumes a VM the capture
// left paused. Unsealing hands every sealed page back to the guest as ordinary
// dirty state, so the next checkpoint takes it again.
func (p *Process) Release(ctx context.Context) error {
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	defer p.mu.Unlock()
	// This is the way back from a capture that failed, so it above all runs on
	// a context of its own: the caller running it has usually just been handed
	// the failure of the context it was given.
	ctx, cancel := operation(ctx)
	defer cancel()
	if err := p.memoryRegions(func(r vmmemory.ConnectedMemoryRegion) error { return r.Memory.Unseal(ctx) }); err != nil {
		p.stop(err)
		return err
	}
	return p.resume(ctx)
}

// Stop pauses the VM for good and returns the VMM state the destination of a
// migration starts from. It is the migration's stop phase: the vCPUs pause,
// device completions drain and the state is captured. It seals nothing and
// waits for nothing — the pages written since the last checkpoint stay in this
// host's pages and the destination faults them out of it, which is what bounds
// the pause. The process stays paused; unlike Prepare, which is a capture the
// same VM resumes from, nothing here brings it back. A caller that abandons the
// migration can still Release it, which resumes the guest.
func (p *Process) Stop(ctx context.Context) ([]byte, error) {
	if err := p.mu.Lock(ctx); err != nil {
		return nil, err
	}
	defer p.mu.Unlock()
	ctx, cancel := operation(ctx)
	defer cancel()
	return p.prepare(ctx, capturedForHandoff)
}

// MemoryRegions reports this machine's memory regions by the volume each one maps:
// "ram0" and one entry per PMEM device id. A migration serves and hands off
// memory regions under those names, which are the names the destination opens the same
// volumes under.
func (p *Process) MemoryRegions() map[string]*vmmemory.MemoryRegion { return p.namedMemoryRegions() }

// attachments is what every memory region's session cost to build, by the volume it
// maps, which is the pager's own share of a start.
func (p *Process) attachments() map[string]vmmemory.AttachStats {
	result := make(map[string]vmmemory.AttachStats, len(p.endpoints))
	for _, e := range p.endpoints {
		if e.connection != nil {
			result[e.name] = e.connection.Attach()
		}
	}
	return result
}

func (p *Process) namedMemoryRegions() map[string]*vmmemory.MemoryRegion {
	result := make(map[string]*vmmemory.MemoryRegion)
	for _, e := range p.endpoints {
		if e.connection == nil {
			continue
		}
		result[e.name] = e.connection.MemoryRegion().Memory
	}
	return result
}

// memory regions runs fn for every attached memory region concurrently and joins the results.
func (p *Process) memoryRegions(fn func(vmmemory.ConnectedMemoryRegion) error) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result error
	for _, e := range p.endpoints {
		wg.Go(func() {
			if err := fn(e.connection.MemoryRegion()); err != nil {
				mu.Lock()
				result = errors.Join(result, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return result
}

// Console reads this machine's serial output from the in-memory ring that
// retains its newest 1 MiB. A reader asking for an offset the ring has already
// dropped is answered from the oldest byte still retained: the returned from
// offset is what the data begins at, and next is what to ask for after it.
func (p *Process) Console(offset int64, limit int) (data []byte, from, next int64) {
	return p.log.read(offset, limit)
}

// Directory is this process's private scratch directory, which holds its
// sockets and staging files and is removed with the process. Nothing in it is
// authority for anything; it is a diagnostic path.
func (p *Process) Directory() string { return p.dir }

// VsockPath is the Unix socket Firecracker listens on for this machine's
// virtio-vsock device, and empty for a machine configured without one. A host
// reaches software in the guest by connecting to it and asking for a guest
// port. It lives in this process's scratch directory and goes with it, so a
// migrated or forked VM is reached at the path of whichever process runs it.
func (p *Process) VsockPath() string { return p.vsock }

// PID identifies the supervised process for host resource accounting.
func (p *Process) PID() int { return p.cmd.Process.Pid }

func (p *Process) WriteConsole(ctx context.Context, data []byte) error {
	if len(data) > 4096 {
		return errors.New("vmmachine: console write too large")
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := p.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := p.stdin.Write(data)
	return err
}

func (p *Process) failed(stage string) error {
	raw := p.log.tail(consoleTailBytes)
	return fmt.Errorf("vmmachine: %s: process exited: %w: %s", stage, p.result(), raw)
}

func (p *Process) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-p.done:
		return p.result()
	}
}

// Close stops all vCPUs and kernel/device users before detaching mappings. It
// removes every private socket and staging file it created. Console output is
// in memory and goes with the process.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		// This exit is asked for, so it is not the death watch reports.
		p.closing.Store(true)
		p.cancel(errors.New("vmmachine: process closed"))
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		for _, e := range p.endpoints {
			_ = e.listener.Close()
		}
		if p.cmd != nil && p.cmd.Process != nil {
			<-p.connectionsReady
		}
		for _, e := range p.endpoints {
			if e.connection != nil {
				p.closeErr = errors.Join(p.closeErr, e.connection.Close(context.Background()))
			}
		}
		if p.client != nil {
			p.client.CloseIdleConnections()
		}
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
	})
	if p.scratch != nil {
		p.scratch.stopped(p)
	}
	// Capture owns state handles under this mutex; wait for it to observe the
	// stopped process before reconciling and deleting its files.
	if err := p.mu.Lock(context.Background()); err != nil {
		return errors.Join(p.closeErr, err)
	}
	defer p.mu.Unlock()
	if p.files != nil {
		if err := p.files.Close(); err != nil {
			return errors.Join(p.closeErr, err)
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		return errors.Join(p.closeErr, err)
	}
	if p.scratch != nil {
		p.scratch.removed(p)
	}
	return p.closeErr
}
