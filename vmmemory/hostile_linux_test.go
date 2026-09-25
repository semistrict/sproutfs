//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A VMM runs untrusted guest code, and an embedder jails it because the VMM
// itself may be compromised. A compromised VMM can speak its memory session's
// protocol directly. The tests in this file play such a VMM against a real
// pager, beside a well-behaved client process on the same pager, and after
// every hostile session they hold the pager to these rules:
//
//   - The hostile session ends with an error. It never hangs.
//   - The well-behaved process reads exactly the bytes it wrote.
//   - Its stores complete within a bound while the hostile session runs.
//   - Once the hostile session is closed, the pager's resident, dirty, logical
//     and idle pages and the memory its arena holds are what they were before,
//     and every resident page is mapped or idle.
//   - The host process holds the descriptors it held before.
//
// A panic anywhere in the host process fails the test by itself.
//
// The protocol is not all a VMM is given. The attachment hands it the arena's
// descriptor, and a VMM can write through that to any page of the arena. These
// tests do not cover that. See TASK-2 in backlog/tasks.

const (
	// hostilePage is the page every memory region here runs: RAM's, over an
	// ordinary arena, which needs no HugeTLB pool.
	hostilePage = checkpoint.PageSize4KiB
	// hostilePages is the size of every memory region here, in pages.
	hostilePages = 16
	// hostileQueue bounds the hostile session's pending faults below its
	// pages, so that a flood of faults on distinct pages can overflow it.
	hostileQueue = 4
	// hostileTimeout is the hostile session's command timeout: how long the
	// pager waits for any one answer from it.
	hostileTimeout = 250 * time.Millisecond
	// hostileBound is how long any end may take: the hostile session's, its
	// close, and a round of the other process's work. Each takes a small
	// fraction of it, so passing it is a hang.
	hostileBound = 5 * time.Second
	// neighbourStore bounds one store of the well-behaved process while the
	// hostile session runs. The most a hostile session can hold up is a page
	// both of them map, for as long as the pager waits for the hostile
	// session's answer, which is hostileTimeout.
	neighbourStore = 2 * time.Second
	// hostileBase is the address a hostile VMM says its memory region is at.
	hostileBase = 1 << 30
	// maxHostileOps bounds a script, so every fuzz input is a short session.
	maxHostileOps = 32
)

// hostileConnection is the configuration the pager serves a hostile session
// with. Two fault workers are enough for two faults to be served at once.
var hostileConnection = vmmemory.ConnectionConfig{Name: "hostile", QueuePages: hostileQueue,
	FaultWorkers: 2, CommandTimeout: hostileTimeout, VerifyInterval: time.Hour}

// descriptor is what a hostile VMM sends where its HELLO carries a UFFD.
type descriptor uint8

const (
	// pipeDescriptor is the read end of a pipe. The VMM writes the other end,
	// so every fault event the pager reads is one the VMM made up.
	pipeDescriptor descriptor = iota
	// userfaultDescriptor is a real userfaultfd that never completed its API
	// handshake, so every read of it and every ioctl on it fails.
	userfaultDescriptor
	nullDescriptor
	// socketDescriptor is one end of a socket pair that never carries a byte.
	socketDescriptor
	noDescriptor
	twoDescriptors
	descriptorKinds
)

// frameEdit changes one field of a frame the hostile VMM sends. Field zero is
// no edit, so that a zero byte of fuzz input is the well-behaved frame; fields
// one to seven are kind, id, offset, length, backing, generation and flags.
type frameEdit struct {
	field uint8
	value uint64
}

func (e frameEdit) apply(f vmwire.Frame) vmwire.Frame {
	fields := []*uint64{&f.Kind, &f.ID, &f.Offset, &f.Length, &f.Backing, &f.Generation, &f.Flags}
	if e.field >= 1 && int(e.field) <= len(fields) {
		*fields[e.field-1] = e.value
	}
	return f
}

// answer is how a hostile VMM answers one command of the pager's.
type answer uint8

const (
	acknowledge answer = iota
	// refuse answers ENOSPC, which is how a client out of mapping budget
	// refuses a command it has not applied.
	refuse
	acknowledgeAnotherID
	acknowledgeAnotherGeneration
	acknowledgeTwice
	// acknowledgeWithFields sets a field an acknowledgement leaves zero.
	acknowledgeWithFields
	// stayQuiet never answers.
	stayQuiet
	// hangUp closes the socket instead of answering.
	hangUp
	answerKinds
)

// opKind is one thing a hostile VMM does after its handshake.
type opKind uint8

