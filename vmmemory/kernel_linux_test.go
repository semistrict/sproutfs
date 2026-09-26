//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// hugePageSize is the page the tests in this file run at: the PMEM pager's,
// over a HugeTLB arena, which is the geometry a guest's disk has. RAM's 4 KiB
// page over an ordinary arena is exercised in small_page_linux_test.go, through
// the same UFFD and the same transport.
const hugePageSize = checkpoint.PageSize2MiB

// kernelBacking models one memory region's volume: its pages are inherited from one
// checkpoint shared by every process mapping that memory region, and a written page
// becomes this backing's own overlay data until published again.
type kernelBacking struct {
	mu      sync.Mutex
	data    []byte
	source  control.Ref
	owner   string
	zero    bool
	private map[uint64]bool
	// holes are the pages this volume holds no object for, which is what a
	// publication makes of a page that reads as all zeroes: it costs no object,
	// nothing is uploaded for it, and it reads back as the zeroes it holds.
	holes  map[uint64]bool
	fenced atomic.Bool
	// page is the page this volume is published in, which must be the page of
	// the pager it is given to. Zero means the one this file's tests run at.
	page uint64
	// sequence is the checkpoint the next publication of this volume names. Two
	// publications may never name one checkpoint: the pager shares a resident
	// page by the identity its volume gives it, so bytes published twice under
	// one name are two contents the host holds one page for.
	sequence uint64
	// onPublish runs before a checkpoint's page lands, which lets a test hold a
	// publication open and observe the pages the checkpoint still owns.
	onPublish func()
}

func newKernelBacking(object byte, size int) *kernelBacking {
	return newPagedKernelBacking(object, size, hugePageSize)
}

// newPagedKernelBacking is a volume published in the named page, which is what
// a RAM pager's 4 KiB tests attach.
func newPagedKernelBacking(object byte, size int, page uint64) *kernelBacking {
	return &kernelBacking{data: make([]byte, size), owner: fmt.Sprintf("kernel-%d", object), page: page, sequence: 2,
		source:  control.Ref{VM: fmt.Sprintf("checkpoint-%d", object), Sequence: 1},
		private: map[uint64]bool{}, holes: map[uint64]bool{}}
}

// hole records that this volume holds no object for a page, as a publication
// does for one that reads as all zeroes. A test that starts a volume with
// zeroes in it says so here, because the pager maps such a page rather than
// reading it.
func (b *kernelBacking) hole(page uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holes[page] = true
}

// PageSize is the page this volume is published in. A pager refuses a volume
// whose page is not its own, so every one of these states it.
func (b *kernelBacking) PageSize() uint64 {
	if b.page == 0 {
		return hugePageSize
	}
	return b.page
}
func (b *kernelBacking) Size() uint64 { return uint64(len(b.data)) }
func (b *kernelBacking) Load(_ context.Context, off uint64, dst []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy(dst, b.data[off:])
	return nil
}
func (b *kernelBacking) Locate(_ context.Context, off, length uint64) ([]control.Extent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	size := b.PageSize()
	var extents []control.Extent
	for cursor := off; cursor < off+length; cursor += size {
		id := control.Identity{Ref: b.source, Volume: "v", Page: cursor / size}
		switch {
		case b.private[cursor/size]:
			id.Ref = control.Ref{VM: b.owner, Sequence: b.sequence}
		case b.zero || b.holes[cursor/size]:
			id = control.Identity{Zero: true}
		}
		next := control.Extent{Offset: cursor, Length: size, Identity: id}
		if n := len(extents); n > 0 && extents[n-1].Identity == next.Identity && extents[n-1].Offset+extents[n-1].Length == next.Offset {
			extents[n-1].Length += next.Length
			continue
		}
		extents = append(extents, next)
	}
	return extents, nil
}
func (b *kernelBacking) Verify(context.Context) error {
	if b.fenced.Load() {
		return errInjected
	}
	return nil
}

// checkpoint is what a checkpoint does to a memory region: it seals, reads every
// sealed page out of the pages the guest is running on, installs them and
// retires the checkpoint.
func (b *kernelBacking) checkpoint(ctx context.Context, r *vmmemory.MemoryRegion) error {
	if err := r.Seal(ctx); err != nil {
		return err
	}
	published, err := b.publish(ctx, r.Checkpoint())
	return errors.Join(err, r.Checkpoint().Retire(ctx, published))
}

