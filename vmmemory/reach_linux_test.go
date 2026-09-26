//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/vmwire"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A VMM is handed files, not only a protocol. A compromised one uses every
// descriptor it is given as far as the kernel lets it: it maps each file
// whole, reads every byte that holds data, and tries every way to write.
// The tests here play such a VMM beside two well-behaved processes: the hostile
// fixture's, in the hostile VMM's own tenant, and one of another tenant. Their
// pages carry markers the hostile VMM must never find, but for one.

const (
	// dirtyMarker fills the pages of the fixture's process that no checkpoint
	// holds, and publishedMarker the ones a checkpoint published and nothing
	// else inherited. tenantMarker fills a page that process published and
	// another memory region of its tenant inherits: the tenant's shared pages
	// are the one thing the design lets a VMM of the tenant read. otherMarker
	// fills every page of the process of another tenant. Nothing else on the
	// pager holds a page of any of these bytes: the fixture's pages are
	// pageByte's, and a hostile volume's are 200 on.
	dirtyMarker     = 0xa5
	publishedMarker = 0xc3
	tenantMarker    = 0x5a
	otherMarker     = 0x3c
	// otherTenant is the tenant of the other process. The fixture's process
	// and every hostile VMM are of none.
	otherTenant = "other"
	// inheritedPage is the RAM page each process publishes and another memory
	// region of its tenant inherits.
	inheritedPage = 4
)

// reacher is a hostile VMM that keeps every file its session is given and
// acknowledges every command without doing what it asks. A REVOKE takes
// nothing away from it: every mapping it made of a file stays.
type reacher struct {
	conn *net.UnixConn
	// events is the end of the pipe the VMM stated as its userfaultfd that it
	// writes, which it never does, so the pager serves it no fault.
	events *os.File

	mu       sync.Mutex
	files    map[uint64]*os.File
	writable map[uint64]bool
	// ready is closed once the pager has sent its first command, which it
	// sends only once the session holds every file it starts with.
	ready     chan struct{}
	readyOnce sync.Once
	ended     chan struct{}
}

// reach attaches a reacher to the fixture's pager with a volume of its own,
// and returns it and its connection once it holds the files its session
// starts with.
func (fx *hostileFixture) reach(t *testing.T) (*reacher, *vmmemory.Connection) {
	t.Helper()
	ours, theirs := socketPair(t)
	type connected struct {
		c   *vmmemory.Connection
		err error
	}
	result := make(chan connected, 1)
	backing := vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: fx.hostileVolume(false)}
	go func() {
		c, err := vmmemory.Connect(t.Context(), fx.h, theirs, backing, hostileConnection)
		result <- connected{c, err}
	}()
	events, stated, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r := &reacher{conn: ours, events: stated, files: map[uint64]*os.File{}, writable: map[uint64]bool{},
		ready: make(chan struct{}), ended: make(chan struct{})}
	hello := vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}.Bytes()
	_, _, err = ours.WriteMsgUnix(hello, syscall.UnixRights(int(events.Fd())), nil)
	_ = events.Close()
	if err != nil {
		t.Fatal(err)
	}
	region := vmwire.Frame{Kind: vmwire.MemoryRegion, Offset: hostileBase,
		Length: hostilePages * hostilePage, Flags: uint64(vmmemory.Ram)}
	if err := vmwire.WriteBytes(ours, region.Bytes()); err != nil {
		t.Fatal(err)
	}
	go r.respond()
	var c connected
	select {
	case c = <-result:
	case <-time.After(hostileBound):
		t.Fatalf("attaching the hostile VMM did not end within %s", hostileBound)
	}
	if c.err != nil {
		t.Fatalf("attaching the hostile VMM: %v", c.err)
	}
	select {
	case <-r.ready:
	case <-r.ended:
		t.Fatal("the pager ended the hostile VMM's session before its first command")
	case <-time.After(hostileBound):
		t.Fatalf("the pager sent the hostile VMM no command within %s", hostileBound)
	}
	return r, c.c
}