const (
	// opFrame sends a raw control frame: kind a&0xff, offset a>>8, id b.
	opFrame opKind = iota
	// opSeal sends a seal request with the next request id, or with id a
	// when a is not zero.
	opSeal
	// opFlush sends a flush request in the same way.
	opFlush
	// opFault forges a fault event: page a modulo two past the memory region's
	// end, byte (b>>8) modulo the page into it, with flags b&0xff.
	opFault
	// opEvent forges an event of type a&0xff whose address field is b.
	opEvent
	// opShortEvent writes the first a%31+1 bytes of a fault event.
	opShortEvent
	// opFaults forges a%64+1 fault events on consecutive pages, wrapping at
	// the memory region's end, each with flags b&0xff.
	opFaults
	// opFlushes sends a%2048+1 flush requests.
	opFlushes
	// opSeals sends a%16+1 seal requests.
	opSeals
	// opPartial writes the first a%55+1 bytes of a seal request.
	opPartial
	// opGarbage writes a%512+1 bytes, each the low byte of b.
	opGarbage
	// opRights sends a seal request carrying a%4+1 descriptors.
	opRights
	// opAwait waits for the next frame from the pager, or for it to hang up,
	// for at most hostileTimeout.
	opAwait
	// opCloseEvents closes the end of the event pipe the VMM writes.
	opCloseEvents
	// opCloseWrite shuts the VMM's side of the control socket for writing.
	opCloseWrite
	// opLinger keeps the session open for hostileTimeout whatever the pager
	// does, so that what it sends in that time can be counted.
	opLinger
	opKinds
)

type hostileOp struct {
	kind opKind
	a, b uint64
}

// hostileScript is one hostile VMM's whole session.
type hostileScript struct {
	// pmem makes the hostile memory region PMEM, which may flush. It is RAM
	// otherwise.
	pmem bool
	// inherits gives the hostile memory region the page identities of the
	// well-behaved process's RAM, so the two map the same resident pages.
	inherits   bool
	descriptor descriptor
	// hello and region edit the HELLO and MEMORY_REGION frames.
	hello, region frameEdit
	// answers are the answers to the pager's commands in order. The last one
	// answers every command after it, and an empty list acknowledges them all.
	answers []answer
	ops     []hostileOp
	// awaitEnd says the pager ends this session on its own, and waits for it
	// to after the ops. A script without it hangs up at once.
	awaitEnd bool
}

func (s hostileScript) kind() vmmemory.MemoryRegionKind {
	if s.pmem {
		return vmmemory.Pmem
	}
	return vmmemory.Ram
}

func (s hostileScript) answer(command int) answer {
	if len(s.answers) == 0 {
		return acknowledge
	}
	return s.answers[min(command, len(s.answers)-1)]
}

// encode is the fuzz input decodeHostile reads back as this script. A fuzz
// input cannot say the pager ends its session on its own, so a script that
// waits for that becomes one that lingers before it hangs up.
func (s hostileScript) encode() []byte {
	var flags byte
	if s.pmem {
		flags |= 1
	}
	if s.inherits {
		flags |= 2
	}
	b := []byte{flags, byte(s.descriptor), s.hello.field}
	b = binary.LittleEndian.AppendUint64(b, s.hello.value)
	b = append(b, s.region.field)
	b = binary.LittleEndian.AppendUint64(b, s.region.value)
	b = append(b, byte(len(s.answers)))
	for _, a := range s.answers {
		b = append(b, byte(a))
	}
	ops := s.ops
	if s.awaitEnd {
		ops = append(slices.Clip(ops), hostileOp{kind: opLinger})
	}
	for _, op := range ops {
		b = append(b, byte(op.kind))
		b = binary.LittleEndian.AppendUint64(b, op.a)
		b = binary.LittleEndian.AppendUint64(b, op.b)
	}
	return b
}

// decodeHostile reads a script out of fuzz input. Every input is a script:
// what the bytes do not say takes its well-behaved value, so an empty input is
// a VMM that attaches properly and hangs up.
func decodeHostile(input []byte) hostileScript {
	r := fuzzReader{input}
	flags := r.byte()
	s := hostileScript{pmem: flags&1 != 0, inherits: flags&2 != 0}
	s.descriptor = descriptor(r.byte() % byte(descriptorKinds))
	s.hello = frameEdit{r.byte(), r.word()}
	s.region = frameEdit{r.byte(), r.word()}
	for range r.byte() % 8 {
		s.answers = append(s.answers, answer(r.byte()%byte(answerKinds)))
	}
	for len(s.ops) < maxHostileOps && len(r.rest) > 0 {
		s.ops = append(s.ops, hostileOp{opKind(r.byte() % byte(opKinds)), r.word(), r.word()})
	}
	return s
}

// fuzzReader reads fuzz input, and zeros once it runs out.
type fuzzReader struct{ rest []byte }

func (r *fuzzReader) byte() byte {
	if len(r.rest) == 0 {
		return 0
	}
	b := r.rest[0]
	r.rest = r.rest[1:]
	return b
}