// publish installs one sealed checkpoint's pages, reporting whether the
// checkpoint that would hold them was selected.
func (b *kernelBacking) publish(ctx context.Context, ckpt *vmmemory.MemoryRegionCheckpoint) (bool, error) {
	size := b.PageSize()
	page := make([]byte, size)
	for _, number := range ckpt.DirtyPages() {
		if b.onPublish != nil {
			b.onPublish()
		}
		if err := ckpt.ReadDirty(ctx, number, page); err != nil {
			return false, err
		}
		b.mu.Lock()
		if b.fenced.Load() {
			b.mu.Unlock()
			return false, errInjected
		}
		copy(b.data[number*size:], page)
		// A page of zeroes is published as a hole, which is what makes a guest
		// that frees memory leave one behind for the next fault to map.
		if bytes.Equal(page, make([]byte, size)) {
			b.holes[number] = true
			delete(b.private, number)
		} else {
			delete(b.holes, number)
			b.private[number] = true
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	b.source = control.Ref{VM: b.owner, Sequence: b.sequence}
	b.sequence++
	clear(b.private)
	b.zero = false
	b.mu.Unlock()
	return true, nil
}

// nativeProcess is one real client process. Each of its memory regions is its own
// managed-memory session over its own socket, as a VMM's RAM and each of its
// PMEM devices are: memory region 0 is PMEM and memory region 1 is RAM.
type nativeProcess struct {
	t           testing.TB
	cmd         *exec.Cmd
	input       io.WriteCloser
	lines       chan string
	done        chan error
	connections [2]*vmmemory.Connection
	bases       [2]uint64
	backing     []*kernelBacking
	reapOnce    sync.Once
}

// reap stops the client process and waits for it, so the pipes the test process
// held to it are closed. It is safe to call more than once: a test that tears a
// process down early and the fixture's cleanup both call it.
func (p *nativeProcess) reap() {
	p.reapOnce.Do(func() {
		_ = p.input.Close()
		_ = p.cmd.Process.Kill()
		<-p.done
	})
}

// memory region is the memory of one of the process's memory regions.
func (p *nativeProcess) memoryRegion(index int) *vmmemory.MemoryRegion {
	return p.connections[index].MemoryRegion().Memory
}

// populate maps every session's already resident identities, as a restore does.
func (p *nativeProcess) populate(ctx context.Context) error {
	for _, c := range p.connections {
		if err := c.Populate(ctx); err != nil {
			return err
		}
	}
	return nil
}

// waitFailure reports the first session of the process to fail.
func (p *nativeProcess) waitFailure(ctx context.Context) error {
	failures := make(chan error, len(p.connections))
	for _, c := range p.connections {
		go func() { failures <- c.Wait(ctx) }()
	}
	return <-failures
}

// seal takes one memory region's checkpoint over the control protocol, exactly as a
// coordinated capture does: the guest process asks and the pager answers in
// page-table time.
func (p *nativeProcess) seal(memoryRegion int) {
	p.t.Helper()
	p.request(fmt.Sprintf("seal %d", memoryRegion), "sealed")
}

// checkpoint is one memory region's whole checkpoint: the wire seal, the publication
// of the sealed pages into its backing, and the retirement that makes them
// clean.
func (p *nativeProcess) checkpoint(memoryRegion int) {
	p.t.Helper()
	p.seal(memoryRegion)
	if memoryRegion >= len(p.backing) {
		p.t.Fatalf("memory region %d has no kernel backing to publish into", memoryRegion)
	}
	r := p.memoryRegion(memoryRegion)
	published, err := p.backing[memoryRegion].publish(p.t.Context(), r.Checkpoint())
	if err != nil {
		p.t.Fatalf("publishing memory region %d: %v", memoryRegion, err)
	}
	if err := r.Checkpoint().Retire(p.t.Context(), published); err != nil {
		p.t.Fatalf("retiring memory region %d: %v", memoryRegion, err)
	}
}

func kernelHost(t *testing.T, slots, pages int) *vmmemory.Host {
	return kernelHostBudget(t, slots, pages, pages)
}

func kernelHostBudget(t *testing.T, slots, pages, dirty int) *vmmemory.Host {
	return kernelHostPaged(t, hugePageSize, slots, pages, dirty)
}

// kernelHostPaged is a host whose pager page is the one named, a whole number
// of host pages; the others run at the host page.
func kernelHostPaged(t *testing.T, page, slots, pages, dirty int) *vmmemory.Host {
	return kernelHostConfigured(t, vmmemory.Config{PageSize: uint64(page),
		ResidentPages: slots, LogicalPages: pages, DirtyPages: dirty})
}

// kernelHostConfigured is a host exactly as configured over a Linux arena. The
// arena is HugeTLB, so a configuration that names no page takes the one it can
// be made of.
func kernelHostConfigured(t testing.TB, cfg vmmemory.Config) *vmmemory.Host {
	t.Helper()
	h, _ := kernelHostArena(t, cfg)
	return h
}

// kernelHostArena is kernelHostConfigured with the arena it runs on, for a test
// that counts the memory the arena holds.
func kernelHostArena(t testing.TB, cfg vmmemory.Config) (*vmmemory.Host, *vmmemory.LinuxArena) {
	t.Helper()
	if cfg.Arena == vmmemory.ArenaShared {
		cfg.Arena = suiteArena
	}
	return kernelHostArenaIn(t, cfg)
}

// kernelHostArenaIn is kernelHostArena in the arena mode cfg names, whatever
// mode the suite runs in, for a test about one mode.
func kernelHostArenaIn(t testing.TB, cfg vmmemory.Config) (*vmmemory.Host, *vmmemory.LinuxArena) {
	t.Helper()
	if cfg.PageSize == 0 {
		cfg.PageSize = hugePageSize
	}
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("run scripts/test-vm-memory-lima.sh for Linux/KVM qualification")
	}
	a, err := vmmemory.NewLinuxArena(cfg.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	h, err := vmmemory.New(t.Context(), testresource.New(), cfg, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	return h, a
}

// zeroKernelBackings are two memory regions' volumes that are holes throughout.
func zeroKernelBackings(size int) []*kernelBacking {
	var backings []*kernelBacking
	for object := range byte(2) {
		b := newKernelBacking(object, size)
		b.owner = fmt.Sprintf("zero-%d", object)
		b.zero = true
		backings = append(backings, b)
	}
	return backings
}

func kernelStats(t *testing.T, h *vmmemory.Host) vmmemory.Stats {
	t.Helper()
	s, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// requireKernelBytes requires a backing to hold exactly want.
func requireKernelBytes(t *testing.T, b *kernelBacking, want []byte) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !bytes.Equal(b.data, want) {
		t.Fatal("the volume does not hold exactly the stores")
	}
}

// A store into a zero page owns no page and fences nothing, so through real
// UFFD it is one mapping command, one remap and no revoke, whether a read had
// zero-mapped the page or it is a hole never touched, and it reads nothing from
// the volume. Four threads reading the bytes around the store, in the faulting
// page and across its run, see zeros before, during and after the fault that
// replaces their zero mapping.
func TestManagedPagerStoreIntoZeroPageIsOneMappingCommand(t *testing.T) {
	const pages = 32
	size := hugePageSize
	for _, ahead := range []int{1, 8} {
		t.Run(fmt.Sprintf("writeAhead=%d", ahead), func(t *testing.T) {
			h := kernelHostConfigured(t, vmmemory.Config{ResidentPages: 4 * pages, LogicalPages: 4 * pages,
				DirtyPages: 4 * pages, ReadAheadPages: 8, WriteAheadPages: ahead})
			backings := zeroKernelBackings(pages * size)
			p := startNative(t, h, pages, backings[0], backings[1])
			require := func(what string, before, after vmmemory.Stats) {
				t.Helper()
				maps, revokes := after.Mapping.Count-before.Mapping.Count, after.Revoke.Count-before.Revoke.Count
				remaps, loads := after.RemapEvents-before.RemapEvents, after.Load.Count-before.Load.Count
				if maps != 1 || revokes != 0 || remaps != 1 || loads != 0 {
					t.Fatalf("%s issued %d mapping commands, %d revokes and %d remaps and read the volume %d times; want 1, 0, 1, 0",
						what, maps, revokes, remaps, loads)
				}
				if got := after.WriteAheadPages - before.WriteAheadPages; got != uint64(ahead-1) {
					t.Fatalf("%s wrote ahead %d pages, want %d", what, got, ahead-1)
				}
			}

			p.request("read 1 0 1", "data 00")
			before := kernelStats(t, h)
			p.request(fmt.Sprintf("racefill 1 0 1 91 1 %d", 8*size-1), "raced")
			require("a store into a zero-mapped page", before, kernelStats(t, h))
			p.request("read 1 0 2", "data 5b00")

			before = kernelStats(t, h)
			p.request(fmt.Sprintf("fill 0 %d 1 92", 5*size), "filled")
			after := kernelStats(t, h)
			require("a store into an untouched hole", before, after)
			if faults := after.Faults - before.Faults; faults != 1 {
				t.Fatalf("a store into an untouched hole took %d faults, want 1", faults)
			}
			p.request(fmt.Sprintf("read 0 %d 1", 5*size), "data 5c")

			p.checkpoint(0)
			p.checkpoint(1)
			want := make([]byte, pages*size)
			want[5*size] = 92
			requireKernelBytes(t, backings[0], want)
			want[5*size], want[0] = 0, 91
			requireKernelBytes(t, backings[1], want)
		})
	}
}

// A guest filling fresh memory in order takes one fault per write-ahead run
// through real UFFD: each fault maps its whole run writable in one command, its
// pages allocated without a byte written and zeroed by the kernel as they are
// installed, and the stores into the rest of the run never fault. A pager page
// spans the underlying host pages while retaining one managed fault unit.
func TestManagedPagerSequentialStoresIntoFreshMemoryFaultOncePerRun(t *testing.T) {
	host := hugePageSize
	for _, hugePageSize := range []int{host} {
		t.Run(fmt.Sprintf("%dKiB", hugePageSize>>10), func(t *testing.T) {
			const pages, ahead = 64, 8
			per := hugePageSize / host
			h := kernelHostConfigured(t, vmmemory.Config{ResidentPages: 2 * pages, LogicalPages: 4 * pages,
				DirtyPages: 4 * pages, ReadAheadPages: 8, WriteAheadPages: ahead})
			backings := zeroKernelBackings(pages * hugePageSize)
			p := startNativeWithConfig(t, h, pages*per, vmmemory.ConnectionConfig{QueuePages: pages,
				CommandTimeout: 5 * time.Second, VerifyInterval: 50 * time.Millisecond}, backings[0], backings[1])
			before := kernelStats(t, h)
			p.request(fmt.Sprintf("fill 1 0 %d 7", pages*hugePageSize), "filled")
			after := kernelStats(t, h)
			const runs = pages / ahead
			faults, maps := after.Faults-before.Faults, after.Mapping.Count-before.Mapping.Count
			revokes, remaps := after.Revoke.Count-before.Revoke.Count, after.RemapEvents-before.RemapEvents
			if faults != runs || maps != runs || revokes != 0 || remaps != runs {
				t.Fatalf("filling %d fresh pages took %d faults, %d mapping commands, %d revokes and %d remaps; want %d, %d, 0, %d",
					pages, faults, maps, revokes, remaps, runs, runs, runs)
			}
			if ahead, loads := after.WriteAheadPages-before.WriteAheadPages, after.Load.Count-before.Load.Count; ahead != pages-runs || loads != 0 {
				t.Fatalf("the fill wrote ahead %d pages and read the volume %d times, want %d and 0", ahead, loads, pages-runs)
			}
			p.request(fmt.Sprintf("read 1 %d 2", pages*hugePageSize-2), "data 0707")
			p.checkpoint(1)
			requireKernelBytes(t, backings[1], bytes.Repeat([]byte{7}, pages*hugePageSize))
			ckpt := kernelStats(t, h)
			if carried, zeros := ckpt.CheckpointPages-after.CheckpointPages, ckpt.WriteAheadZeroPages; carried != uint64(pages) || zeros != 0 {
				t.Fatalf("the checkpoint carried %d pages with %d write-ahead pages still zero, want %d and 0", carried, zeros, pages)
			}
		})
	}
}
func startNative(t testing.TB, h *vmmemory.Host, pages int, provided ...vmmemory.Backing) *nativeProcess {
	return startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: pages * 2, CommandTimeout: 5 * time.Second, VerifyInterval: 50 * time.Millisecond}, provided...)
}
func startNativeWithConfig(t testing.TB, h *vmmemory.Host, pages int, config vmmemory.ConnectionConfig, provided ...vmmemory.Backing) *nativeProcess {
	t.Helper()
	return startNativeIn(t, h, "", pages, config, provided...)
}

// clientOptions tune how a client process attaches. A test that plays a
// compromised VMM interposes a proxy on one memory region's control socket.
type clientOptions struct {
	// proxy wraps the socket the pager is given for one memory region. It
	// receives the memory region's index and the socket the client connected,
	// and returns the socket to hand the pager instead, which may forward the
	// two and misbehave between them. A nil result leaves the socket as it is.
	proxy func(region int, client *net.UnixConn) *net.UnixConn
}

type clientOption func(*clientOptions)

// withProxy interposes a proxy on the client's control sockets.
func withProxy(proxy func(region int, client *net.UnixConn) *net.UnixConn) clientOption {
	return func(o *clientOptions) { o.proxy = proxy }
}

// startNativeIn starts a client process whose VM belongs to tenant, empty for
// none.
func startNativeIn(t testing.TB, h *vmmemory.Host, tenant string, pages int, config vmmemory.ConnectionConfig, provided ...vmmemory.Backing) *nativeProcess {
	return startNativeOptions(t, h, tenant, pages, config, clientOptions{}, provided...)
}

// startNativeOptions is startNativeIn with the options a proxied client needs.
func startNativeOptions(t testing.TB, h *vmmemory.Host, tenant string, pages int, config vmmemory.ConnectionConfig, opts clientOptions, provided ...vmmemory.Backing) *nativeProcess {
	t.Helper()
	if len(provided) != 0 && len(provided) != 2 {
		t.Fatal("two memory region backings are required")
	}
	dir := t.TempDir()
	paths := [2]string{filepath.Join(dir, "pmem.sock"), filepath.Join(dir, "ram.sock")}
	var listeners [2]*net.UnixListener
	for i, path := range paths {
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		_ = l.SetDeadline(time.Now().Add(10 * time.Second))
		listeners[i] = l
	}
	p := &nativeProcess{t: t, lines: make(chan string, 16), done: make(chan error, 1)}
	p.cmd = exec.Command(os.Getenv("SPROUTFS_VM_MEMORY_CLIENT"), paths[0], paths[1],
		strconv.Itoa(pages), strconv.FormatUint(h.PageSize(), 10))
	p.cmd.Stderr = os.Stderr
	input, err := p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.input = input
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
		close(p.lines)
	}()
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		p.reap()
		for _, c := range p.connections {
			if c == nil {
				continue
			}
			if err := c.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}
	})
	// The client connects its sessions in memory region order, so each listener is
	// accepted in turn.
	for i, kind := range []vmmemory.MemoryRegionKind{vmmemory.Pmem, vmmemory.Ram} {
		var b vmmemory.Backing
		if len(provided) != 0 {
			b = provided[i]
			// A provided kernel backing is still the one a checkpoint publishes into.
			if kernel, ok := provided[i].(*kernelBacking); ok {
				p.backing = append(p.backing, kernel)
			}
		} else {
			kernel := newKernelBacking(byte(i+1), pages*hugePageSize)
			for j := range kernel.data {
				kernel.data[j] = byte(1 + i*32 + j/hugePageSize)
			}
			p.backing = append(p.backing, kernel)
			b = kernel
		}
		socket, err := listeners[i].AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		pagerSide := socket
		if opts.proxy != nil {
			if wrapped := opts.proxy(i, socket); wrapped != nil {
				pagerSide = wrapped
			}
		}
		p.connections[i], err = vmmemory.Connect(t.Context(), h, pagerSide,
			vmmemory.MemoryRegionBacking{Kind: kind, Backing: b, Tenant: tenant}, config)
		if err != nil {
			t.Fatal(err)
		}
	}
	var pid int
	var size0, size1 uint64
	line := p.line()
	if _, err := fmt.Sscanf(line, "ready %d %d %d %d %d", &pid, &p.bases[0], &size0, &p.bases[1], &size1); err != nil || pid != p.cmd.Process.Pid {
		t.Fatalf("ready: %q %v", line, err)
	}
	return p
}

