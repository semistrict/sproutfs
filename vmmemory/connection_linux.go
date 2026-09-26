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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/vmwire"
	"github.com/semistrict/sproutfs/vmmemory/internal/pageranges"
)

type ConnectionConfig struct {
	// Name identifies this session's memory region in what it logs: the volume the
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
type ConnectedMemoryRegion struct {
	Kind            MemoryRegionKind
	Address, Length uint64
	Memory          *MemoryRegion
}

// Connection serves one Rust Session, which maps exactly one memory region. Wait
// reports terminal failure; its owner must stop the process before Close
// releases possibly mapped slots. Neither a transport disconnect nor
// cancellation proves that memory users have stopped.
type Connection struct {
	host         *Host
	socket       *net.UnixConn
	uffd         *os.File
	memoryRegion ConnectedMemoryRegion
	mapping      *remoteMapping
	cfg          ConnectionConfig
	ctx          context.Context
	cancel       context.CancelCauseFunc
	// reported admits the one failure this session is logged as ending on.
	reported  sync.Once
	commandMu *ctxsync.Mutex
	writeMu   *ctxsync.Mutex
	// awaiting is the ID of the command waiting for its acknowledgement, and
	// zero while none is. The control reader takes an ACK only for that
	// command, and only once, so acks never holds more than its answer.
	awaiting atomic.Uint64
	acks     chan vmwire.Frame
	requests chan vmwire.Frame
	// flushes holds the guest's flush requests the host has not yet been
	// handed, in the order they came. See maxQueuedFlushes.
	flushes  chan vmwire.Frame
	sequence uint64
	queueMu  sync.Mutex
	queue    map[uint64]queuedFault
	inflight map[uint64]struct{}
	notify   chan struct{}
	workers  sync.WaitGroup
	closeMu  *ctxsync.Mutex
	closed   bool
	// attachNS is what building this session cost, from the descriptor exchange
	// to the acknowledged READY that ends the mandatory populate. It is written
	// once, by Connect, before anything else can read it.
	attachNS int64
}

// AttachStats is what one session's attachment cost, which is the part of a
// restore the pager owns: the whole of Connect, and the populate inside it.
// DurationNS is the attach — the descriptor exchange, the memory region admission, the
// ATTACH, the populate and the READY round trip — so what it holds beyond the
// populate is handshake and metadata and nothing else.
type AttachStats struct {
	DurationNS int64
	Populate   PopulateStats
}

// Attach reports what this session's attachment cost.
func (c *Connection) Attach() AttachStats {
	stats := AttachStats{DurationNS: c.attachNS}
	if c.memoryRegion.Memory != nil {
		stats.Populate = c.memoryRegion.Memory.Populated()
	}
	return stats
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
// memory region against trusted configuration. The caller must authenticate the peer
// and authorize its volume identity before calling. No identity supplied by the
// Rust process selects a volume.
// An error may return a retained Connection once mappings can be installed;
// stop the client process before closing that connection and releasing aliases.
func Connect(ctx context.Context, h *Host, socket *net.UnixConn, backing MemoryRegionBacking, cfg ConnectionConfig) (*Connection, error) {
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
	a, ok := h.files[0].ArenaFile.(*LinuxFile)
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
	// What the attach costs is the pager's half of a restore, so it is timed from
	// here: everything below is this session being built, and the guest cannot run
	// until the READY at the end of it is acknowledged.
	attaching := h.clock.Now()
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
	c := &Connection{host: h, socket: socket, uffd: fd, cfg: cfg, ctx: sessionCtx, cancel: cancel, commandMu: ctxsync.NewMutex(), writeMu: ctxsync.NewMutex(), acks: make(chan vmwire.Frame, 1), requests: make(chan vmwire.Frame, 1), flushes: make(chan vmwire.Frame, maxQueuedFlushes), closeMu: ctxsync.NewMutex(), queue: make(map[uint64]queuedFault), inflight: make(map[uint64]struct{}), notify: make(chan struct{}, cfg.FaultWorkers)}
	attached := false
	fail := func(err error) (*Connection, error) {
		err = errors.Join(err, context.Cause(ctx))
		// The client sees only a socket that closed before its backing came,
		// and reports that to whatever request built the session. This is where
		// the reason is, so it is written down here as well as returned.
		slog.ErrorContext(ctx, "vmmemory: a memory session never attached",
			"memory_region", cfg.Name, "kind", backing.Kind, "error", err)
		cancel(err)
		_ = socket.Close()
		if attached {
			c.workers.Wait()
			return c, err
		}
		_ = fd.Close()
		if c.memoryRegion.Memory != nil {
			if detachErr := c.memoryRegion.Memory.Detach(context.Background()); detachErr != nil {
				err = errors.Join(err, detachErr)
			}
		}
		return nil, err
	}
	// The hello carries nothing but the version: one session is one memory region, and
	// what page that memory region runs is the attachment's to say. A version 6 peer
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
	if f.Kind != vmwire.MemoryRegion || f.Flags != uint64(backing.Kind) || f.Length != backing.Backing.Size() || f.Length == 0 || f.Length%h.pageSize != 0 || f.Offset%h.pageSize != 0 || f.Offset > ^uint64(0)-f.Length || f.ID != 0 || f.Backing != 0 || f.Generation != 0 {
		return fail(errors.New("invalid managed-memory-region"))
	}
	// Admission bounds logical capacity; untouched generations are implicit.
	m := &remoteMapping{connection: c, address: f.Offset, pageCount: f.Length / h.pageSize, pageSize: h.pageSize, mu: ctxsync.NewRWMutex()}
	r, err := h.admit(ctx, backing, m)
	if err != nil {
		return fail(err)
	}
	c.memoryRegion = ConnectedMemoryRegion{backing.Kind, f.Offset, f.Length, r}
	c.mapping = m
	// The attachment states the geometry: this memory region's page, what its
	// files are made of, and the mapping-count budget. The files follow it,
	// each with its descriptor. The arena is the one file, file 0, and the
	// client receives it read-write. It is a sparse file, so its length is its
	// addresses and not the memory behind them. The client refuses a page it
	// does not map, a descriptor shorter than the length stated or whose
	// filesystem is not the memory that page is, or a memory region of its own
	// that is not whole pages of it — all before it exposes an address to the
	// VMM.
	if err := vmwire.Write(socket, vmwire.AttachFrame(h.pageSize, a.backing, uint64(cfg.MaxVMAs))); err != nil {
		return fail(err)
	}
	if err := r.giveFiles(ctx); err != nil {
		return fail(err)
	}
	attached = true
	if err := socket.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	context.AfterFunc(sessionCtx, func() { _ = socket.Close() })
	c.workers.Add(5 + cfg.FaultWorkers)
	go c.work()
	go c.deliverFlushes()
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
	c.attachNS = h.clock.Since(attaching).Nanoseconds()
	return c, nil
}

// Populate maps matching resident identities without loading absent pages.
// Connect calls it while Rust serves commands before exposing addresses to the
// VMM. An explicit later call requires quiescent guest memory.
func (c *Connection) Populate(ctx context.Context) error {
	if err := c.memoryRegion.Memory.Populate(ctx); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

// MemoryRegion is the one memory region this session maps.
func (c *Connection) MemoryRegion() ConnectedMemoryRegion { return c.memoryRegion }
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
// is where the error that ended a VM's memory is written down. Which memory region it
// was is this session's name, and which page it was is in the error the caller
// built. Only the first failure is logged: everything the session does after it
// fails with the cancellation this installs, and those are consequences.
func (c *Connection) fail(err error) {
	c.reported.Do(func() {
		attrs := []any{"memory_region", c.cfg.Name, "address", c.memoryRegion.Address}
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
// its kind, its identifier, the memory region offset and length it covers, the arena
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
	// The command is awaited before it is sent, because its answer can arrive
	// before the send returns.
	c.awaiting.Store(f.ID)
	if err := c.sendFrames(ctx, append([]vmwire.Frame{f}, runs...)); err != nil {
		c.giveUp(f.ID)
		return failed(err)
	}
	var response vmwire.Frame
	timer := c.host.clock.NewTimer(c.cfg.CommandTimeout)
	defer timer.Stop()
	select {
	case response = <-c.acks:
	case <-ctx.Done():
		c.giveUp(f.ID)
		return failed(context.Cause(ctx))
	case <-c.ctx.Done():
		// The reader publishes a final ACK before recording a following EOF.
		// Prefer that evidence when the peer closes immediately after STOP. If
		// the reader has not taken this command's ACK, no reader will.
		if c.awaiting.CompareAndSwap(f.ID, 0) {
			return failed(context.Cause(c.ctx))
		}
		response = <-c.acks
	case <-timer.C():
		c.giveUp(f.ID)
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

// giveUp stops awaiting a command whose answer its caller no longer waits for.
// If the reader has already taken that answer, it is in acks or about to be,
// and it is taken out so that the next command does not read it as its own.
func (c *Connection) giveUp(id uint64) {
	if !c.awaiting.CompareAndSwap(id, 0) {
		<-c.acks
	}
}

// frames appends one frame of the given kind per span of the run whose pages
// share a generation, since the protocol advances every page of a frame from
// the same one. A MAP names the file of the run's slots, and only the private
// file may be mapped writable: the client refuses anything else. Caller holds
// the mapping lock.
func (m *remoteMapping) frames(frames []vmwire.Frame, kind uint64, run MapRun, writable bool) ([]vmwire.Frame, error) {
	if run.Count < 1 || run.Page >= m.pageCount || uint64(run.Count) > m.pageCount-run.Page {
		return nil, ErrRange
	}
	if kind == vmwire.MapRange && (run.File < 0 || (writable && run.File != vmwire.PrivateFile)) {
		return nil, fmt.Errorf("%w: a map of file %d writable=%t, and only file %d is writable",
			ErrRange, run.File, writable, vmwire.PrivateFile)
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
			f.Flags = vmwire.MapFlags(uint64(run.File), !writable)
		case vmwire.MapZero:
			f.Flags = vmwire.Immutable
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
func (m *remoteMapping) change(ctx context.Context, run MapRun, writable bool, kind uint64) error {
	if err := m.mu.Lock(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	frames, err := m.frames(nil, kind, run, writable)
	if err != nil {
		return err
	}
	_, _, err = m.commit(ctx, frames)
	return err
}

// GiveFile sends the client one file with its descriptor. The private file's
// descriptor is the pager's own, read-write; every other file goes as a
// read-only open of it, which is what keeps a VMM from writing a page another
// memory region may map. It is written under the lock commands are written
// under, so it reaches the client before any map that names it.
func (m *remoteMapping) GiveFile(ctx context.Context, number int, file ArenaFile, writable bool) error {
	f, ok := file.(*LinuxFile)
	if !ok || number < 0 || writable != (number == vmwire.PrivateFile) {
		return fmt.Errorf("%w: file %d writable=%t of %T", ErrConfig, number, writable, file)
	}
	descriptor := f.file
	if !writable {
		var err error
		if descriptor, err = f.readOnly(); err != nil {
			return err
		}
	}
	frame := vmwire.FileFrame(uint64(number), uint64(f.offsets)*uint64(f.pageSize), f.backing, writable)
	return m.connection.write(ctx, func() error { return vmwire.SendFD(m.connection.socket, frame, descriptor) })
}

// DropFile tells the client to close one file's descriptor.
func (m *remoteMapping) DropFile(ctx context.Context, number int) error {
	return m.connection.send(ctx, vmwire.DropFileFrame(uint64(number)))
}

func (m *remoteMapping) Map(ctx context.Context, page uint64, file, slot, count int, writable bool) error {
	return m.change(ctx, MapRun{Page: page, File: file, Slot: slot, Count: count}, writable, vmwire.MapRange)
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
	return m.change(ctx, MapRun{Page: page, Count: 1}, false, vmwire.Revoke)
}

// Protect write-protects a whole run in place with one UFFD ioctl. It sends no
// mapping command and advances no generation: the client's mappings are not
// touched, so nothing about them has to be acknowledged, and the run may cover
// as many of them as it likes. Like the ioctls Resolve issues, it runs under no
// mapping lock; the memory region's exclusive lock is what keeps this range's mappings
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
		// from the memory region's base, which need only be host-page aligned.
		address := binary.LittleEndian.Uint64(b[16:24])
		if address < c.memoryRegion.Address || address-c.memoryRegion.Address >= c.memoryRegion.Length || flags&^uint64(7) != 0 {
			c.fail(errors.New("invalid UFFD page fault"))
			return
		}
		page := (address - c.memoryRegion.Address) / c.mapping.pageSize
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
// pager has revoked a mapping, then queues it to be served again. It reports
// whether the session is still live. The page is neither queued nor in flight
// while this waits, so no worker retries it meanwhile: the budget it ran out of
// is freed by revocation, and nothing about serving the same page again before
// one has happened can free it. It waits for a revocation and not for any
// change, because the refused fault took and gave back pages itself: two
// refused faults would each wake the other, and a client that refuses every
// command would keep the pager serving it for as long as it lives. A page that
// faults again in the meantime is merged with this entry, so the wait for the
// queue is measured from the access that has waited longest.
func (c *Connection) deferFault(page uint64, entry queuedFault) bool {
	revoked := c.host.revocations()
	c.host.mu.Lock()
	c.host.stats.RefusedMappings++
	c.host.mu.Unlock()
	select {
	case <-c.ctx.Done():
		return false
	case <-revoked:
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

// work serves the memory region's seal requests.
func (c *Connection) work() {
	defer c.workers.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case request := <-c.requests:
			ctx, cancel := context.WithTimeout(c.ctx, c.cfg.CommandTimeout)
			err := c.memoryRegion.Memory.Seal(ctx)
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

// maxQueuedFlushes bounds the flush requests a session holds before the host
// has been handed them. A guest has no more flushes outstanding than its
// virtio-pmem queue has entries, and its device asks once for each queue drain,
// so a client past this is one this pager does not understand.
const maxQueuedFlushes = 1024

// deliverFlushes hands the memory region's guest flushes to the host's callback, one
// at a time and off the reader, so a callback that takes its time holds up only
// the flushes after it and never the session.
func (c *Connection) deliverFlushes() {
	defer c.workers.Done()
	for {
		var request vmwire.Frame
		select {
		case <-c.ctx.Done():
			return
		case request = <-c.flushes:
		}
		c.memoryRegion.Memory.deliverFlush(c.answerFlush(request.ID))
	}
}

// answerFlush is the done a flush request is handed with: it sends the RESULT
// that completes the guest's flush, once. A session that has ended has nobody
// to answer, and its client has already failed the flushes it was waiting on.
func (c *Connection) answerFlush(id uint64) func(error) {
	var answered atomic.Bool
	return func(err error) {
		if answered.Swap(true) {
			slog.Error("vmmemory: a flush was answered twice; the second answer is ignored",
				"memory_region", c.cfg.Name, "request", id, "error", err)
			return
		}
		response := vmwire.Frame{Kind: vmwire.Result, ID: id}
		if err != nil {
			slog.Warn("vmmemory: a guest's flush failed", "memory_region", c.cfg.Name, "request", id, "error", err)
			response.Flags = uint64(syscall.EIO)
		}
		if sendErr := c.send(c.ctx, response); sendErr != nil {
			if context.Cause(c.ctx) != nil {
				slog.Info("vmmemory: a flush was answered after its session ended",
					"memory_region", c.cfg.Name, "request", id, "error", sendErr)
				return
			}
			c.fail(fmt.Errorf("answering flush %d: %w", id, sendErr))
		}
	}
}

// serveFaults is one of the memory region's fault workers. Workers drain the queue
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
			err := c.memoryRegion.Memory.Fault(c.ctx, page, entry.write)
			c.finishFault(page)
			if errors.Is(err, ErrMappingRefused) {
				// The client refused a command for want of mapping budget and
				// changed nothing, so this fault is one to serve again rather
				// than a session to end. What frees that budget is revocation,
				// which is other work of this pager's: the fault is queued
				// again once a revocation has landed. Until then the guest
				// waits, as it does for the dirty budget.
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

// One control reader demultiplexes mapping ACKs, seal requests and flush
// requests, which share one sequence of IDs. It never waits for a seal, which
// can itself need mapping revocation ACKs, nor for the host to answer a flush,
// whose checkpoint needs the same.
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
			// An ACK answers the one command awaiting it, once. Any other is a
			// client answering what it was not asked, or answering twice. Read
			// as the next command's answer, it would acknowledge that command
			// before it was sent.
			if f.ID == 0 || !c.awaiting.CompareAndSwap(f.ID, 0) {
				c.fail(fmt.Errorf("unexpected mapping ACK of command %d", f.ID))
				return
			}
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
		case vmwire.Flush:
			if f.ID == 0 || f.ID <= lastRequest || f.Offset != 0 || f.Length != 0 || f.Backing != 0 || f.Generation != 0 || f.Flags != 0 {
				c.fail(errors.New("invalid flush request"))
				return
			}
			// A flush is a disk's. RAM is not made durable by a disk checkpoint,
			// so a client that flushes it is one this pager does not understand.
			if c.memoryRegion.Kind != Pmem {
				c.fail(fmt.Errorf("a flush of a %s memory region", c.memoryRegion.Kind))
				return
			}
			lastRequest = f.ID
			c.host.countFlush()
			select {
			case c.flushes <- f:
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
	var data []byte
	for _, f := range frames {
		data = append(data, f.Bytes()...)
	}
	return c.write(ctx, func() error { return vmwire.WriteBytes(c.socket, data) })
}

// write runs one write to the socket under the write lock and a deadline, so
// that what one command sends is never interleaved with another's and a client
// that stops reading fails the write rather than holding it.
func (c *Connection) write(ctx context.Context, send func() error) error {
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
	return send()
}

func (c *Connection) verify() {
	defer c.workers.Done()
	timer := c.host.clock.NewTicker(c.cfg.VerifyInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.memoryRegion.Memory.ended:
			// Another memory region's fault found this one's VMM writing a page
			// it holds read-only.
			c.fail(c.memoryRegion.Memory.serving())
			return
		case <-timer.C():
		}
		ctx, cancel := context.WithTimeout(c.ctx, c.cfg.CommandTimeout)
		err := c.memoryRegion.Memory.Verify(ctx)
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
	if err := c.memoryRegion.Memory.Detach(ctx); err != nil {
		return err
	}
	c.closed = true
	return c.uffd.Close()
}