func (r *fuzzReader) word() uint64 {
	var b [8]byte
	n := copy(b[:], r.rest)
	r.rest = r.rest[n:]
	return binary.LittleEndian.Uint64(b[:])
}

// hostileFixture is one pager with a well-behaved client process on it, whose
// PMEM and RAM each have hostilePages pages. Hostile sessions come and go
// beside it.
type hostileFixture struct {
	h     *vmmemory.Host
	arena *vmmemory.LinuxArena
	good  *nativeProcess
	// backings are the volumes of the process's PMEM and RAM.
	backings [2]*kernelBacking
	// values are what each page of each of its memory regions holds.
	values [2][hostilePages]byte
	stores int
	// previous is the hostile session the last round played, which is where
	// something one round finds wrong may have started.
	previous hostileScript
	// before is the pager's account and the host's descriptors with only the
	// well-behaved process on it.
	before      pagerAccount
	descriptors int
}

// pagerAccount is what a pager holds, as the rules above compare it.
type pagerAccount struct {
	resident, dirty, logical, idle int
	allocated                      uint64
	// unreachable counts the pages nothing maps that are not idle either,
	// which nothing would ever give back.
	unreachable int
}

func newHostileFixture(t testing.TB) *hostileFixture {
	t.Helper()
	cfg := vmmemory.Config{PageSize: hostilePage, ResidentPages: 4 * hostilePages,
		LogicalPages: 4 * hostilePages, DirtyPages: 4 * hostilePages, ReadAheadPages: 4, WriteAheadPages: 1}
	h, arena := kernelHostArena(t, cfg)
	fx := &hostileFixture{h: h, arena: arena}
	var provided []vmmemory.Backing
	for region := range fx.backings {
		fx.backings[region] = fx.volume(region)
		provided = append(provided, fx.backings[region])
		for page := range hostilePages {
			fx.values[region][page] = pageByte(region, page)
		}
	}
	fx.good = startNativeWithConfig(t, h, hostilePages, vmmemory.ConnectionConfig{Name: "neighbour",
		QueuePages: hostilePages, CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour}, provided...)
	// Every page of both memory regions is faulted in, so the process holds
	// its whole working set before any hostile session starts.
	if err := fx.check(); err != nil {
		t.Fatal(err)
	}
	var err error
	if fx.before, err = fx.account(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fx.descriptors, err = openDescriptors(); err != nil {
		t.Fatal(err)
	}
	return fx
}

// volume is the well-behaved process's volume of one memory region as it was
// first attached: every page its own bytes, under one checkpoint's identities.
func (fx *hostileFixture) volume(region int) *kernelBacking {
	b := newPagedKernelBacking(byte(region+1), hostilePages*hostilePage, hostilePage)
	for i := range b.data {
		b.data[i] = pageByte(region, i/hostilePage)
	}
	return b
}

// hostileVolume is the volume a hostile session attaches: a fork of the
// well-behaved process's RAM as it was first attached, or a volume of its own.
func (fx *hostileFixture) hostileVolume(inherits bool) *kernelBacking {
	if inherits {
		return fx.volume(1)
	}
	b := newPagedKernelBacking(9, hostilePages*hostilePage, hostilePage)
	for i := range b.data {
		b.data[i] = byte(200 + i/hostilePage)
	}
	return b
}

// check reads every byte of both memory regions of the well-behaved process
// and reports the first page that does not hold what it wrote there.
func (fx *hostileFixture) check() error {
	for region := range fx.values {
		for page, value := range fx.values[region] {
			command := fmt.Sprintf("stridescan %d %d %d 1 %d", region, page*hostilePage, hostilePage, value)
			if err := fx.good.ask(command, "strided"); err != nil {
				return fmt.Errorf("page %d of memory region %d does not hold %d: %w", page, region, value, err)
			}
		}
	}
	return nil
}

// store is one round of the well-behaved process's work: it stores into one
// page of its RAM, which faults, and then takes a checkpoint of that RAM, so
// its next store into the page faults again. The store must complete within
// neighbourStore.
func (fx *hostileFixture) store(ctx context.Context, page int, value byte) error {
	started := time.Now()
	if err := fx.good.ask(fmt.Sprintf("fill 1 %d %d %d", page*hostilePage, hostilePage, value), "filled"); err != nil {
		return err
	}
	if took := time.Since(started); took > neighbourStore {
		return fmt.Errorf("a store of the well-behaved process took %s, want at most %s", took, neighbourStore)
	}
	if err := fx.good.ask("seal 1", "sealed"); err != nil {
		return err
	}
	r := fx.good.memoryRegion(1)
	published, err := fx.backings[1].publish(ctx, r.Checkpoint())
	return errors.Join(err, r.Checkpoint().Retire(ctx, published))
}

// account is what the pager holds once it has given up its idle pages. A
// hostile session's pages that no one else maps go idle when it detaches, and
// dropping them is how a host takes its memory back.
func (fx *hostileFixture) account(ctx context.Context) (pagerAccount, error) {
	if _, err := fx.h.DropIdle(ctx); err != nil {
		return pagerAccount{}, err
	}
	s, err := fx.h.Stats(ctx)
	if err != nil {
		return pagerAccount{}, err
	}
	allocated, err := fx.arena.AllocatedBytes()
	if err != nil {
		return pagerAccount{}, err
	}
	return pagerAccount{resident: s.ResidentPages, dirty: s.DirtyPages, logical: s.LogicalPages,
		idle: s.IdlePages, allocated: allocated, unreachable: len(fx.h.Unreachable())}, nil
}

// openDescriptors counts the descriptors this process holds.
func openDescriptors() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	return len(entries), err
}