func TestKVMVolumeCheckpointWithSpillRequiresAuthority(t *testing.T) {
	h := kernelHost(t, 3, 16)
	c := newPagerCluster(t)
	vm, err := c.manager.Create(t.Context(), "vm", []volume.VolumeSpec{
		{Name: "pmem0", Size: uint64(8 * hugePageSize), PageSize: hugePageSize},
		{Name: "ram0", Size: uint64(8 * hugePageSize), PageSize: hugePageSize},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close(context.Background()) })
	volumes := []*volume.Volume{vm.Volume("pmem0"), vm.Volume("ram0")}
	p := startNativeWithConfig(t, h, 8, vmmemory.ConnectionConfig{QueuePages: 16, CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour}, volumes[0], volumes[1])
	for memoryRegion := range 2 {
		for page := range 8 {
			value := 91 + memoryRegion + page
			p.request(fmt.Sprintf("kvmwrite %d %d %d", memoryRegion, page*hugePageSize, value), fmt.Sprintf("kvm %d", value))
		}
	}
	// One checkpoint of the whole VM: every memory region seals over the control protocol
	// and the checkpoint publishes their pages together.
	sources := make(map[string]volume.DirtySource, len(volumes))
	for memoryRegion, v := range volumes {
		p.seal(memoryRegion)
		sources[v.Name()] = p.memoryRegion(memoryRegion).Checkpoint()
	}
	ckpt, err := vm.Snapshot(t.Context(), volume.Prepared([]byte("kvm"), sources), volume.Terms{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	for memoryRegion, v := range volumes {
		for page := range 8 {
			var data [1]byte
			if err := v.Read(t.Context(), uint64(page*hugePageSize), data[:]); err != nil || data[0] != byte(91+memoryRegion+page) {
				t.Fatalf("the checkpoint lost a KVM store: %v %d", err, data[0])
			}
		}
	}
	stats, err := h.Stats(t.Context())
	if err != nil || stats.Spills == 0 || stats.DirtyPages != 0 {
		t.Fatalf("spill/checkpoint coverage: %+v %v", stats, err)
	}
	// A subsequent real KVM store cannot become durable when the VM's control
	// record is unavailable: the publication fails and the memory region keeps its
	// pages, which the next checkpoint takes again.
	p.request("kvmwrite 0 0 199", "kvm 199")
	memoryRegion := p.memoryRegion(0)
	p.seal(0)
	c.runtime.ObjectStore().Fail()
	publication, err := vm.Snapshot(t.Context(), volume.Prepared(nil, map[string]volume.DirtySource{"pmem0": memoryRegion.Checkpoint()}), volume.Terms{})
	if err != nil {
		t.Fatal(err)
	}
	err = publication.Wait(t.Context())
	c.runtime.ObjectStore().Recover()
	if !errors.Is(err, platform.ErrUnavailable) {
		t.Fatalf("a checkpoint without the object store = %v, want ErrUnavailable", err)
	}
	if err := memoryRegion.Verify(t.Context()); err != nil {
		t.Fatalf("a failed publication left the KVM memory region ineligible to run: %v", err)
	}
	if s, err := h.Stats(t.Context()); err != nil || s.DirtyPages == 0 {
		t.Fatalf("the abandoned checkpoint kept %d dirty pages, want the store it could not publish: %v", s.DirtyPages, err)
	}
}

func (p *nativeProcess) line() string {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			p.t.Fatalf("memory process output closed: %v", p.waitFailure(ctx))
		}
		return line
	case <-time.After(10 * time.Second):
		p.t.Fatal("memory process stalled")
		return ""
	}
}
func (p *nativeProcess) request(command, want string) {
	p.t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		p.t.Fatal(err)
	}
	if got := p.line(); got != want {
		p.t.Fatalf("%s: got %q, want %q", command, got, want)
	}
}

