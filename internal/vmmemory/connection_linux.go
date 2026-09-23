//go:build linux && (amd64 || arm64)

package vmmemory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/vmmemory/internal/pageranges"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

type ConnectionConfig struct {
	// Name identifies this session's region in what it logs: the volume the
	// backing stands in front of. It is a diagnostic only — nothing selects a
	// volume, a page or an authority by it — and empty is allowed.
	Name string
	// MaxVMAs bounds the client process's mapping count for the replacements
	// this pager drives. Zero disables the budget entirely, which is what a
	// jailed client without /proc needs; a nonzero value is the admission
	// limit and requires /proc/self/maps in the client to be accountable.
	MaxVMAs int
	// QueuePages bounds distinct pending faults. Overflow terminates service;
	// the UFFD reader never waits on the worker while draining REMAP events.
	QueuePages int
	// FaultWorkers bounds faults served concurrently. Faults in one read-ahead
	// window still serialize. Zero selects 8.
	FaultWorkers   int
	CommandTimeout time.Duration
	VerifyInterval time.Duration
}
type ConnectedRegion struct {
	Kind            RegionKind
	Address, Length uint64
	Memory          *Region
}

// Connection serves one Rust Session, which maps exactly one region. Wait
// reports terminal failure; its owner must stop the process before Close
// releases possibly mapped slots. Neither a transport disconnect nor
// cancellation proves that memory users have stopped.
type Connection struct {
	host    *Host
	socket  *net.UnixConn
	uffd    *os.File
	region  ConnectedRegion
	mapping *remoteMapping
	cfg     ConnectionConfig
	ctx     context.Context
	cancel  context.CancelCauseFunc
	// reported admits the one failure this session is logged as ending on.
	reported  sync.Once
	commandMu *ctxsync.Mutex
	writeMu   *ctxsync.Mutex
	acks      chan vmwire.Frame
	requests  chan vmwire.Frame
	sequence  uint64
	queueMu   sync.Mutex
	queue     map[uint64]queuedFault
	inflight  map[uint64]struct{}
	notify    chan struct{}
	workers   sync.WaitGroup
	closeMu   *ctxsync.Mutex
	closed    bool
}

// queuedFault is one page waiting for a worker: whether any of its trapped
// accesses was a store, and when the first of them was read from the UFFD,
// which is what the queue-delay histogram measures against.
type queuedFault struct {
	write bool
	at    time.Time
}
type remoteMapping struct {
	connection *Connection
	address    uint64
	pageCount  uint64
	// pageSize is the page every offset and length on this session's wire is
	// counted in. It is the pager's, which Connect has already refused unless
	// it is the one the transport maps.
	pageSize uint64
	states   pageranges.Map
	// mu is exclusive for the commands that advance generations and shared for
	// the state reads a resolution needs. No UFFD ioctl runs under it: a long
	// population must never block a mapping command.
	mu *ctxsync.RWMutex
}