// hostileEnd is how a hostile session ended.
type hostileEnd struct {
	// err is what the session ended with.
	err error
	// commands is how many commands the pager sent the hostile VMM.
	commands int
}

// round runs one hostile session while the well-behaved process stores into
// one of its pages, and then holds the pager to the rules at the top of this
// file.
func (fx *hostileFixture) round(t *testing.T, s hostileScript) hostileEnd {
	t.Helper()
	fx.stores++
	page := hostilePages/2 + fx.stores%(hostilePages/2)
	value := byte(100 + fx.stores%100)
	stored := make(chan error, 1)
	go func() { stored <- fx.store(t.Context(), page, value) }()
	end := fx.play(t, s)
	select {
	case err := <-stored:
		if err != nil {
			t.Fatalf("the well-behaved process beside the hostile session: %v", err)
		}
	case <-time.After(hostileBound):
		t.Fatalf("a store of the well-behaved process did not complete within %s", hostileBound)
	}
	fx.values[1][page] = value
	if err := fx.check(); err != nil {
		t.Fatalf("after a hostile session: %v", err)
	}
	after, err := fx.account(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after != fx.before {
		// What the well-behaved process holds says whose the difference is.
		// The fixture lives through many sessions, so the one before this one
		// is named too.
		var held []vmmemory.MemoryRegionStats
		for region := range fx.backings {
			s, err := fx.good.memoryRegion(region).Stats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, s)
		}
		t.Fatalf("after hostile session %d the pager holds %+v, want what it held before it, %+v; "+
			"the well-behaved process holds %+v; nothing reaches %q; this session was %+v and the one before it %+v",
			fx.stores, after, fx.before, held, fx.h.Unreachable(), s, fx.previous)
	}
	if descriptors, err := openDescriptors(); err != nil || descriptors != fx.descriptors {
		t.Fatalf("after a hostile session the host holds %d descriptors, want the %d it held before: %v",
			descriptors, fx.descriptors, err)
	}
	fx.previous = s
	return end
}

// play runs one hostile session to its end and closes it.
func (fx *hostileFixture) play(t *testing.T, s hostileScript) hostileEnd {
	t.Helper()
	ours, theirs := socketPair(t)
	defer ours.Close()
	type connected struct {
		c   *vmmemory.Connection
		err error
	}
	result := make(chan connected, 1)
	backing := vmmemory.MemoryRegionBacking{Kind: s.kind(), Backing: fx.hostileVolume(s.inherits)}
	go func() {
		c, err := vmmemory.Connect(t.Context(), fx.h, theirs, backing, hostileConnection)
		result <- connected{c, err}
	}()
	peer := &hostilePeer{conn: ours, script: s, arrived: make(chan struct{}, 1), ended: make(chan struct{})}
	if err := peer.attach(t, s); err == nil {
		go peer.respond()
		for _, op := range s.ops {
			if peer.play(op) != nil {
				break
			}
		}
	} else {
		close(peer.ended)
	}
	if s.awaitEnd {
		select {
		case <-peer.ended:
		case <-time.After(hostileBound):
			t.Errorf("the pager did not end the hostile session within %s", hostileBound)
		}
	}
	// The VMM hangs up. Its descriptors stay open until the session has
	// ended, so that the session ends on the control socket.
	_ = ours.Close()
	defer peer.closeDescriptors()
	<-peer.ended

	var r connected
	select {
	case r = <-result:
	case <-time.After(hostileBound):
		t.Fatalf("attaching the hostile session did not end within %s of its hanging up", hostileBound)
	}
	err := r.err
	if r.c != nil {
		if err == nil {
			ctx, cancel := context.WithTimeout(t.Context(), hostileBound)
			err = r.c.Wait(ctx)
			if ctx.Err() != nil {
				t.Fatalf("the hostile session did not end within %s of its hanging up", hostileBound)
			}
			cancel()
		}
		closed := make(chan error, 1)
		go func() { closed <- r.c.Close(context.Background()) }()
		select {
		case closeErr := <-closed:
			if closeErr != nil {
				t.Fatalf("closing the hostile session: %v", closeErr)
			}
		case <-time.After(hostileBound):
			t.Fatalf("closing the hostile session did not end within %s", hostileBound)
		}
	}
	if err == nil {
		t.Fatal("the hostile session ended without an error")
	}
	return hostileEnd{err: err, commands: int(peer.commands.Load())}
}