// ask is request for a caller that is not the test goroutine, which may not
// fail a test itself: it reports what went wrong instead. One process serves
// one caller at a time, as its single pair of pipes requires.
func (p *nativeProcess) ask(command, want string) error {
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	select {
	case line, ok := <-p.lines:
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return fmt.Errorf("%s: the memory process closed its output: %w", command, p.waitFailure(ctx))
		}
		if line != want {
			return fmt.Errorf("%s: got %q, want %q", command, line, want)
		}
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("%s: the memory process stalled", command)
	}
}
func (p *nativeProcess) pfn(memoryRegion int) uint64 {
	p.t.Helper()
	f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", p.cmd.Process.Pid))
	if err != nil {
		p.t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.ReadAt(b[:], int64(p.bases[memoryRegion]/uint64(os.Getpagesize())*8)); err != nil {
		p.t.Fatal(err)
	}
	entry := binary.LittleEndian.Uint64(b[:])
	pfn := entry & ((1 << 55) - 1)
	if entry>>63 == 0 || pfn == 0 {
		p.t.Fatal("physical page evidence inaccessible")
	}
	return pfn
}

func TestManagedPagerKVMSharingBoundedReclaimCheckpoint(t *testing.T) {
	h := kernelHost(t, 3, 32)
	a, b := startNative(t, h, 8), startNative(t, h, 8)
	for memoryRegion := range 2 {
		value := 1 + memoryRegion*32
		a.request(fmt.Sprintf("kvmread %d 0", memoryRegion), fmt.Sprintf("kvm %d", value))
		b.request(fmt.Sprintf("kvmread %d 0", memoryRegion), fmt.Sprintf("kvm %d", value))
		if a.pfn(memoryRegion) != b.pfn(memoryRegion) {
			t.Fatal("clean KVM pages not physically shared")
		}
		a.request(fmt.Sprintf("kvmwrite %d 0 91", memoryRegion), "kvm 91")
		b.request(fmt.Sprintf("kvmread %d 0", memoryRegion), fmt.Sprintf("kvm %d", value))
		for page := 1; page < 8; page++ {
			b.request(fmt.Sprintf("kvmread %d %d", memoryRegion, page*hugePageSize), fmt.Sprintf("kvm %d", value+page))
		}
		a.request(fmt.Sprintf("kvmread %d 0", memoryRegion), "kvm 91")
		a.checkpoint(memoryRegion)
		var data [1]byte
		if err := a.backing[memoryRegion].Load(t.Context(), 0, data[:]); err != nil || data[0] != 91 {
			t.Fatalf("the checkpoint published %d: %v", data[0], err)
		}
		a.request(fmt.Sprintf("kvmwrite %d 0 92", memoryRegion), "kvm 92")
	}
	s, err := h.Stats(t.Context())
	if err != nil || s.ResidentPages > 3 {
		t.Fatalf("resident budget: %+v %v", s, err)
	}
}

func TestManagedPagerAuthorityFailureStopsClient(t *testing.T) {
	h := kernelHost(t, 2, 2)
	p := startNative(t, h, 1)
	p.request("kvmread 0 0", "kvm 1")
	p.backing[0].fenced.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := p.connections[0].Wait(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("authority failure not observed: %v", err)
	}
	select {
	case line, ok := <-p.lines:
		if ok {
			t.Fatalf("expected terminated memory client, got %q", line)
		}
	case <-ctx.Done():
		t.Fatal("client did not stop on control failure")
	}
}

func TestManagedPagerPopulateAvoidsKVMFirstTouchFaults(t *testing.T) {
	const pages = 8
	h := kernelHost(t, 2*pages, 6*pages)
	a := startNative(t, h, pages)
	touch := func(p *nativeProcess) {
		for memoryRegion := range 2 {
			for page := range pages {
				p.request(fmt.Sprintf("kvmread %d %d", memoryRegion, page*hugePageSize), fmt.Sprintf("kvm %d", 1+memoryRegion*32+page))
			}
		}
	}
	touch(a)
	for range 2 {
		beforeAttach, err := h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		b := startNative(t, h, pages)
		before, err := h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if before.Loads != beforeAttach.Loads || before.MappedPages-beforeAttach.MappedPages != 2*pages || before.Mappings-beforeAttach.Mappings > 2 {
			t.Fatalf("attach did not eagerly batch all resident pages: before=%+v after=%+v", beforeAttach, before)
		}
		touch(b)
		after, err := h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.Loads != before.Loads || after.Faults != before.Faults {
			t.Fatalf("eager first touch: loads=%d faults=%d; want zero", after.Loads-before.Loads, after.Faults-before.Faults)
		}
	}
}

func TestManagedPagerEagerZeroMappingsRemainCopyOnWrite(t *testing.T) {
	const pages = 32
	h := kernelHost(t, 1, 4*pages)
	backings := func() []vmmemory.Backing {
		result := make([]vmmemory.Backing, 2)
		for i := range result {
			b := newKernelBacking(0, pages*hugePageSize)
			b.zero = true
			result[i] = b
		}
		return result
	}
	a := startNative(t, h, pages, backings()...)
	a.request("kvmread 0 0", "kvm 0")
	before, _ := h.Stats(t.Context())
	if before.ResidentPages != 0 || before.Loads != 0 {
		t.Fatalf("sparse mappings used arena or backing data: %+v", before)
	}
	b := startNative(t, h, pages, backings()...)
	attached, _ := h.Stats(t.Context())
	if attached.MappedPages-before.MappedPages != 2*pages || attached.Mappings-before.Mappings != 2 || attached.MappingRuns-before.MappingRuns != 2 {
		t.Fatalf("zero attach was not two contiguous mappings: before=%+v after=%+v", before, attached)
	}
	for memoryRegion := range 2 {
		for page := range pages {
			b.request(fmt.Sprintf("kvmread %d %d", memoryRegion, page*hugePageSize), "kvm 0")
		}
	}
	after, _ := h.Stats(t.Context())
	if after.Faults != attached.Faults || after.Loads != attached.Loads {
		t.Fatalf("eager zeros faulted or loaded: before=%+v after=%+v", attached, after)
	}
	b.request("kvmwrite 0 0 91", "kvm 91")
	a.request("kvmread 0 0", "kvm 0")
	b.request("kvmread 0 0", "kvm 91")
	// Two private pages compete for the only slot. The other process's
	// anonymous zero mappings must remain intact across spill and refault.
	a.request(fmt.Sprintf("kvmwrite 0 %d 92", hugePageSize), "kvm 92")
	b.request("kvmread 0 0", "kvm 91")
	b.request(fmt.Sprintf("kvmread 0 %d", hugePageSize), "kvm 0")
	a.request(fmt.Sprintf("kvmread 0 %d", hugePageSize), "kvm 92")
	a.request("kvmread 0 0", "kvm 0")
}

func TestManagedPagerReadAheadKeepsZerosAndDataSeparate(t *testing.T) {
	h := kernelHostConfigured(t, vmmemory.Config{ResidentPages: 4, LogicalPages: 8, DirtyPages: 8, ReadAheadPages: 4})
	var backing []vmmemory.Backing
	for range 2 {
		b := newKernelBacking(0, 4*hugePageSize)
		b.zero = true
		b.private[1] = true
		b.private[2] = true
		b.data[hugePageSize] = 9
		b.data[2*hugePageSize] = 10
		backing = append(backing, b)
	}
	p := startNative(t, h, 4, backing...)
	p.request(fmt.Sprintf("kvmread 0 %d", hugePageSize), "kvm 9")
	before, _ := h.Stats(t.Context())
	if before.Loads != 1 || before.LoadedPages != 2 {
		t.Fatalf("read-ahead did not load just the data run: %+v", before)
	}
	p.request("kvmread 0 0", "kvm 0")
	p.request(fmt.Sprintf("kvmread 0 %d", 2*hugePageSize), "kvm 10")
	p.request(fmt.Sprintf("kvmread 0 %d", 3*hugePageSize), "kvm 0")
	after, _ := h.Stats(t.Context())
	if after.Faults != before.Faults || after.Loads != before.Loads {
		t.Fatalf("read-ahead left a page fault: before=%+v after=%+v", before, after)
	}
}

// A seal protects whole runs of consecutive pages in place, one range command
// each, whatever pages those pages hold and however many of the client's
// mappings they span. The guest here dirties a run backwards, so its pages
// descend and every page of the run is a separate mapping: one protection must
// still cover all of it, the guest must keep reading those very pages without
// refaulting, and its next store must trap and come back on a copy.
func TestManagedPagerSealProtectsARunSpanningSeveralMappings(t *testing.T) {
	const pages = 4
	h := kernelHost(t, 8, 16)
	p := startNative(t, h, pages)
	size := hugePageSize
	for page := pages - 1; page >= 0; page-- {
		p.request(fmt.Sprintf("fill 1 %d 1 %d", page*size, 70+page), "filled")
	}
	sealed := p.pfn(1)
	memoryRegion := p.memoryRegion(1)
	before, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRegion.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Protections-before.Protections != 1 || after.ProtectedPages-before.ProtectedPages != pages {
		t.Fatalf("the seal issued %d protections over %d pages, want 1 over %d",
			after.Protections-before.Protections, after.ProtectedPages-before.ProtectedPages, pages)
	}
	if after.Mappings != before.Mappings {
		t.Fatalf("the seal issued %d mapping commands, want none", after.Mappings-before.Mappings)
	}
	// Every page of the run still reads its own sealed bytes through the memory
	// it already had, and no fault was needed to get them.
	for page := range pages {
		p.request(fmt.Sprintf("read 1 %d 1", page*size), fmt.Sprintf("data %02x", 70+page))
	}
	if got, err := h.Stats(t.Context()); err != nil || got.Faults != after.Faults {
		t.Fatalf("reading the protected run took %d faults: %v", got.Faults-after.Faults, err)
	}
	if got := p.pfn(1); got != sealed {
		t.Fatalf("the seal moved the guest to physical page %d, want the %d it already had", got, sealed)
	}
	p.request("fill 1 0 1 91", "filled")
	p.request("read 1 0 1", "data 5b")
	if got := p.pfn(1); got == sealed {
		t.Fatal("a store into a protected page kept the physical page the checkpoint holds")
	}
	published, err := p.backing[1].publish(t.Context(), memoryRegion.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRegion.Checkpoint().Retire(t.Context(), published); err != nil {
		t.Fatal(err)
	}
	for page := range pages {
		var data [1]byte
		if err := p.backing[1].Load(t.Context(), uint64(page*size), data[:]); err != nil || data[0] != byte(70+page) {
			t.Fatalf("the checkpoint published %d for page %d, want %d: %v", data[0], page, 70+page, err)
		}
	}
	for page := range pages {
		p.request(fmt.Sprintf("fill 1 %d 1 %d", page*size, 80+page), "filled")
		p.request(fmt.Sprintf("read 1 %d 1", page*size), fmt.Sprintf("data %02x", 80+page))
	}
}

// A 2 MiB pager page is one unit end to end through real UFFD: a fault on
// any host page maps the whole pager page, a store copies it, a seal ingests it,
// and a spilled page comes back with every host subpage intact.
func TestManagedPagerHugePagesFaultCopySealAndSpill(t *testing.T) {
	const pages = 4
	host := os.Getpagesize()
	per := hugePageSize / host
	// Two slots: a third resident page evicts.
	h := kernelHostPaged(t, hugePageSize, 2, 4*pages, 4*pages)
	backings := make([]vmmemory.Backing, 2)
	var ram *kernelBacking
	for i := range backings {
		ram = newKernelBacking(byte(i+1), pages*hugePageSize)
		for j := range ram.data {
			ram.data[j] = byte(1 + j/host) // every host page tells itself apart
		}
		backings[i] = ram
	}
	p := startNativeWithConfig(t, h, pages, vmmemory.ConnectionConfig{QueuePages: pages,
		CommandTimeout: 5 * time.Second, VerifyInterval: 50 * time.Millisecond}, backings...)
	memoryRegion := p.memoryRegion(1)
	at := func(page, sub int) int { return page*hugePageSize + sub*host }
	want := func(page, sub int) byte { return byte(1 + page*per + sub) }
	read := func(page, sub int, value byte) {
		t.Helper()
		p.request(fmt.Sprintf("read 1 %d 1", at(page, sub)), fmt.Sprintf("data %02x", value))
	}
	stats := func() vmmemory.Stats {
		t.Helper()
		s, err := h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A read of one host page maps all of its pager page.
	before := stats()
	read(1, 2, want(1, 2))
	for sub := range per {
		read(1, sub, want(1, sub))
	}
	after := stats()
	if faults, loaded := after.Faults-before.Faults, after.LoadedPages-before.LoadedPages; faults != 1 || loaded != 1 {
		t.Fatalf("reading one pager page took %d faults and loaded %d pages, want 1 and 1", faults, loaded)
	}

	// A store into one host page copies the whole pager page, and stores into
	// its other host pages need no further fault.
	before = stats()
	p.request(fmt.Sprintf("fill 1 %d 1 91", at(1, 2)), "filled")
	p.request(fmt.Sprintf("fill 1 %d 1 92", at(1, 3)), "filled")
	stored := make(map[int]byte, per)
	for sub := range per {
		stored[sub] = want(1, sub)
	}
	stored[2], stored[3] = 91, 92
	for sub := range per {
		read(1, sub, stored[sub])
	}
	after = stats()
	if faults, copies := after.Faults-before.Faults, after.CopyOnWrites-before.CopyOnWrites; faults != 1 || copies != 1 {
		t.Fatalf("storing into one pager page took %d faults and %d copies, want 1 and 1", faults, copies)
	}

	// A seal takes the whole pager page, and the checkpoint publishes all of it.
	before = stats()
	if err := memoryRegion.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := memoryRegion.Checkpoint().DirtyPages(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("the seal checkpoint %v, want the one pager page the guest stored into", got)
	}
	published, err := ram.publish(t.Context(), memoryRegion.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRegion.Checkpoint().Retire(t.Context(), published); err != nil {
		t.Fatal(err)
	}
	after = stats()
	if ckpt := after.CheckpointPages - before.CheckpointPages; ckpt != 1 {
		t.Fatalf("the seal checkpoint %d pages, want 1", ckpt)
	}
	for sub := range per {
		var data [1]byte
		if err := ram.Load(t.Context(), uint64(at(1, sub)), data[:]); err != nil || data[0] != stored[sub] {
			t.Fatalf("the checkpoint published %d for host page %d, want %d: %v", data[0], sub, stored[sub], err)
		}
	}

	// Dirty it again and read three other pager pages through two slots: the
	// private page spills whole and refaults whole.
	p.request(fmt.Sprintf("fill 1 %d 1 93", at(1, 0)), "filled")
	stored[0] = 93
	before = stats()
	for _, page := range []int{0, 2, 3} {
		read(page, 1, want(page, 1))
	}
	for sub := range per {
		read(1, sub, stored[sub])
	}
	after = stats()
	if spills, bytes, refaults := after.Spills-before.Spills, after.SpillWriteBytes-before.SpillWriteBytes, after.SpillRefaults-before.SpillRefaults; spills != 1 || bytes != hugePageSize || refaults != 1 {
		t.Fatalf("eviction spilled %d pages as %d bytes and refaulted %d, want 1 page of %d bytes and 1", spills, bytes, refaults, hugePageSize)
	}
	if err := ram.checkpoint(t.Context(), memoryRegion); err != nil {
		t.Fatal(err)
	}
	for sub := range per {
		var data [1]byte
		if err := ram.Load(t.Context(), uint64(at(1, sub)), data[:]); err != nil || data[0] != stored[sub] {
			t.Fatalf("the checkpoint published %d for host page %d, want %d: %v", data[0], sub, stored[sub], err)
		}
	}
}

// Sealing takes write access away from a private page in place, which is the
// only part of a capture the guest's pause pays for. On a real UFFD the guest
// must keep reading that page, trap on its next store, come back on a page of
// its own, and leave the checkpoint's bytes as they were; releasing the capture
// must make the page writable again.
func TestManagedPagerSealProtectsAndCopiesOnWriteOnUFFD(t *testing.T) {
	h := kernelHost(t, 4, 8)
	p := startNative(t, h, 2)
	p.request("fill 1 0 1 70", "filled")
	p.request("read 1 0 1", "data 46")
	sealed := p.pfn(1)
	memoryRegion := p.memoryRegion(1)
	if err := memoryRegion.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The write-protected mapping still serves reads from the checkpoint's page.
	p.request("read 1 0 1", "data 46")
	if got := p.pfn(1); got != sealed {
		t.Fatalf("the seal moved the guest to physical page %d, want the %d it already had", got, sealed)
	}
	// A store traps on that protection and comes back on a private copy.
	p.request("fill 1 0 1 71", "filled")
	p.request("read 1 0 1", "data 47")
	if got := p.pfn(1); got == sealed {
		t.Fatal("a store into a sealed page kept the physical page the checkpoint holds")
	}
	published, err := p.backing[1].publish(t.Context(), memoryRegion.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRegion.Checkpoint().Retire(t.Context(), published); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if err := p.backing[1].Load(t.Context(), 0, data[:]); err != nil || data[0] != 70 {
		t.Fatalf("the checkpoint published %d, want the 70 it sealed: %v", data[0], err)
	}
	// The retired page takes stores again without any further capture.
	p.request("fill 1 0 1 72", "filled")
	p.request("read 1 0 1", "data 48")
	p.checkpoint(1)
	if err := p.backing[1].Load(t.Context(), 0, data[:]); err != nil || data[0] != 72 {
		t.Fatalf("the checkpoint after the capture published %d, want 72: %v", data[0], err)
	}
	s, err := h.Stats(t.Context())
	if err != nil || s.CheckpointPages != 2 {
		t.Fatalf("the two captures counted %d sealed pages, want 2: %v", s.CheckpointPages, err)
	}
}
