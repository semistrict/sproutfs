//go:build linux && (amd64 || arm64)

package vmtest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmwire"
)

// shared is the mapping-command flag that keeps a page read-only and shared.
const shared = vmwire.Immutable

type page struct {
	id      int
	initial []byte // immutable fixture backing; nil for private pages
	private bool
	file    uint64 // the file the page's slot is in
	slot    int    // -1 means nonresident
	spilled bool
	aliases map[*alias]struct{}
}

// arenaLayout is where the fixture puts the pages it shares.
type arenaLayout int

const (
	// sharedArena puts every page in one file that every client receives
	// read-write, as the pager does today.
	sharedArena arenaLayout = iota
	// isolatedArena puts the pages it shares in a second file that clients
	// receive read-only and map private, and only private pages in the file
	// they receive read-write.
	isolatedArena
)

func (l arenaLayout) String() string {
	if l == isolatedArena {
		return "isolated"
	}
	return "shared"
}

// eachArena runs a test once over each layout.
func eachArena(t *testing.T, test func(t *testing.T, layout arenaLayout)) {
	for _, layout := range []arenaLayout{sharedArena, isolatedArena} {
		t.Run(layout.String(), func(t *testing.T) { test(t, layout) })
	}
}

// fixtureFile is one file the fixture hands its clients: its number, the
// fixture's own read-write descriptor, the descriptor its clients receive, and
// its free slots.
type fixtureFile struct {
	number uint64
	file   *os.File
	client *os.File
	free   []int
}

type alias struct {
	client     *pagerClient
	index      uint64
	address    uint64
	generation uint64
	mapped     bool
	page       *page
}

// pagerClient is one managed-memory session, which maps one memory region.
type pagerClient struct {
	conn       *net.UnixConn
	uffd       *os.File
	aliases    []*alias
	closing    atomic.Bool
	readerDone chan struct{}
}

type faultKey struct {
	client  *pagerClient
	address uint64
}
type fault struct {
	key   faultKey
	flags uint64
}

// pager is a real syscall/IPC fixture with explicit eviction policy. A single
// state lock serializes page transitions; independent UFFD readers always drain
// REMAP events, even while a transition is waiting for a client acknowledgement.
type pager struct {
	t        *testing.T
	mu       sync.Mutex
	pageSize int
	// files are the files every client receives. File 0 is the arena every
	// client receives read-write. An isolated arena has a second file, which
	// clients receive read-only.
	files       []*fixtureFile
	spill       *os.File
	pages       []*page
	initial     map[[2]int]*page
	clients     []*pagerClient
	sequence    uint64
	err         error
	failSpill   bool
	spillWrites int
	remaps      atomic.Int64
	faults      atomic.Int64
	queueMu     sync.Mutex
	queue       map[faultKey]uint64
	notify      chan struct{}
	done        chan struct{}
	workerDone  chan struct{}
}

func newPager(t *testing.T, slots int) *pager {
	t.Helper()
	return newArenaPager(t, slots, sharedArena)
}

// newArenaPager is a pager of the given layout whose every file has this many
// slots.
func newArenaPager(t *testing.T, slots int, layout arenaLayout) *pager {
	t.Helper()
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("run scripts/test-vm-memory-lima.sh for real Linux memory tests")
	}
	size := 2 << 20
	p := &pager{t: t, pageSize: size, initial: make(map[[2]int]*page), queue: make(map[faultKey]uint64), notify: make(chan struct{}, 1), done: make(chan struct{}), workerDone: make(chan struct{})}
	names := []string{"sproutfs-page-arena"}
	if layout == isolatedArena {
		names = append(names, "sproutfs-page-shared")
	}
	for number, name := range names {
		f := &fixtureFile{number: uint64(number)}
		var err error
		if f.file, err = vmwire.ArenaMemfd(name, uint64(size), int64(slots*size)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.file.Close() })
		// Every file but the private one goes to clients as a new open of it
		// that can only read, as the pager sends it.
		f.client = f.file
		if number != vmwire.PrivateFile {
			if f.client, err = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", f.file.Fd()), os.O_RDONLY, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.client.Close() })
		}
		for i := slots - 1; i >= 0; i-- {
			f.free = append(f.free, i)
		}
		p.files = append(p.files, f)
	}
	spill, err := os.CreateTemp(t.TempDir(), "spill-")
	if err != nil {
		t.Fatal(err)
	}
	p.spill = spill
	go p.work()
	t.Cleanup(func() {
		close(p.done)
		for _, c := range p.clients {
			c.closing.Store(true)
			c.conn.Close()
			c.uffd.Close()
			<-c.readerDone
		}
		<-p.workerDone
		p.spill.Close()
	})
	return p
}