// respond keeps every file the pager sends and acknowledges every command,
// until either end hangs up.
func (r *reacher) respond() {
	defer close(r.ended)
	for {
		f, file, err := vmwire.Receive(r.conn)
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			return
		}
		switch f.Kind {
		case vmwire.File:
			r.keep(f, file)
			continue
		case vmwire.Attach, vmwire.DropFile, vmwire.Result:
			// A VMM told to drop a file keeps it, and loses nothing a pager
			// that isolates it could still reach.
			continue
		case vmwire.MapBatch:
			for range min(f.Length, vmwire.MaxBatchRuns) {
				if _, err := vmwire.ReadCommand(r.conn); err != nil {
					return
				}
			}
		}
		r.readyOnce.Do(func() { close(r.ready) })
		if err := vmwire.WriteBytes(r.conn, vmwire.Frame{Kind: vmwire.Ack, ID: f.ID, Generation: f.Generation}.Bytes()); err != nil {
			return
		}
	}
}

// keep holds the descriptor a FILE frame carries, under the file's number. A
// file that grows is sent again, and the newer descriptor replaces the older.
func (r *reacher) keep(f vmwire.Frame, file *os.File) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.files[f.ID]; old != nil {
		_ = old.Close()
	}
	r.files[f.ID] = file
	r.writable[f.ID] = f.Flags&vmwire.FileWritable != 0
}

// held is the numbers of the files the VMM holds, in order.
func (r *reacher) held() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var numbers []uint64
	for number := range r.files {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	return numbers
}

func (r *reacher) file(number uint64) (*os.File, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[number], r.writable[number]
}

// hangUp ends the VMM's side of the session and closes what it kept.
func (r *reacher) hangUp(t *testing.T) {
	t.Helper()
	_ = r.conn.Close()
	select {
	case <-r.ended:
	case <-time.After(hostileBound):
		t.Fatalf("the hostile VMM's reader did not end within %s of its hanging up", hostileBound)
	}
	_ = r.events.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		_ = f.Close()
	}
}

// view is one whole file as the VMM maps it: read-only and shared, so that it
// sees every store anyone makes to the file from then on.
type view struct {
	number uint64
	file   *os.File
	bytes  []byte
}

// mapWhole maps every file the VMM holds, whole, by its number. The mappings
// outlive any revocation, as a compromised VMM's would.
func (r *reacher) mapWhole(t *testing.T) map[uint64]view {
	t.Helper()
	views := map[uint64]view{}
	for _, number := range r.held() {
		f, _ := r.file(number)
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		if st.Size == 0 {
			continue
		}
		b, err := unix.Mmap(int(f.Fd()), 0, int(st.Size), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			t.Fatalf("mapping file %d of the hostile VMM's session read-only: %v", number, err)
		}
		t.Cleanup(func() { _ = unix.Munmap(b) })
		views[number] = view{number: number, file: f, bytes: b}
	}
	return views
}

// markers counts the pages of a view that hold nothing but one of the markers.
// It reads only where the file holds data, found with SEEK_DATA and SEEK_HOLE,
// so that reading a hole allocates nothing the pager would then count.
func (v view) markers(t *testing.T) map[byte]int {
	t.Helper()
	found := map[byte]int{}
	fd := int(v.file.Fd())
	size := int64(len(v.bytes))
	for offset := int64(0); offset < size; {
		data, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			t.Fatal(err)
		}
		for page := data / hostilePage * hostilePage; page < hole && page < size; page += hostilePage {
			if marker, ok := wholePageOf(v.bytes[page : page+hostilePage]); ok {
				found[marker]++
			}
		}
		offset = hole
	}
	return found
}

// wholePageOf reports the marker a page holds in every byte, if it does.
func wholePageOf(page []byte) (byte, bool) {
	first := page[0]
	switch first {
	case dirtyMarker, publishedMarker, tenantMarker, otherMarker:
	default:
		return 0, false
	}
	for _, b := range page {
		if b != first {
			return 0, false
		}
	}
	return first, true
}

// reachable counts the marker pages every view of the VMM's reaches.
func reachable(t *testing.T, views map[uint64]view) map[byte]int {
	t.Helper()
	found := map[byte]int{}
	for _, v := range views {
		for marker, pages := range v.markers(t) {
			found[marker] += pages
		}
	}
	return found
}