// Connect consumes an already accepted local socket and verifies the advertised
// region against trusted configuration. The caller must authenticate the peer
// and authorize its volume identity before calling. No identity supplied by the
// Rust process selects a volume.
// An error may return a retained Connection once mappings can be installed;
// stop the client process before closing that connection and releasing aliases.
func Connect(ctx context.Context, h *Host, socket *net.UnixConn, backing RegionBacking, cfg ConnectionConfig) (*Connection, error) {
	if h == nil || socket == nil {
		return nil, ErrConfig
	}
	// The page this session runs is the pager's, and the arena is the memory
	// that page is made of. A pager whose page this transport does not map, or
	// whose arena was made with another slot, is refused here rather than
	// serving faults the client would read in the wrong unit; the geometry it
	// does state goes out on the attachment below, where the client checks it
	// against the descriptor it is given.
	if _, err := vmwire.BackingFor(h.pageSize); err != nil {
		_ = socket.Close()
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	a, ok := h.arena.(*LinuxArena)
	if cfg.FaultWorkers == 0 {
		cfg.FaultWorkers = 8
	}
	if !ok || h.cfg.ArenaOffsets != a.offsets || uint64(a.pageSize) != h.pageSize || backing.Backing == nil ||
		(backing.Kind != Pmem && backing.Kind != Ram) || cfg.QueuePages < 1 || cfg.QueuePages > h.cfg.LogicalPages || cfg.FaultWorkers < 1 || cfg.FaultWorkers > 64 || cfg.MaxVMAs < 0 || (cfg.MaxVMAs > 0 && cfg.MaxVMAs < 128) || cfg.MaxVMAs > 1<<20 || cfg.CommandTimeout <= 0 || cfg.VerifyInterval <= 0 {
		_ = socket.Close()
		return nil, ErrConfig
	}
	// Descriptor exchange precedes the session workers. Wake its socket reads
	// on cancellation too, including contexts without a startup deadline.
	stopHandshake := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stopHandshake()
	// The deadline below is the kernel's, which knows only the wall clock: a
	// socket deadline is an instant the operating system compares against, not
	// something this process can be given its own reading of.
	deadline := time.Now().Add(cfg.CommandTimeout) // wall clock: kernel socket deadline
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := socket.SetDeadline(deadline); err != nil {
		_ = socket.Close()
		return nil, errors.Join(err, context.Cause(ctx))
	}
	header, fd, err := vmwire.ReceiveFD(socket)
	if err != nil {
		_ = socket.Close()
		return nil, errors.Join(err, context.Cause(ctx))
	}
	sessionCtx, cancel := context.WithCancelCause(ctx)
	c := &Connection{host: h, socket: socket, uffd: fd, cfg: cfg, ctx: sessionCtx, cancel: cancel, commandMu: ctxsync.NewMutex(), writeMu: ctxsync.NewMutex(), acks: make(chan vmwire.Frame, 1), requests: make(chan vmwire.Frame, 1), closeMu: ctxsync.NewMutex(), queue: make(map[uint64]queuedFault), inflight: make(map[uint64]struct{}), notify: make(chan struct{}, cfg.FaultWorkers)}
	attached := false
	fail := func(err error) (*Connection, error) {
		err = errors.Join(err, context.Cause(ctx))
		// The client sees only a socket that closed before its backing came,
		// and reports that to whatever request built the session. This is where
		// the reason is, so it is written down here as well as returned.
		slog.ErrorContext(ctx, "vmmemory: a memory session never attached",
			"region", cfg.Name, "kind", backing.Kind, "error", err)
		cancel(err)
		_ = socket.Close()
		if attached {
			c.workers.Wait()
			return c, err
		}
		_ = fd.Close()
		if c.region.Memory != nil {
			if detachErr := c.region.Memory.Detach(context.Background()); detachErr != nil {
				err = errors.Join(err, detachErr)
			}
		}
		return nil, err
	}
	// The hello carries nothing but the version: one session is one region, and
	// what page that region runs is the attachment's to say. A version 6 peer
	// fails here, which is the whole of this build's support for one — its page
	// numbers mean something else.
	if header.Kind != vmwire.Hello || header.ID != vmwire.Version || header.Length != 0 || header.Offset != 0 || header.Backing != 0 || header.Generation != 0 || header.Flags != 0 {
		return fail(errors.New("invalid managed-memory hello"))
	}
	if err := syscall.SetNonblock(int(fd.Fd()), true); err != nil {
		return fail(err)
	}
	f, err := vmwire.Read(socket)
	if err != nil {
		return fail(err)
	}
	if f.Kind != vmwire.Region || f.Flags != uint64(backing.Kind) || f.Length != backing.Backing.Size() || f.Length == 0 || f.Length%h.pageSize != 0 || f.Offset%h.pageSize != 0 || f.Offset > ^uint64(0)-f.Length || f.ID != 0 || f.Backing != 0 || f.Generation != 0 {
		return fail(errors.New("invalid managed-memory region"))
	}
	// Admission bounds logical capacity; untouched generations are implicit.
	m := &remoteMapping{connection: c, address: f.Offset, pageCount: f.Length / h.pageSize, pageSize: h.pageSize, mu: ctxsync.NewRWMutex()}
	r, err := h.admit(ctx, backing, m)
	if err != nil {
		return fail(err)
	}
	c.region = ConnectedRegion{backing.Kind, f.Offset, f.Length, r}
	c.mapping = m
	// The attachment states the geometry: this region's page, the arena's offset
	// space, what the arena is made of, and the mapping-count budget. The arena
	// is a sparse file, so what is stated is its addresses and not the memory
	// behind them. The client refuses a page it does not map, a descriptor whose
	// length is not the offset space claimed or whose filesystem is not the
	// memory that page is, or a region of its own that is not whole pages of it
	// — all before it exposes an address to the VMM.
	attach := vmwire.AttachFrame(h.pageSize, uint64(a.offsets)*uint64(a.pageSize), a.backing, uint64(cfg.MaxVMAs))
	if err := vmwire.SendFD(socket, attach, a.file); err != nil {
		return fail(err)
	}
	attached = true
	if err := socket.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	context.AfterFunc(sessionCtx, func() { _ = socket.Close() })
	c.workers.Add(4 + cfg.FaultWorkers)
	go c.work()
	go c.verify()
	for range cfg.FaultWorkers {
		go c.serveFaults()
	}
	go c.readFaults()
	go c.readControl()
	if err := c.Populate(ctx); err != nil {
		return fail(err)
	}
	if err := c.command(ctx, vmwire.Frame{Kind: vmwire.Ready}); err != nil {
		c.fail(err)
		return fail(err)
	}
	return c, nil
}

// Populate maps matching resident identities without loading absent pages.
// Connect calls it while Rust serves commands before exposing addresses to the
// VMM. An explicit later call requires quiescent guest memory.
func (c *Connection) Populate(ctx context.Context) error {
	if err := c.region.Memory.Populate(ctx); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

// Region is the one region this session maps.
func (c *Connection) Region() ConnectedRegion { return c.region }
func (c *Connection) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
}

// fail ends the session. Its owner learns only that the connection is gone — it
// kills the client process and has no way back to what happened here — so this
// is where the error that ended a VM's memory is written down. Which region it
// was is this session's name, and which page it was is in the error the caller
// built. Only the first failure is logged: everything the session does after it
// fails with the cancellation this installs, and those are consequences.
func (c *Connection) fail(err error) {
	c.reported.Do(func() {
		attrs := []any{"region", c.cfg.Name, "address", c.region.Address}
		var command *commandFailure
		if errors.As(err, &command) {
			attrs = append(attrs, command.attrs()...)
		}
		slog.Error("vmmemory: the memory session failed", append(attrs, "error", err)...)
	})
	c.cancel(err)
	_ = c.socket.Close()
}

// commandFailure is the command a session was refused or failed on.
//
// The owner of a session sees only that the connection is gone: it kills the
// client process and has no way back to what was asked for, and what the client
// answers a refusal with is one errno. So the command travels with the error —
// its kind, its identifier, the region offset and length it covers, the arena
// offset it maps from, the generation it advances, its flags, and how many runs
// followed it — and goes out as structured fields on the one line a failed
// session logs. Without it an "invalid argument" on a host names neither the
// pages it was about nor which of six commands it was.
type commandFailure struct {
	frame vmwire.Frame
	// runs is how many run frames followed the command, which is zero for every
	// command but a batch. A batch's own length is the same number.
	runs int
	err  error
}

func (f *commandFailure) Unwrap() error { return f.err }

func (f *commandFailure) Error() string {
	return fmt.Sprintf("%v (%s command id %d offset %d length %d backing %d generation %d flags %d runs %d)",
		f.err, vmwire.KindName(f.frame.Kind), f.frame.ID, f.frame.Offset, f.frame.Length,
		f.frame.Backing, f.frame.Generation, f.frame.Flags, f.runs)
}

func (f *commandFailure) attrs() []any {
	return []any{"command", vmwire.KindName(f.frame.Kind), "id", f.frame.ID,
		"offset", f.frame.Offset, "length", f.frame.Length, "backing", f.frame.Backing,
		"generation", f.frame.Generation, "flags", f.frame.Flags, "runs", f.runs}
}

func (c *Connection) command(ctx context.Context, f vmwire.Frame) error {
	return c.commandFrames(ctx, f, nil)
}

func (c *Connection) commandFrames(ctx context.Context, f vmwire.Frame, runs []vmwire.Frame) error {
	if err := c.commandMu.Lock(ctx); err != nil {
		return err
	}
	defer c.commandMu.Unlock()
	if err := context.Cause(c.ctx); err != nil {
		return err
	}
	if c.sequence == ^uint64(0) {
		return errors.New("mapping command IDs exhausted")
	}
	c.sequence++
	f.ID = c.sequence
	for i := range runs {
		runs[i].ID = f.ID
	}
	// Every way this command can end names the command, because the failure a
	// session reports is all its owner ever learns about it.
	failed := func(err error) error { return &commandFailure{frame: f, runs: len(runs), err: err} }
	if err := c.sendFrames(ctx, append([]vmwire.Frame{f}, runs...)); err != nil {
		return failed(err)
	}
	var response vmwire.Frame
	timer := c.host.clock.NewTimer(c.cfg.CommandTimeout)
	defer timer.Stop()
	select {
	case response = <-c.acks:
	case <-ctx.Done():
		return failed(context.Cause(ctx))
	case <-c.ctx.Done():
		// The reader publishes a final ACK before recording a following EOF.
		// Prefer that evidence when the peer closes immediately after STOP.
		select {
		case response = <-c.acks:
		default:
			return failed(context.Cause(c.ctx))
		}
	case <-timer.C():
		return failed(context.DeadlineExceeded)
	}
	if response.Kind != vmwire.Ack || response.ID != f.ID || response.Generation != f.Generation || response.Offset != 0 || response.Length != 0 || response.Backing != 0 {
		return failed(errors.New("invalid mapping acknowledgement"))
	}
	if response.Flags != 0 {
		// The client validates a command and admits it against its mapping
		// budget before it touches anything, and answers a refusal as an
		// ordinary acknowledgement; a failure after that point ends the session
		// without one. So a flagged acknowledgement means nothing moved.
		return failed(fmt.Errorf("%w: %w", ErrMappingRefused, syscall.Errno(response.Flags)))
	}
	return nil
}

// frames appends one frame of the given kind per span of the run whose pages
// share a generation, since the protocol advances every page of a frame from
// the same one. Caller holds the mapping lock.
func (m *remoteMapping) frames(frames []vmwire.Frame, kind uint64, run MapRun, writable bool) ([]vmwire.Frame, error) {
	if run.Count < 1 || run.Page >= m.pageCount || uint64(run.Count) > m.pageCount-run.Page {
		return nil, ErrRange
	}
	size := m.pageSize
	for done := 0; done < run.Count; {
		page := run.Page + uint64(done)
		state, end := m.states.Run(page, run.Page+uint64(run.Count))
		if state.Generation == ^uint64(0) {
			return nil, ErrRange
		}
		count := int(end - page)
		f := vmwire.Frame{Kind: kind, Offset: page * size, Length: uint64(count) * size, Generation: state.Generation + 1}
		switch kind {
		case vmwire.MapRange:
			f.Backing = uint64(run.Slot+done) * size
			if !writable {
				f.Flags = 1
			}
		case vmwire.MapZero:
			f.Flags = 1
		}
		frames = append(frames, f)
		done += count
	}
	return frames, nil
}

// commit sends frames as the fewest commands the protocol allows and records
// the generations each acknowledged command advanced: a lone frame is a plain
// command, and more go as bounded batches, so a range whose pages sit at
// different generations still costs one round trip. Caller holds the mapping
// lock.
//
// A refusal changed nothing, so it fails the command and not the session — but
// only while this commit has applied nothing. Once one of its commands has
// landed, the caller knows the runs it gave but not the frames they became, so
// it cannot tell which pages a later refusal leaves mapped: that is as
// ambiguous as an acknowledgement that never arrived, and terminal like one.
// Every other failure is terminal whatever it has applied.
func (m *remoteMapping) commit(ctx context.Context, frames []vmwire.Frame) (commands, runs int, err error) {
	size := m.pageSize
	for len(frames) > 0 {
		count := min(len(frames), vmwire.MaxBatchRuns)
		batch := frames[:count]
		if count == 1 {
			err = m.connection.command(ctx, batch[0])
		} else {
			err = m.connection.commandFrames(ctx, vmwire.Frame{Kind: vmwire.MapBatch, Length: uint64(count)}, batch)
		}
		if err != nil {
			if errors.Is(err, ErrMappingRefused) && commands == 0 {
				return commands, runs, err
			}
			if errors.Is(err, ErrMappingRefused) {
				// The caller cannot tell which of its runs the commands before
				// this one mapped, so this is not the failure that changed
				// nothing. The chain is broken deliberately: nothing above may
				// take this for a refusal and unrecord pages that are mapped.
				err = fmt.Errorf("mapping refused after %d of this batch's commands landed: %v", commands, err)
			}
			m.connection.fail(err)
			return commands, runs, err
		}
		commands++
		runs += count
		for _, f := range batch {
			m.states.Set(f.Offset/size, (f.Offset+f.Length)/size, pageranges.State{Generation: f.Generation, Zero: f.Kind == vmwire.MapZero})
		}
		frames = frames[count:]
	}
	return commands, runs, nil
}

// change replaces one run of pages with a single mapping kind.
func (m *remoteMapping) change(ctx context.Context, page uint64, slot, count int, writable bool, kind uint64) error {
	if err := m.mu.Lock(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	frames, err := m.frames(nil, kind, MapRun{Page: page, Slot: slot, Count: count}, writable)
	if err != nil {
		return err
	}
	_, _, err = m.commit(ctx, frames)
	return err
}
func (m *remoteMapping) Map(ctx context.Context, page uint64, slot, count int, writable bool) error {
	return m.change(ctx, page, slot, count, writable, vmwire.MapRange)
}

func (m *remoteMapping) MapZero(ctx context.Context, page uint64, count int) error {
	_, _, err := m.MapBatch(ctx, []MapRun{{Page: page, Count: count, Zero: true}})
	return err
}

func (m *remoteMapping) RevokeBatch(ctx context.Context, ranges []PageRun) (int, int, error) {
	runs := make([]MapRun, len(ranges))
	for i, r := range ranges {
		runs[i] = MapRun{Page: r.Page, Count: r.Count}
	}
	return m.batch(ctx, runs, true)
}
func (m *remoteMapping) MapBatch(ctx context.Context, runs []MapRun) (int, int, error) {
	return m.batch(ctx, runs, false)
}
func (m *remoteMapping) batch(ctx context.Context, runs []MapRun, revoke bool) (int, int, error) {
	if err := m.mu.Lock(ctx); err != nil {
		return 0, 0, err
	}
	defer m.mu.Unlock()
	var frames []vmwire.Frame
	for _, run := range runs {
		kind := uint64(vmwire.MapRange)
		if revoke {
			kind = vmwire.Revoke
		} else if run.Zero {
			kind = vmwire.MapZero
		}
		var err error
		if frames, err = m.frames(frames, kind, run, false); err != nil {
			return 0, 0, err
		}
	}
	return m.commit(ctx, frames)
}
func (m *remoteMapping) Revoke(ctx context.Context, page uint64) error {
	return m.change(ctx, page, 0, 1, false, vmwire.Revoke)
}

// Protect write-protects a whole run in place with one UFFD ioctl. It sends no
// mapping command and advances no generation: the client's mappings are not
// touched, so nothing about them has to be acknowledged, and the run may cover
// as many of them as it likes. Like the ioctls Resolve issues, it runs under no
// mapping lock; the region's exclusive lock is what keeps this range's mappings
// from changing underneath it.
func (m *remoteMapping) Protect(ctx context.Context, page uint64, count int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if count < 1 || page >= m.pageCount || uint64(count) > m.pageCount-page {
		return ErrRange
	}
	size := m.pageSize
	if err := vmwire.ProtectRange(m.connection.uffd.Fd(), m.address+page*size, uint64(count)*size); err != nil {
		err = fmt.Errorf("protecting %d pages from page %d: %w", count, page, err)
		m.connection.fail(err)
		return err
	}
	return nil
}
func (m *remoteMapping) Resolve(ctx context.Context, page uint64, count int, writable bool) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if count < 1 || page >= m.pageCount || uint64(count) > m.pageCount-page {
		return ErrRange
	}
	size := m.pageSize
	// Read the mapping states under the shared lock and release it before any
	// ioctl, so a long population cannot block a mapping command.
	zero := false
	if !writable {
		if err := m.mu.RLock(ctx); err != nil {
			return err
		}
		zero = m.states.Get(page).Zero
		if zero {
			for cursor := page; cursor < page+uint64(count); {
				state, end := m.states.Run(cursor, page+uint64(count))
				if !state.Zero {
					m.mu.RUnlock()
					return ErrRange
				}
				cursor = end
			}
		}
		m.mu.RUnlock()
	}
	if zero {
		// Sparse zero runs are populated and protected before their temporary
		// anonymous mapping is exposed, so they need no HugeTLB CONTINUE.
		if err := vmwire.WakeRange(m.connection.uffd.Fd(), m.address+page*size, uint64(count)*size); err != nil {
			return fmt.Errorf("waking %d zero pages from page %d: %w", count, page, err)
		}
		return nil
	}
	// One CONTINUE covers every HugeTLB page of the run; transient mapping races
	// retry EAGAIN.
	if err := vmwire.Resolve(m.connection.uffd.Fd(), m.address+page*size, uint64(count)*size, m.pageSize, writable); err != nil {
		err = fmt.Errorf("resolving %d pages from page %d writable=%t: %w", count, page, writable, err)
		m.connection.fail(err)
		return err
	}
	return nil
}

func (c *Connection) readFaults() {
	defer c.workers.Done()
	// The duplicate is nonblocking before NewFile sees it, so its RawConn
	// uses Go's shared readiness poller rather than parking an OS thread per
	// connection. Closing it wakes cancellation without closing the original
	// UFFD, which must survive until all memory users have stopped.
	fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, c.uffd.Fd(), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		c.fail(fmt.Errorf("duplicate UFFD reader: %w", errno))
		return
	}
	reader := os.NewFile(fd, "uffd-events")
	defer reader.Close()
	if err := reader.SetReadDeadline(time.Time{}); err != nil {
		c.fail(fmt.Errorf("UFFD readiness unavailable: %w", err))
		return
	}
	raw, err := reader.SyscallConn()
	if err != nil {
		c.fail(err)
		return
	}
	stopWake := context.AfterFunc(c.ctx, func() { _ = reader.Close() })
	defer stopWake()
	var b [32]byte
	for c.ctx.Err() == nil {
		var n int
		var readErr error
		pollErr := raw.Read(func(fd uintptr) bool {
			for {
				// Counted outside the host lock: an idle descriptor read must
				// not contend with page transitions.
				c.host.uffdReads.Add(1)
				n, readErr = syscall.Read(int(fd), b[:])
				if !errors.Is(readErr, syscall.EINTR) {
					return !errors.Is(readErr, syscall.EAGAIN)
				}
			}
		})
		if pollErr != nil || readErr != nil || n != 32 {
			c.fail(fmt.Errorf("UFFD event read: %d bytes, %v", n, errors.Join(pollErr, readErr)))
			return
		}
		if b[0] == 0x14 {
			c.host.remapEvents.Add(1)
			continue
		} // REMAP must never wait for the fault worker.
		if b[0] != 0x12 {
			c.fail(fmt.Errorf("unexpected UFFD event %#x", b[0]))
			return
		}
		flags := binary.LittleEndian.Uint64(b[8:16])
		// The kernel reports the faulting host page. Its pager page is counted
		// from the region's base, which need only be host-page aligned.
		address := binary.LittleEndian.Uint64(b[16:24])
		if address < c.region.Address || address-c.region.Address >= c.region.Length || flags&^uint64(7) != 0 {
			c.fail(errors.New("invalid UFFD page fault"))
			return
		}
		page := (address - c.region.Address) / c.mapping.pageSize
		c.queueMu.Lock()
		entry, exists := c.queue[page]
		if !exists {
			if len(c.queue) >= c.cfg.QueuePages {
				c.queueMu.Unlock()
				c.fail(ErrCapacity)
				return
			}
			// A page that faults again while queued keeps the first reading, so
			// the delay is measured against the access that has waited longest.
			entry.at = c.host.clock.Now()
		}
		entry.write = entry.write || flags&3 != 0
		c.queue[page] = entry
		c.queueMu.Unlock()
		c.wake()
	}
}