// socketPair is the two ends of one managed-memory control socket.
func socketPair(t testing.TB) (ours, theirs *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "hostile-socket")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	return wrap(fds[0]), wrap(fds[1])
}

// hostilePeer is the hostile VMM's end of a session.
type hostilePeer struct {
	conn   *net.UnixConn
	script hostileScript
	// events is the end of the event pipe the VMM writes, when its descriptor
	// is a pipe, and held is the other end of a socket it sent, which it holds
	// open without writing.
	events, held *os.File
	// base is the memory region's address as the VMM stated it.
	base    uint64
	writeMu sync.Mutex
	request uint64
	// arrived holds a token once the pager has sent a frame, and ended is
	// closed once the pager has hung up or the VMM has.
	arrived  chan struct{}
	ended    chan struct{}
	commands atomic.Int64
}

// attach sends the HELLO with its descriptors and the MEMORY_REGION frame.
func (p *hostilePeer) attach(t *testing.T, s hostileScript) error {
	t.Helper()
	sent, err := p.descriptors(s.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var fds []int
	for _, f := range sent {
		fds = append(fds, int(f.Fd()))
	}
	hello := s.hello.apply(vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}).Bytes()
	if len(fds) == 0 {
		err = p.write(hello)
	} else {
		_, _, err = p.conn.WriteMsgUnix(hello, syscall.UnixRights(fds...), nil)
	}
	// The pager holds its own copies of what it was sent.
	for _, f := range sent {
		_ = f.Close()
	}
	if err != nil {
		return err
	}
	region := s.region.apply(vmwire.Frame{Kind: vmwire.MemoryRegion, Offset: hostileBase,
		Length: hostilePages * hostilePage, Flags: uint64(s.kind())})
	p.base = region.Offset
	return p.write(region.Bytes())
}

// descriptors opens what the HELLO carries, keeping the writing end of an
// event pipe for the VMM.
func (p *hostilePeer) descriptors(d descriptor) ([]*os.File, error) {
	switch d {
	case pipeDescriptor:
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		p.events = w
		return []*os.File{r}, nil
	case userfaultDescriptor:
		fd, _, errno := syscall.Syscall(unix.SYS_USERFAULTFD, syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0, 0)
		if errno != 0 {
			return nil, fmt.Errorf("userfaultfd: %w", errno)
		}
		return []*os.File{os.NewFile(fd, "userfaultfd")}, nil
	case nullDescriptor:
		f, err := os.Open(os.DevNull)
		return []*os.File{f}, err
	case socketDescriptor:
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		p.held = os.NewFile(uintptr(fds[1]), "socket")
		return []*os.File{os.NewFile(uintptr(fds[0]), "socket")}, nil
	case twoDescriptors:
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		p.events = w
		null, err := os.Open(os.DevNull)
		return []*os.File{r, null}, err
	}
	return nil, nil
}