// markNeighbours has the fixture's process hold its markers and starts the
// process of another tenant. The fixture's process holds RAM page 3 published
// by a checkpoint and inherited by nothing, RAM page 4 published and inherited
// by another memory region of its tenant, and RAM page 2 and PMEM page 5
// stored since, which no checkpoint holds.
func (fx *hostileFixture) markNeighbours(t *testing.T) *otherProcess {
	t.Helper()
	for _, published := range []struct {
		page   int
		marker byte
	}{{3, publishedMarker}, {inheritedPage, tenantMarker}} {
		if err := fx.store(t.Context(), published.page, published.marker); err != nil {
			t.Fatal(err)
		}
		fx.values[1][published.page] = published.marker
	}
	fx.inherit(t, "", fx.backings[1], inheritedPage)
	fx.markDirty(t, 1, 2)
	fx.markDirty(t, 0, 5)
	if err := fx.check(); err != nil {
		t.Fatal(err)
	}
	return fx.startOther(t)
}

// inherit attaches a memory region of tenant that inherits what b's last
// checkpoint published, and has it read one page of it. Nothing maps what the
// pager gives the region, and it stays attached until the test ends.
func (fx *hostileFixture) inherit(t *testing.T, tenant string, b *kernelBacking, page uint64) {
	t.Helper()
	r, err := fx.h.Attach(t.Context(), vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: b.fork(),
		Tenant: tenant}, seedMapping{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Detach(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := r.Fault(t.Context(), page, false); err != nil {
		t.Fatal(err)
	}
}

// otherProcess is the well-behaved process of another tenant, and the volume
// of its RAM. Every page of its PMEM and RAM holds otherMarker, always.
type otherProcess struct {
	process *nativeProcess
	ram     *kernelBacking
}

// startOther starts the process of another tenant on the fixture's pager. It
// reads every page, which loads them by identity. It publishes RAM page 4,
// which another memory region of its tenant inherits, and stores into RAM page
// 2 and PMEM page 5 since, which no checkpoint holds.
func (fx *hostileFixture) startOther(t *testing.T) *otherProcess {
	t.Helper()
	var provided []vmmemory.Backing
	var volumes [2]*kernelBacking
	for region := range volumes {
		b := newPagedKernelBacking(byte(5+region), hostilePages*hostilePage, hostilePage)
		b.inTenant(otherTenant)
		for i := range b.data {
			b.data[i] = otherMarker
		}
		volumes[region] = b
		provided = append(provided, b)
	}
	o := &otherProcess{ram: volumes[1], process: startNativeIn(t, fx.h, otherTenant, hostilePages,
		vmmemory.ConnectionConfig{Name: "other", QueuePages: hostilePages, CommandTimeout: 5 * time.Second,
			VerifyInterval: time.Hour}, provided...)}
	if err := o.check(); err != nil {
		t.Fatal(err)
	}
	if err := o.store(t.Context(), inheritedPage); err != nil {
		t.Fatal(err)
	}
	fx.inherit(t, otherTenant, o.ram, inheritedPage)
	for _, dirty := range []struct{ region, page int }{{1, 2}, {0, 5}} {
		if err := o.fill(dirty.region, dirty.page); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.check(); err != nil {
		t.Fatal(err)
	}
	return o
}

// fill has the process store its marker into one page.
func (o *otherProcess) fill(region, page int) error {
	return o.process.ask(fmt.Sprintf("fill %d %d %d %d", region, page*hostilePage, hostilePage, otherMarker), "filled")
}

// store has the process store its marker into one page of its RAM and then
// publish its RAM.
func (o *otherProcess) store(ctx context.Context, page int) error {
	if err := o.fill(1, page); err != nil {
		return err
	}
	return publishRAM(ctx, o.process, o.ram)
}

// check reports the first page of the process that does not hold its marker.
func (o *otherProcess) check() error {
	for region := range 2 {
		for page := range hostilePages {
			command := fmt.Sprintf("stridescan %d %d %d 1 %d", region, page*hostilePage, hostilePage, otherMarker)
			if err := o.process.ask(command, "strided"); err != nil {
				return fmt.Errorf("page %d of memory region %d of the other tenant's process: %w", page, region, err)
			}
		}
	}
	return nil
}

// inTenant makes b the volume of a VM of tenant, whose checkpoints are that
// tenant's.
func (b *kernelBacking) inTenant(tenant string) {
	b.owner = control.InTenant(tenant, b.owner)
	b.source.VM = control.InTenant(tenant, b.source.VM)
}

// fork is the volume of another VM of b's tenant, which inherits what b's last
// checkpoint published.
func (b *kernelBacking) fork() *kernelBacking {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &kernelBacking{data: bytes.Clone(b.data), source: b.source, owner: b.owner + "-fork", page: b.page,
		sequence: 1, private: map[uint64]bool{}, holes: maps.Clone(b.holes)}
}

// markDirty has the well-behaved process store the dirty marker into one page.
func (fx *hostileFixture) markDirty(t *testing.T, region, page int) {
	t.Helper()
	fill := fmt.Sprintf("fill %d %d %d %d", region, page*hostilePage, hostilePage, dirtyMarker)
	if err := fx.good.ask(fill, "filled"); err != nil {
		t.Fatal(err)
	}
	fx.values[region][page] = dirtyMarker
}

// requireErrno fails the test unless err is the errno the kernel gives the
// attempt on a descriptor that is not open for writing.
func requireErrno(t *testing.T, attempt string, number uint64, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s through read-only file %d = %v, want %v", attempt, number, err, want)
	}
}

// A compromised VMM that uses every descriptor its session gives it reaches
// none of another VM's pages. Of a VM of its own tenant, it reaches neither the
// pages no checkpoint holds nor the ones a checkpoint published that nothing
// else inherited. Of a VM of another tenant, it reaches nothing at all: not the
// pages that VM loaded by identity, not the one another memory region of that
// tenant inherits, and not its dirty pages. It does reach the page its own
// tenant published and another region of the tenant inherits, which is the
// one thing the design concedes.
//
// It cannot write a file it was given read-only. Run as another user, as a
// jailed VMM is, it can neither reopen such a file for writing nor change the
// mode of any file it holds. It can allocate memory in its
// own private file, and the pager ends its session for that. Mappings it keeps
// past every revocation reach nothing more after the other VMs go on storing
// and publishing. The other VMs keep their bytes, and the pager and the host
// get back everything the session held.
func TestAHostileVMMReachesNoOtherVMsBytes(t *testing.T) {
	fx := newHostileFixtureFor(t, vmmemory.ArenaIsolated, 8)
	other := fx.markNeighbours(t)
	if s := kernelStats(t, fx.h); s.MovedPages != 2 {
		t.Fatalf("%d published pages moved into a shared file, want the two another region inherits", s.MovedPages)
	}
	fx.baseline(t)
	r, c := fx.reach(t)
	numbers := r.held()
	if len(numbers) != 2 || numbers[0] != vmwire.PrivateFile || numbers[1] != vmwire.SharedFile {
		t.Fatalf("the hostile VMM's session holds files %v, want its private file and its tenant's shared file", numbers)
	}

	// 1. Every byte of every file it holds, through one mapping of each.
	views := r.mapWhole(t)
	concession := map[byte]int{tenantMarker: 1}
	if found := reachable(t, views); !maps.Equal(found, concession) {
		t.Fatalf("the hostile VMM reads %v whole pages of each marker, want only its tenant's shared page, %v",
			found, concession)
	}

	// 2. Every way to write a file it holds read-only.
	for _, number := range numbers {
		f, writable := r.file(number)
		if writable {
			continue
		}
		fd := int(f.Fd())
		size := len(views[number].bytes)
		_, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		requireErrno(t, "a writable shared mapping", number, err, unix.EACCES)
		_, err = unix.Pwrite(fd, make([]byte, hostilePage), 0)
		requireErrno(t, "a write", number, err, unix.EBADF)
		requireErrno(t, "an allocation", number, unix.Fallocate(fd, 0, 0, hostilePage), unix.EBADF)
		requireErrno(t, "a punch", number,
			unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, hostilePage), unix.EBADF)
		requireErrno(t, "a truncation", number, unix.Ftruncate(fd, 0), unix.EINVAL)
		_, err = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE)
		requireErrno(t, "a seal", number, err, unix.EPERM)
	}
	// A jailed VMM runs as another user, so it can neither open a file anew
	// for writing through /proc/self/fd nor change a file's mode.
	var files []*os.File
	for _, number := range numbers {
		f, _ := r.file(number)
		files = append(files, f)
	}
	for i, attempt := range jailAttempts(t, files) {
		f, writable := r.file(numbers[i])
		if !writable {
			requireErrno(t, "a jailed VMM's reopening for writing", numbers[i], attempt.reopen, unix.EACCES)
		}
		if attempt.fchmod != unix.EPERM {
			t.Errorf("a jailed VMM's fchmod of file %d = %v, want %v", numbers[i], attempt.fchmod, unix.EPERM)
		}
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("file %d has mode %v after the jailed VMM's attempts, want 0600", numbers[i], st.Mode().Perm())
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// 3. The other VMs go on storing and publishing into every page of their
	// RAM but the one another region inherits, which revokes and reuses what
	// the pager holds of them, while the hostile VMM keeps every mapping it
	// made. They end as they began, with RAM page 2 stored since their last
	// checkpoint, and sharing the inherited page.
	for round := range 2 * hostilePages {
		page := round % hostilePages
		if page == inheritedPage {
			continue
		}
		value := byte(dirtyMarker)
		if round%2 == 0 {
			value = publishedMarker
		}
		if err := fx.store(t.Context(), page, value); err != nil {
			t.Fatal(err)
		}
		fx.values[1][page] = value
		if err := other.store(t.Context(), page); err != nil {
			t.Fatal(err)
		}
	}
	fx.markDirty(t, 1, 2)
	if err := other.fill(1, 2); err != nil {
		t.Fatal(err)
	}
	if found := reachable(t, views); !maps.Equal(found, concession) {
		t.Fatalf("after the other VMs stored and published, the hostile VMM's kept mappings read %v "+
			"whole pages of each marker, want only its tenant's shared page, %v", found, concession)
	}

	// 4. It writes, punches and allocates every offset of its private file.
	private, _ := r.file(vmwire.PrivateFile)
	fd := int(private.Fd())
	size := int64(len(views[vmwire.PrivateFile].bytes))
	for offset := int64(0); offset < size; offset += hostilePage {
		if _, err := unix.Pwrite(fd, make([]byte, hostilePage), offset); err != nil {
			t.Fatalf("writing offset %d of its own private file: %v", offset, err)
		}
	}
	if err := unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, size/2); err != nil {
		t.Fatalf("punching its own private file: %v", err)
	}
	if err := unix.Fallocate(fd, 0, 0, size); err != nil {
		t.Fatalf("allocating its own private file: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), hostileBound)
	defer cancel()
	if err := c.MemoryRegion().Memory.Verify(ctx); !errors.Is(err, vmmemory.ErrUncounted) {
		t.Fatalf("verifying the session of a VMM that allocated its own private file = %v, want ErrUncounted", err)
	}
	if err := c.Wait(ctx); !errors.Is(err, vmmemory.ErrUncounted) {
		t.Fatalf("the hostile VMM's session ended with %v, want ErrUncounted", err)
	}
	r.hangUp(t)
	if err := c.Close(ctx); err != nil {
		t.Fatalf("closing the hostile VMM's session: %v", err)
	}
	fx.requireWhole(t, "a VMM that reached through its files")
	if err := other.check(); err != nil {
		t.Fatalf("after a VMM that reached through its files: %v", err)
	}
}

// The hole the isolated arena closes: in a shared arena, the one file every
// session is given is the whole arena, writable, so a VMM that maps it reads
// every page of every other VM on the pager, of every tenant.
func TestASharedArenaHandsEveryVMMItsNeighboursBytes(t *testing.T) {
	fx := newHostileFixtureFor(t, vmmemory.ArenaShared, 8)
	fx.markNeighbours(t)
	r, c := fx.reach(t)
	defer func() {
		r.hangUp(t)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hostileBound)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("closing the hostile VMM's session: %v", err)
		}
	}()
	numbers := r.held()
	if len(numbers) != 1 || numbers[0] != vmwire.PrivateFile {
		t.Fatalf("the hostile VMM's session holds files %v, want the arena alone", numbers)
	}
	if _, writable := r.file(vmwire.PrivateFile); !writable {
		t.Fatal("the shared arena was handed over read-only, want read-write")
	}
	// The other tenant's process holds the 32 pages it loaded and a copy of
	// each of the three it stored into. The region that inherited its page 4
	// read pages 5 to 7 ahead of it.
	found := reachable(t, r.mapWhole(t))
	if want := (map[byte]int{dirtyMarker: 2, publishedMarker: 1, tenantMarker: 1, otherMarker: 38}); !maps.Equal(found, want) {
		t.Fatalf("the hostile VMM reads %v whole pages of each marker, want %v", found, want)
	}
}