// wake lets one idle fault worker take queued work.
func (c *Connection) wake() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// takeFault claims a queued fault that no worker is serving. A page that faults
// again while in flight stays queued and is served after, which upgrades a read
// fault that became a write.
func (c *Connection) takeFault() (page uint64, entry queuedFault, ok bool) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	for p, e := range c.queue {
		if _, busy := c.inflight[p]; busy {
			continue
		}
		delete(c.queue, p)
		c.inflight[p] = struct{}{}
		return p, e, true
	}
	return 0, queuedFault{}, false
}

// deferFault holds a fault the client refused a mapping command for until the
// pager has made some progress, then queues it to be served again. It reports
// whether the session is still live. The page is neither queued nor in flight
// while this waits, so no worker retries it meanwhile: the budget it ran out of
// is freed by revocation, and nothing about serving the same page again before
// one has happened can free it. A page that faults again in the meantime is
// merged with this entry, so the wait for the queue is measured from the access
// that has waited longest.
func (c *Connection) deferFault(page uint64, entry queuedFault) bool {
	changed := c.host.changes()
	c.host.mu.Lock()
	c.host.stats.RefusedMappings++
	c.host.mu.Unlock()
	select {
	case <-c.ctx.Done():
		return false
	case <-changed:
	}
	c.queueMu.Lock()
	if existing, ok := c.queue[page]; ok {
		entry.write = entry.write || existing.write
		if existing.at.Before(entry.at) {
			entry.at = existing.at
		}
	}
	c.queue[page] = entry
	c.queueMu.Unlock()
	c.wake()
	return true
}