// closeDescriptors closes what the VMM kept of the descriptors it sent.
func (p *hostilePeer) closeDescriptors() {
	for _, f := range []*os.File{p.events, p.held} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// write sends bytes on the control socket. A pager that stops reading it is
// what the write deadline is for.
func (p *hostilePeer) write(b []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.conn.SetWriteDeadline(time.Now().Add(hostileBound)); err != nil {
		return err
	}
	return vmwire.WriteBytes(p.conn, b)
}

// event writes one forged UFFD event of the given type.
func (p *hostilePeer) event(kind byte, flags, address uint64) error {
	var b [32]byte
	b[0] = kind
	binary.LittleEndian.PutUint64(b[8:], flags)
	binary.LittleEndian.PutUint64(b[16:], address)
	return p.writeEvents(b[:])
}

func (p *hostilePeer) writeEvents(b []byte) error {
	if p.events == nil {
		return nil
	}
	if err := p.events.SetWriteDeadline(time.Now().Add(hostileBound)); err != nil {
		return err
	}
	_, err := p.events.Write(b)
	return err
}

// fault forges a fault on one byte of a page, which may be past the memory
// region's end.
func (p *hostilePeer) fault(page, offset, flags uint64) error {
	return p.event(0x12, flags, p.base+page*hostilePage+offset%hostilePage)
}

func (p *hostilePeer) nextRequest(id uint64) uint64 {
	p.request++
	if id != 0 {
		return id
	}
	return p.request
}

// play does one op, and reports an error once the session cannot take more.
func (p *hostilePeer) play(op hostileOp) error {
	switch op.kind {
	case opFrame:
		return p.write(vmwire.Frame{Kind: op.a & 0xff, Offset: op.a >> 8, ID: op.b}.Bytes())
	case opSeal:
		return p.write(vmwire.Frame{Kind: vmwire.Seal, ID: p.nextRequest(op.a)}.Bytes())
	case opFlush:
		return p.write(vmwire.Frame{Kind: vmwire.Flush, ID: p.nextRequest(op.a)}.Bytes())
	case opFault:
		return p.fault(op.a%(hostilePages+2), op.b>>8, op.b&0xff)
	case opEvent:
		return p.event(byte(op.a), 0, op.b)
	case opShortEvent:
		var b [32]byte
		b[0] = 0x12
		binary.LittleEndian.PutUint64(b[16:], p.base)
		return p.writeEvents(b[:op.a%31+1])
	case opFaults:
		for i := range op.a%64 + 1 {
			if err := p.fault(i%hostilePages, 0, op.b&0xff); err != nil {
				return err
			}
		}
		return nil
	case opFlushes, opSeals:
		kind, count := uint64(vmwire.Flush), op.a%2048+1
		if op.kind == opSeals {
			kind, count = vmwire.Seal, op.a%16+1
		}
		var b []byte
		for range count {
			b = append(b, vmwire.Frame{Kind: kind, ID: p.nextRequest(0)}.Bytes()...)
		}
		return p.write(b)
	case opPartial:
		return p.write(vmwire.Frame{Kind: vmwire.Seal, ID: p.nextRequest(0)}.Bytes()[:op.a%55+1])
	case opGarbage:
		b := make([]byte, op.a%512+1)
		for i := range b {
			b[i] = byte(op.b)
		}
		return p.write(b)
	case opRights:
		var files []*os.File
		defer func() {
			for _, f := range files {
				_ = f.Close()
			}
		}()
		var fds []int
		for range op.a%4 + 1 {
			f, err := os.Open(os.DevNull)
			if err != nil {
				return err
			}
			files = append(files, f)
			fds = append(fds, int(f.Fd()))
		}
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		_, _, err := p.conn.WriteMsgUnix(vmwire.Frame{Kind: vmwire.Seal, ID: p.nextRequest(0)}.Bytes(),
			syscall.UnixRights(fds...), nil)
		return err
	case opAwait:
		select {
		case <-p.arrived:
		case <-p.ended:
		case <-time.After(hostileTimeout):
		}
		return nil
	case opCloseEvents:
		if p.events != nil {
			err := p.events.Close()
			p.events = nil
			return err
		}
		return nil
	case opCloseWrite:
		return p.conn.CloseWrite()
	case opLinger:
		select {
		case <-p.ended:
		case <-time.After(hostileTimeout):
		}
		return nil
	}
	return nil
}

// respond reads everything the pager sends and answers its commands as the
// script says, until either end hangs up.
func (p *hostilePeer) respond() {
	defer close(p.ended)
	_, arena, err := vmwire.ReceiveAttachment(p.conn)
	if err != nil {
		return
	}
	// A compromised VMM would keep this descriptor, and could write any page
	// of the arena through it. This one gives it up: see the top of the file.
	_ = arena.Close()
	for {
		f, err := vmwire.Read(p.conn)
		if err != nil {
			return
		}
		if f.Kind == vmwire.MapBatch {
			for range min(f.Length, vmwire.MaxBatchRuns) {
				if _, err := vmwire.Read(p.conn); err != nil {
					return
				}
			}
		}
		select {
		case p.arrived <- struct{}{}:
		default:
		}
		if f.Kind == vmwire.Result {
			continue
		}
		ack := vmwire.Frame{Kind: vmwire.Ack, ID: f.ID, Generation: f.Generation}
		var reply []byte
		switch p.script.answer(int(p.commands.Add(1) - 1)) {
		case acknowledge:
			reply = ack.Bytes()
		case refuse:
			ack.Flags = uint64(syscall.ENOSPC)
			reply = ack.Bytes()
		case acknowledgeAnotherID:
			ack.ID++
			reply = ack.Bytes()
		case acknowledgeAnotherGeneration:
			ack.Generation++
			reply = ack.Bytes()
		case acknowledgeTwice:
			reply = append(ack.Bytes(), ack.Bytes()...)
		case acknowledgeWithFields:
			ack.Offset = hostilePage
			reply = ack.Bytes()
		case stayQuiet:
			continue
		case hangUp:
			_ = p.conn.Close()
			return
		}
		if p.write(reply) != nil {
			return
		}
	}
}

// hostileCases are the concrete bad inputs, each with the reason the pager
// gives for ending the session. The fuzz target starts from them.
var hostileCases = []struct {
	name   string
	script hostileScript
	ends   string
}{
	{"a hello without a descriptor", hostileScript{descriptor: noDescriptor}, "expected one UFFD descriptor"},
	{"a hello with two descriptors", hostileScript{descriptor: twoDescriptors}, "expected one UFFD descriptor"},
	{"a hello of another version", hostileScript{hello: frameEdit{2, vmwire.Version - 1}}, "invalid managed-memory hello"},
	{"a hello with a field set", hostileScript{hello: frameEdit{7, 1}}, "invalid managed-memory hello"},
	{"a hello that is not a hello", hostileScript{hello: frameEdit{1, vmwire.Seal}}, "invalid managed-memory hello"},
	{"a memory region of an unknown kind", hostileScript{region: frameEdit{7, 3}}, "invalid managed-memory-region"},
	{"a memory region of the other kind", hostileScript{pmem: true, region: frameEdit{7, uint64(vmmemory.Ram)}},
		"invalid managed-memory-region"},
	{"a memory region longer than its volume", hostileScript{region: frameEdit{4, (hostilePages + 1) * hostilePage}},
		"invalid managed-memory-region"},
	{"a memory region of no pages", hostileScript{region: frameEdit{4, 0}}, "invalid managed-memory-region"},
	{"a memory region at a misaligned address", hostileScript{region: frameEdit{3, hostileBase + 512}},
		"invalid managed-memory-region"},
	{"a memory region that wraps the address space", hostileScript{region: frameEdit{3, math.MaxUint64 - hostilePage + 1}},
		"invalid managed-memory-region"},
	{"a memory region frame of another kind", hostileScript{region: frameEdit{1, vmwire.Attach}},
		"invalid managed-memory-region"},
	{"a descriptor that reads end of file", hostileScript{descriptor: nullDescriptor, awaitEnd: true}, "UFFD"},
	{"a userfaultfd without its handshake", hostileScript{descriptor: userfaultDescriptor, awaitEnd: true},
		"UFFD event read"},
	{"a socket for a userfaultfd", hostileScript{descriptor: socketDescriptor,
		ops: []hostileOp{{opAwait, 0, 0}, {opAwait, 0, 0}}}, "EOF"},
	{"a fault past the memory region's end", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opFault, hostilePages, 0}}}, "invalid UFFD page fault"},
	{"a fault with an unknown flag", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opFault, 0, 8}}}, "invalid UFFD page fault"},
	{"an unknown UFFD event", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opEvent, 0x13, hostileBase}}}, "unexpected UFFD event 0x13"},
	{"a short UFFD event", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opShortEvent, 4, 0}}}, "UFFD event read: 5 bytes"},
	{"an event pipe that closes", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opCloseEvents, 0, 0}}}, "UFFD event read: 0 bytes"},
	{"a fault it cannot be resolved for", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opFault, 1, 0}}}, "UFFDIO_CONTINUE"},
	{"a store into a page it never read", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opFault, 1, 1}}}, "UFFDIO_CONTINUE"},
	{"a store after a refused populate", hostileScript{pmem: true, inherits: true, answers: []answer{refuse, acknowledge},
		ops: []hostileOp{{opFault, 2, 7}, {opLinger, 0, 0}}}, "managed-memory mapping refused"},
	{"a fault on a page it shares", hostileScript{inherits: true, awaitEnd: true,
		ops: []hostileOp{{opFault, 1, 1}}}, "UFFDIO_CONTINUE"},
	{"faults past its queue", hostileScript{awaitEnd: true, answers: []answer{acknowledge, stayQuiet},
		ops: []hostileOp{{opFaults, hostilePages - 1, 0}}}, "capacity"},
	{"remap events", hostileScript{ops: []hostileOp{{opEvent, 0x14, 0}, {opEvent, 0x14, 0},
		{opAwait, 0, 0}, {opAwait, 0, 0}}}, "EOF"},
	{"an acknowledgement of nothing", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opAwait, 0, 0}, {opFrame, vmwire.Ack, 99}}}, "unexpected mapping ACK"},
	{"an acknowledgement sent twice", hostileScript{awaitEnd: true, answers: []answer{acknowledgeTwice}},
		"unexpected mapping ACK"},
	{"an acknowledgement of another command", hostileScript{awaitEnd: true, answers: []answer{acknowledgeAnotherID}},
		"unexpected mapping ACK"},
	{"an acknowledgement at another generation", hostileScript{awaitEnd: true,
		answers: []answer{acknowledgeAnotherGeneration}}, "invalid mapping acknowledgement"},
	{"an acknowledgement with a field set", hostileScript{awaitEnd: true, answers: []answer{acknowledgeWithFields}},
		"invalid mapping acknowledgement"},
	{"a refusal of its readiness", hostileScript{awaitEnd: true, answers: []answer{refuse}},
		"managed-memory mapping refused"},
	{"a client that never answers", hostileScript{awaitEnd: true, answers: []answer{stayQuiet}},
		"context deadline exceeded"},
	{"a client that hangs up for an answer", hostileScript{awaitEnd: true, answers: []answer{hangUp}}, "EOF"},
	{"a seal request of id zero", hostileScript{awaitEnd: true, ops: []hostileOp{{opFrame, vmwire.Seal, 0}}},
		"invalid seal request"},
	{"a seal request reusing an id", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opSeal, 1, 0}, {opSeal, 1, 0}}}, "invalid seal request"},
	{"a seal request with a field set", hostileScript{awaitEnd: true,
		ops: []hostileOp{{opFrame, vmwire.Seal | 1<<8, 1}}}, "invalid seal request"},
	{"a flush of RAM", hostileScript{awaitEnd: true, ops: []hostileOp{{opFlush, 0, 0}}},
		"a flush of a ram memory region"},
	{"a flush request reusing an id", hostileScript{pmem: true, awaitEnd: true,
		ops: []hostileOp{{opFlush, 1, 0}, {opFlush, 1, 0}}}, "invalid flush request"},
	{"a flush request with a field set", hostileScript{pmem: true, awaitEnd: true,
		ops: []hostileOp{{opFrame, vmwire.Flush | 1<<8, 1}}}, "invalid flush request"},
	{"a batch from the client", hostileScript{awaitEnd: true, ops: []hostileOp{{opFrame, vmwire.MapBatch, 1}}},
		"unexpected managed-memory control message"},
	{"an unknown control message", hostileScript{awaitEnd: true, ops: []hostileOp{{opFrame, 99, 1}}},
		"unexpected managed-memory control message"},
	{"a frame cut short", hostileScript{ops: []hostileOp{{opAwait, 0, 0}, {opAwait, 0, 0},
		{opPartial, 20, 0}}}, "unexpected EOF"},
	{"a frame cut short by a half close", hostileScript{awaitEnd: true, ops: []hostileOp{{opAwait, 0, 0},
		{opAwait, 0, 0}, {opPartial, 20, 0}, {opCloseWrite, 0, 0}}}, "unexpected EOF"},
	{"descriptors on a control message", hostileScript{ops: []hostileOp{{opAwait, 0, 0}, {opAwait, 0, 0},
		{opRights, 3, 0}, {opAwait, 0, 0}}}, "EOF"},
}