// fileFor is the file a page belongs in: an isolated arena's shared file for a
// page the pager shares, and the arena for every other.
func (p *pager) fileFor(pg *page) *fixtureFile {
	if !pg.private && len(p.files) > 1 {
		return p.files[vmwire.SharedFile]
	}
	return p.files[vmwire.PrivateFile]
}

// accept takes one session of a client process. memory region names the family of
// pages it maps, which is what two processes share when they map the same one.
func (p *pager) accept(listener *net.UnixListener, memoryRegion int) (*pagerClient, error) {
	conn, err := listener.AcceptUnix()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	header, fd, err := vmwire.ReceiveFD(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &pagerClient{conn: conn, uffd: fd, readerDone: make(chan struct{})}
	fail := func(err error) (*pagerClient, error) { conn.Close(); fd.Close(); return nil, err }
	if header.Kind != vmwire.Hello || header.ID != vmwire.Version || header.Length != 0 {
		return fail(fmt.Errorf("invalid hello: %+v", header))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := vmwire.Read(conn)
	if err != nil {
		return fail(err)
	}
	if f.Kind != vmwire.MemoryRegion || f.Flags != uint64(memoryRegion+1) || f.Length == 0 || f.Length > uint64(p.pageSize*4096) || f.Length%uint64(p.pageSize) != 0 || f.Offset%uint64(p.pageSize) != 0 || f.Offset > ^uint64(0)-f.Length {
		return fail(fmt.Errorf("invalid memory region: %+v", f))
	}
	for i := range int(f.Length / uint64(p.pageSize)) {
		key := [2]int{memoryRegion, i}
		pg := p.initial[key]
		if pg == nil {
			pg = &page{id: len(p.pages), slot: -1, initial: make([]byte, p.pageSize), aliases: make(map[*alias]struct{})}
			for j := range pg.initial {
				pg.initial[j] = byte(17 + memoryRegion*17 + i)
			}
			p.pages = append(p.pages, pg)
			p.initial[key] = pg
		}
		a := &alias{client: c, index: uint64(i), address: f.Offset + uint64(i*p.pageSize), page: pg}
		pg.aliases[a] = struct{}{}
		c.aliases = append(c.aliases, a)
	}
	// The attachment states this fixture's geometry: the page its memory
	// regions run, and the memory its files are made of. The files follow it.
	backing, err := vmwire.BackingFor(uint64(p.pageSize))
	if err != nil {
		return fail(err)
	}
	if err := vmwire.Write(conn, vmwire.AttachFrame(uint64(p.pageSize), backing, 0)); err != nil {
		return fail(err)
	}
	for _, file := range p.files {
		stat, err := file.file.Stat()
		if err != nil {
			return fail(err)
		}
		frame := vmwire.FileFrame(file.number, uint64(stat.Size()), backing, file.number == vmwire.PrivateFile)
		if err := vmwire.SendFD(conn, frame, file.client); err != nil {
			return fail(err)
		}
	}
	p.sequence++
	if err := vmwire.Write(conn, vmwire.Frame{Kind: vmwire.Ready, ID: p.sequence}); err != nil {
		return fail(err)
	}
	ack, err := vmwire.Read(conn)
	if err != nil || ack != (vmwire.Frame{Kind: vmwire.Ack, ID: p.sequence}) {
		return fail(fmt.Errorf("attach readiness: %+v %v", ack, err))
	}
	conn.SetDeadline(time.Time{})
	p.clients = append(p.clients, c)
	go p.readFaults(c)
	return c, nil
}

func (p *pager) readFaults(c *pagerClient) {
	defer close(c.readerDone)
	buffer := make([]byte, 32)
	for !c.closing.Load() {
		n, err := c.uffd.Read(buffer)
		if errors.Is(err, syscall.EAGAIN) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil || n != 32 {
			if !c.closing.Load() {
				p.recordError(fmt.Errorf("UFFD read: n=%d err=%v", n, err))
			}
			return
		}
		switch buffer[0] {
		case 0x14: // REMAP must be drained without taking the page-state lock.
			p.remaps.Add(1)
		case 0x12:
			p.faults.Add(1)
			flags := binary.LittleEndian.Uint64(buffer[8:16])
			address := binary.LittleEndian.Uint64(buffer[16:24]) & ^uint64(p.pageSize-1)
			p.queueMu.Lock()
			p.queue[faultKey{c, address}] |= flags
			p.queueMu.Unlock()
			select {
			case p.notify <- struct{}{}:
			default:
			}
		default:
			p.recordError(fmt.Errorf("unexpected UFFD event %#x", buffer[0]))
			return
		}
	}
}

func (p *pager) recordError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
	for _, c := range p.clients {
		c.conn.Close()
	}
}

func (p *pager) work() {
	defer close(p.workerDone)
	for {
		select {
		case <-p.done:
			return
		case <-p.notify:
		}
		for {
			p.queueMu.Lock()
			var f fault
			found := false
			for key, flags := range p.queue {
				f = fault{key, flags}
				delete(p.queue, key)
				found = true
				break
			}
			p.queueMu.Unlock()
			if !found {
				break
			}
			p.mu.Lock()
			err := p.handleFault(f)
			p.mu.Unlock()
			if err != nil {
				p.recordError(err)
				return
			}
		}
	}
}

func (p *pager) command(c *pagerClient, f vmwire.Frame) (vmwire.Frame, error) {
	c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetDeadline(time.Time{})
	if err := vmwire.Write(c.conn, f); err != nil {
		return vmwire.Frame{}, err
	}
	response, err := vmwire.Read(c.conn)
	if err != nil {
		return vmwire.Frame{}, err
	}
	if response.Kind != vmwire.Ack || response.ID != f.ID || response.Generation != f.Generation {
		return vmwire.Frame{}, fmt.Errorf("wrong acknowledgement: command=%+v response=%+v", f, response)
	}
	return response, nil
}

func (p *pager) change(a *alias, kind uint64) error {
	p.sequence++
	f := vmwire.Frame{Kind: kind, ID: p.sequence, Offset: a.index * uint64(p.pageSize), Length: uint64(p.pageSize), Generation: a.generation + 1}
	if kind == vmwire.MapRange {
		f.Backing = uint64(a.page.slot * p.pageSize)
		f.Flags = vmwire.MapFlags(a.page.file, !a.page.private)
	}
	response, err := p.command(a.client, f)
	if err != nil {
		return err
	}
	if response.Flags != 0 {
		return fmt.Errorf("mapping command rejected: %v", syscall.Errno(response.Flags))
	}
	a.generation = f.Generation
	a.mapped = kind == vmwire.MapRange
	return nil
}

// fallocate allocates or punches one arena slot. HugeTLB allocation can observe
// a runtime signal after dropping its locks, and retrying the same request is
// safe even after partial progress, exactly as the production arena does it.
func (p *pager) fallocate(f *fixtureFile, mode uint32, offset int64) error {
	for {
		err := syscall.Fallocate(int(f.file.Fd()), mode, offset, int64(p.pageSize))
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (p *pager) makeResident(pg *page) error {
	if pg.slot >= 0 {
		return nil
	}
	f := p.fileFor(pg)
	if len(f.free) == 0 {
		return fmt.Errorf("fixture file %d exhausted", f.number)
	}
	bytes := pg.initial
	if pg.spilled {
		bytes = make([]byte, p.pageSize)
		if _, err := p.spill.ReadAt(bytes, int64(pg.id*p.pageSize)); err != nil {
			return err
		}
	}
	if len(bytes) != p.pageSize {
		return errors.New("page has no recoverable backing")
	}
	slot := f.free[len(f.free)-1]
	if err := p.fallocate(f, 1, int64(slot*p.pageSize)); err != nil {
		return err
	}
	mapped, err := syscall.Mmap(int(f.file.Fd()), int64(slot*p.pageSize), p.pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	copy(mapped, bytes)
	if err := syscall.Munmap(mapped); err != nil {
		return err
	}
	f.free = f.free[:len(f.free)-1]
	pg.file, pg.slot = f.number, slot
	return nil
}

// read reads a resident page's bytes from its file.
func (p *pager) read(pg *page) ([]byte, error) {
	bytes := make([]byte, p.pageSize)
	_, err := p.files[pg.file].file.ReadAt(bytes, int64(pg.slot*p.pageSize))
	return bytes, err
}

func (p *pager) handleFault(f fault) error {
	if f.key.client.closing.Load() {
		return nil
	}
	var a *alias
	for _, candidate := range f.key.client.aliases {
		if candidate.address == f.key.address {
			a = candidate
			break
		}
	}
	if a == nil {
		return fmt.Errorf("fault outside the registered memory region: %#x", f.key.address)
	}
	if err := p.makeResident(a.page); err != nil {
		return err
	}
	// A WRITE or WP event must obtain private backing before any write resumes,
	// including first access being a store into an entirely nonresident page.
	if f.flags&3 != 0 && !a.page.private {
		bytes, err := p.read(a.page)
		if err != nil {
			return err
		}
		private := &page{id: len(p.pages), private: true, slot: -1, initial: bytes, aliases: make(map[*alias]struct{})}
		if err := p.makeResident(private); err != nil {
			return err
		}
		private.initial = nil
		delete(a.page.aliases, a)
		private.aliases[a] = struct{}{}
		a.page = private
		p.pages = append(p.pages, private)
		a.mapped = false // replace the old shared alias with its private copy
	}
	if !a.mapped {
		if err := p.change(a, vmwire.MapRange); err != nil {
			return err
		}
	}
	return vmwire.Resolve(a.client.uffd.Fd(), a.address, uint64(p.pageSize), uint64(p.pageSize), a.page.private)
}

// evictLocked revokes all aliases before observing final bytes, and does not
// release the slot until disk write + sync succeed. A failed spill keeps RAM.
func (p *pager) evictLocked(pg *page) error {
	if pg.slot < 0 {
		return nil
	}
	for a := range pg.aliases {
		if a.mapped {
			if err := p.change(a, vmwire.Revoke); err != nil {
				return err
			}
		}
	}
	if pg.private {
		bytes, err := p.read(pg)
		if err != nil {
			return err
		}
		if p.failSpill {
			return errors.New("injected spill write failure")
		}
		if _, err := p.spill.WriteAt(bytes, int64(pg.id*p.pageSize)); err != nil {
			return err
		}
		if err := p.spill.Sync(); err != nil {
			return err
		}
		pg.spilled = true
		p.spillWrites++
	}
	f := p.files[pg.file]
	if err := p.fallocate(f, 3, int64(pg.slot*p.pageSize)); err != nil { // KEEP_SIZE | PUNCH_HOLE
		return err
	}
	f.free = append(f.free, pg.slot)
	pg.slot = -1
	return nil
}

func (p *pager) evict(c *pagerClient, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	return p.evictLocked(c.aliases[index].page)
}