func (c *Connection) finishFault(page uint64) {
	c.queueMu.Lock()
	delete(c.inflight, page)
	_, again := c.queue[page]
	c.queueMu.Unlock()
	if again {
		c.wake()
	}
}

// work serves the region's seal requests.
func (c *Connection) work() {
	defer c.workers.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case request := <-c.requests:
			ctx, cancel := context.WithTimeout(c.ctx, c.cfg.CommandTimeout)
			err := c.region.Memory.Seal(ctx)
			cancel()
			response := vmwire.Frame{Kind: vmwire.Result, ID: request.ID}
			if err != nil {
				response.Flags = uint64(syscall.EIO)
			}
			if err := c.send(c.ctx, response); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// serveFaults is one of the region's fault workers. Workers drain the queue
// concurrently; the pager serializes only faults within one read-ahead window.
func (c *Connection) serveFaults() {
	defer c.workers.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.notify:
		}
		for {
			page, entry, ok := c.takeFault()
			if !ok {
				break
			}
			// The delay between reading the event and starting on it is queueing
			// and scheduling only, so it is recorded apart from the service time.
			c.host.faultQueueLatency.Observe(c.host.clock.Since(entry.at))
			// A fault is served for as long as the session lives. It is not a
			// command and it has no deadline of its own: the round trips inside
			// it are each bounded by the command timeout, and the one thing
			// that makes a fault long is the host's dirty budget, which stalls
			// a guest deliberately. Failing that wait on a deadline is the
			// killed VMM the budget exists to avoid.
			err := c.region.Memory.Fault(c.ctx, page, entry.write)
			c.finishFault(page)
			if errors.Is(err, ErrMappingRefused) {
				// The client refused a command for want of mapping budget and
				// changed nothing, so this fault is one to serve again rather
				// than a session to end. What frees that budget is revocation,
				// which is other work of this pager's: the fault is queued
				// again once something has moved, which is what the host's
				// progress signal reports. Until then the guest waits, as it
				// does for the dirty budget.
				if !c.deferFault(page, entry) {
					return
				}
				continue
			}
			if err != nil {
				c.fail(fmt.Errorf("page %d fault (write=%t): %w", page, entry.write, err))
				return
			}
		}
	}
}

