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
const shared = 1

type page struct {
	id      int
	initial []byte // immutable fixture backing; nil for private pages
	private bool
	slot    int // -1 means nonresident
	spilled bool
	aliases map[*alias]struct{}
}

type alias struct {
	client     *pagerClient
	index      uint64
	address    uint64
	generation uint64
	mapped     bool
	page       *page
}

// pagerClient is one managed-memory session, which maps one region.
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
	t            *testing.T
	mu           sync.Mutex
	pageSize     int
	arena, spill *os.File
	free         []int
	pages        []*page
	initial      map[[2]int]*page
	clients      []*pagerClient
	sequence     uint64
	err          error
	failSpill    bool
	spillWrites  int
	remaps       atomic.Int64
	faults       atomic.Int64
	queueMu      sync.Mutex
	queue        map[faultKey]uint64
	notify       chan struct{}
	done         chan struct{}
	workerDone   chan struct{}
}

func newPager(t *testing.T, slots int) *pager {
	t.Helper()
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("run scripts/test-vm-memory-lima.sh for real Linux memory tests")
	}
	size := 2 << 20
	arena, err := vmwire.HugeMemfd("sproutfs-page-arena", int64(slots*size))
	if err != nil {
		t.Fatal(err)
	}
	spill, err := os.CreateTemp(t.TempDir(), "spill-")
	if err != nil {
		arena.Close()
		t.Fatal(err)
	}
	p := &pager{t: t, pageSize: size, arena: arena, spill: spill, initial: make(map[[2]int]*page), queue: make(map[faultKey]uint64), notify: make(chan struct{}, 1), done: make(chan struct{}), workerDone: make(chan struct{})}
	for i := slots - 1; i >= 0; i-- {
		p.free = append(p.free, i)
	}
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
		p.arena.Close()
		p.spill.Close()
	})
	return p
}

// accept takes one session of a client process. region names the family of
// pages it maps, which is what two processes share when they map the same one.
func (p *pager) accept(listener *net.UnixListener, region int) (*pagerClient, error) {
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
	if f.Kind != vmwire.Region || f.Flags != uint64(region+1) || f.Length == 0 || f.Length > uint64(p.pageSize*4096) || f.Length%uint64(p.pageSize) != 0 || f.Offset%uint64(p.pageSize) != 0 || f.Offset > ^uint64(0)-f.Length {
		return fail(fmt.Errorf("invalid region: %+v", f))
	}
	for i := range int(f.Length / uint64(p.pageSize)) {
		key := [2]int{region, i}
		pg := p.initial[key]
		if pg == nil {
			pg = &page{id: len(p.pages), slot: -1, initial: make([]byte, p.pageSize), aliases: make(map[*alias]struct{})}
			for j := range pg.initial {
				pg.initial[j] = byte(17 + region*17 + i)
			}
			p.pages = append(p.pages, pg)
			p.initial[key] = pg
		}
		a := &alias{client: c, index: uint64(i), address: f.Offset + uint64(i*p.pageSize), page: pg}
		pg.aliases[a] = struct{}{}
		c.aliases = append(c.aliases, a)
	}
	stat, err := p.arena.Stat()
	if err != nil {
		return fail(err)
	}
	if err = vmwire.SendFD(conn, vmwire.Frame{Kind: vmwire.Attach, ID: vmwire.Version, Length: uint64(stat.Size())}, p.arena); err != nil {
		return fail(err)
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
		if !a.page.private {
			f.Flags = shared
		}
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
func (p *pager) fallocate(mode uint32, offset int64) error {
	for {
		err := syscall.Fallocate(int(p.arena.Fd()), mode, offset, int64(p.pageSize))
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (p *pager) makeResident(pg *page) error {
	if pg.slot >= 0 {
		return nil
	}
	if len(p.free) == 0 {
		return errors.New("fixture arena exhausted")
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
	slot := p.free[len(p.free)-1]
	if err := p.fallocate(1, int64(slot*p.pageSize)); err != nil {
		return err
	}
	mapped, err := syscall.Mmap(int(p.arena.Fd()), int64(slot*p.pageSize), p.pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	copy(mapped, bytes)
	if err := syscall.Munmap(mapped); err != nil {
		return err
	}
	p.free = p.free[:len(p.free)-1]
	pg.slot = slot
	return nil
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
		return fmt.Errorf("fault outside the registered region: %#x", f.key.address)
	}
	if err := p.makeResident(a.page); err != nil {
		return err
	}
	// A WRITE or WP event must obtain private backing before any write resumes,
	// including first access being a store into an entirely nonresident page.
	if f.flags&3 != 0 && !a.page.private {
		bytes := make([]byte, p.pageSize)
		if _, err := p.arena.ReadAt(bytes, int64(a.page.slot*p.pageSize)); err != nil {
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
		bytes := make([]byte, p.pageSize)
		if _, err := p.arena.ReadAt(bytes, int64(pg.slot*p.pageSize)); err != nil {
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
	if err := p.fallocate(3, int64(pg.slot*p.pageSize)); err != nil { // KEEP_SIZE | PUNCH_HOLE
		return err
	}
	p.free = append(p.free, pg.slot)
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