// A hostile VMM's session ends with an error of its own, and the well-behaved
// process beside it on the same pager keeps its bytes, its fault latency and
// its share of the pager.
func TestAHostileSessionEndsAloneAndLeavesItsNeighbourWhole(t *testing.T) {
	for _, c := range hostileCases {
		t.Run(c.name, func(t *testing.T) {
			fx := newHostileFixture(t)
			end := fx.round(t, c.script)
			if !errorSays(end.err, c.ends) {
				t.Fatalf("the hostile session ended with %q, want it to say %q", end.err, c.ends)
			}
		})
	}
}

// errorSays reports whether an error's text contains what it should say.
func errorSays(err error, says string) bool {
	return err != nil && says != "" && strings.Contains(err.Error(), says)
}

// A client refuses a mapping command when it is out of mapping budget, and a
// refusal changed nothing, so the fault it was for is served again. What frees
// a client's budget is a revocation, so that is what the fault waits for. It
// does not wait for any other change on the host: two refused faults would
// otherwise wake each other, since each one takes and gives back pages, and a
// client that refuses every command would keep the pager serving it for as
// long as it lives.
func TestARefusedFaultWaitsForARevocation(t *testing.T) {
	fx := newHostileFixture(t)
	before := kernelStats(t, fx.h)
	// The two faults are in different read-ahead windows, so they are served
	// at once. After the READY, the client refuses every command.
	end := fx.round(t, hostileScript{answers: []answer{acknowledge, refuse}, ops: []hostileOp{
		{opFault, 0, 0}, {opFault, 4, 0},
		{opAwait, 0, 0}, {opAwait, 0, 0},
		// The pager has nothing to revoke, so no command arrives now.
		{opLinger, 0, 0},
	}})
	if !errorSays(end.err, "EOF") {
		t.Fatalf("the refusing session ended with %q, want the client's hanging up", end.err)
	}
	if end.commands != 3 {
		t.Fatalf("the pager sent the refusing client %d commands, want 3: the READY and one mapping per fault", end.commands)
	}
	if refused := kernelStats(t, fx.h).RefusedMappings - before.RefusedMappings; refused != 2 {
		t.Fatalf("the pager deferred %d refused faults, want 2", refused)
	}
}

// FuzzHostileSession plays arbitrary hostile sessions against one pager beside
// one well-behaved process, which lives through all of them.
func FuzzHostileSession(f *testing.F) {
	for _, c := range hostileCases {
		f.Add(c.script.encode())
	}
	fx := newHostileFixture(f)
	f.Fuzz(func(t *testing.T, input []byte) {
		fx.round(t, decodeHostile(input))
	})
}