// One control reader demultiplexes mapping ACKs and seal requests. It never
// waits for a seal, which can itself need mapping revocation ACKs.
func (c *Connection) readControl() {
	defer c.workers.Done()
	var lastRequest uint64
	for {
		f, err := vmwire.Read(c.socket)
		if err != nil {
			c.fail(err)
			return
		}
		switch f.Kind {
		case vmwire.Ack:
			select {
			case c.acks <- f:
			default:
				c.fail(errors.New("unexpected mapping ACK"))
				return
			}
		case vmwire.Seal:
			if f.ID == 0 || f.ID <= lastRequest || f.Offset != 0 || f.Length != 0 || f.Backing != 0 || f.Generation != 0 || f.Flags != 0 {
				c.fail(errors.New("invalid seal request"))
				return
			}
			lastRequest = f.ID
			select {
			case c.requests <- f:
			default:
				c.fail(ErrCapacity)
				return
			}
		default:
			c.fail(errors.New("unexpected managed-memory control message"))
			return
		}
	}
}
func (c *Connection) send(ctx context.Context, f vmwire.Frame) error {
	return c.sendFrames(ctx, []vmwire.Frame{f})
}

func (c *Connection) sendFrames(ctx context.Context, frames []vmwire.Frame) error {
	if err := c.writeMu.Lock(ctx); err != nil {
		return err
	}
	defer c.writeMu.Unlock()
	if err := context.Cause(c.ctx); err != nil {
		return err
	}
	// As above, the write deadline is the kernel's and stays on the wall clock.
	deadline := time.Now().Add(c.cfg.CommandTimeout) // wall clock: kernel socket deadline
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.socket.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer c.socket.SetWriteDeadline(time.Time{})
	var data []byte
	for _, f := range frames {
		data = append(data, f.Bytes()...)
	}
	return vmwire.WriteBytes(c.socket, data)
}

func (c *Connection) verify() {
	defer c.workers.Done()
	timer := c.host.clock.NewTicker(c.cfg.VerifyInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C():
		}
		ctx, cancel := context.WithTimeout(c.ctx, c.cfg.CommandTimeout)
		err := c.region.Memory.Verify(ctx)
		cancel()
		if err != nil {
			c.fail(err)
			return
		}
	}
}

// Stop asks the Rust owner to release its Session. Call only after all its
// users have stopped and KVM slots have been unregistered. Close follows once
// the owner has exited or confirmed that Session is gone.
func (c *Connection) Stop(ctx context.Context) error {
	return c.command(ctx, vmwire.Frame{Kind: vmwire.Stop})
}

// Close must follow process exit or an equivalent complete memory-user stop.
// Unlike cancellation, that proof permits backing reuse after ambiguous ACKs.
func (c *Connection) Close(ctx context.Context) error {
	if err := c.closeMu.Lock(ctx); err != nil {
		return err
	}
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.fail(ErrClosed)
	c.workers.Wait()
	if err := c.region.Memory.Detach(ctx); err != nil {
		return err
	}
	c.closed = true
	return c.uffd.Close()
}
