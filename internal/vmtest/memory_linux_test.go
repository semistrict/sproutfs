//go:build linux && (amd64 || arm64)

package vmtest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmwire"
)

type lockedBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedBuffer) String() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }

type process struct {
	t      *testing.T
	pager  *pager
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  chan string
	done   chan struct{}
	err    error
	stderr *lockedBuffer
	// clients is one session per region, as a VMM's RAM and each of its PMEM
	// devices are their own session.
	clients [2]*pagerClient
	base    [2]uint64
}

func startClient(t *testing.T, pager *pager, pages int) *process {
	t.Helper()
	dir := t.TempDir()
	paths := [2]string{filepath.Join(dir, "pmem.sock"), filepath.Join(dir, "ram.sock")}
	var listeners [2]*net.UnixListener
	for i, path := range paths {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		listener.SetDeadline(time.Now().Add(10 * time.Second))
		listeners[i] = listener
	}
	cmd := exec.Command(os.Getenv("SPROUTFS_VM_MEMORY_CLIENT"), paths[0], paths[1], strconv.Itoa(pages))
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &process{t: t, pager: pager, cmd: cmd, input: input, lines: make(chan string, 16), done: make(chan struct{}), stderr: &lockedBuffer{}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
		close(p.lines)
	}()
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { input.Close(); cmd.Process.Kill(); <-p.done })
	// The client connects its sessions in region order.
	for i := range listeners {
		p.clients[i], err = pager.accept(listeners[i], i)
		if err != nil {
			t.Fatalf("attach: %v; child: %s", err, p.stderr.String())
		}
	}
	line := p.line()
	var pid int
	var size0, size1 uint64
	if _, err := fmt.Sscanf(line, "ready %d %d %d %d %d", &pid, &p.base[0], &size0, &p.base[1], &size1); err != nil || pid != cmd.Process.Pid || size0 != uint64(pages*pager.pageSize) || size1 != size0 {
		t.Fatalf("invalid child ready message %q: %v", line, err)
	}
	return p
}

func (p *process) send(command string) {
	p.t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		p.t.Fatalf("client input: %v (%s)", err, p.stderr.String())
	}
}

func (p *process) line() string {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.pager.mu.Lock()
			err := p.pager.err
			p.pager.mu.Unlock()
			p.t.Fatalf("client output closed: %s; pager: %v", p.stderr.String(), err)
		}
		return line
	case <-time.After(15 * time.Second):
		p.t.Fatalf("client stalled: %s", p.stderr.String())
		return ""
	}
}

func (p *process) expect(want string) {
	p.t.Helper()
	if got := p.line(); got != want {
		p.t.Fatalf("client got %q, want %q (%s)", got, want, p.stderr.String())
	}
}

func (p *process) read(kind string, region, offset, length int, want byte) {
	p.t.Helper()
	p.send(fmt.Sprintf("%s %d %d %d", kind, region, offset, length))
	p.expect("data " + hex.EncodeToString(bytes.Repeat([]byte{want}, length)))
}

func (p *process) fill(region, offset, length int, value byte) {
	p.t.Helper()
	p.send(fmt.Sprintf("fill %d %d %d %d", region, offset, length, value))
	p.expect("filled")
}

func pfn(t *testing.T, p *process, region, offset int) uint64 {
	t.Helper()
	f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", p.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.ReadAt(b[:], int64((p.base[region]+uint64(offset))/uint64(os.Getpagesize())*8)); err != nil {
		t.Fatal(err)
	}
	entry := binary.LittleEndian.Uint64(b[:])
	frame := entry & ((1 << 55) - 1)
	if entry>>63 != 1 || frame == 0 {
		t.Fatalf("cannot establish physical residency/sharing: pagemap=%#x (run the dedicated test as root)", entry)
	}
	return frame
}

func allocated(t *testing.T, p *pager) int64 {
	t.Helper()
	info, err := p.arena.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

func checkPager(t *testing.T, p *pager) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		t.Fatal(p.err)
	}
}

func TestSharedCOWSpillRefault(t *testing.T) {
	p := newPager(t, 16)
	a, b := startClient(t, p, 1), startClient(t, p, 1)
	for region := range 2 {
		original, changed, sibling := byte(17+region*17), byte(161+region), byte(201+region)
		a.read("read", region, 0, p.pageSize, original)
		b.read("read", region, 0, p.pageSize, original)
		if pfn(t, a, region, 0) != pfn(t, b, region, 0) {
			t.Fatal("unchanged data is not physically shared")
		}
		a.fill(region, 0, p.pageSize, changed)
		a.read("read", region, 0, p.pageSize, changed)
		b.read("read", region, 0, p.pageSize, original)
		if pfn(t, a, region, 0) == pfn(t, b, region, 0) {
			t.Fatal("private write still shares its physical frame")
		}
		p.mu.Lock()
		oldSlot := a.clients[region].aliases[0].page.slot
		p.mu.Unlock()
		before := allocated(t, p)
		if err := p.evict(a.clients[region], 0); err != nil {
			t.Fatal(err)
		}
		if after := allocated(t, p); before-after < int64(p.pageSize) {
			t.Fatalf("eviction did not release backing memory: before=%d after=%d", before, after)
		}
		// Reuse the actual freed slot for different contents before refault.
		b.fill(region, 0, p.pageSize, sibling)
		p.mu.Lock()
		reused := b.clients[region].aliases[0].page.slot
		p.mu.Unlock()
		if reused != oldSlot {
			t.Fatalf("test did not exercise slot reuse: old=%d new=%d", oldSlot, reused)
		}
		a.read("read", region, 0, p.pageSize, changed)
		b.read("read", region, 0, p.pageSize, sibling)
		// Write after swap-in, then evict again: old spill contents must not win.
		a.fill(region, 0, p.pageSize, changed+1)
		if err := p.evict(a.clients[region], 0); err != nil {
			t.Fatal(err)
		}
		a.read("read", region, 0, p.pageSize, changed+1)
	}
	checkPager(t, p)
}

func TestSharedEvictionRevokesEveryAlias(t *testing.T) {
	p := newPager(t, 8)
	a, b := startClient(t, p, 1), startClient(t, p, 1)
	a.read("read", 0, 0, 16, 17)
	b.read("read", 0, 0, 16, 17)
	before := allocated(t, p)
	if err := p.evict(a.clients[0], 0); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	am, bm, writes := a.clients[0].aliases[0].mapped, b.clients[0].aliases[0].mapped, p.spillWrites
	p.mu.Unlock()
	if am || bm || writes != 0 {
		t.Fatalf("shared eviction: mapped=%v,%v spill writes=%d", am, bm, writes)
	}
	if after := allocated(t, p); before-after < int64(p.pageSize) {
		t.Fatal("shared page remained resident")
	}
	a.read("read", 0, 0, p.pageSize, 17)
	b.read("read", 0, 0, p.pageSize, 17)
	if pfn(t, a, 0, 0) != pfn(t, b, 0, 0) {
		t.Fatal("refault lost physical sharing")
	}
	checkPager(t, p)
}

func TestFirstAccessWriteDoesNotMutateSibling(t *testing.T) {
	p := newPager(t, 16)
	a, b := startClient(t, p, 1), startClient(t, p, 1)
	for r := range 2 {
		a.fill(r, 0, p.pageSize, 99)
		b.read("read", r, 0, p.pageSize, byte(17+r*17))
		a.read("read", r, 0, p.pageSize, 99)
	}
	checkPager(t, p)
}

func TestSpillFailureKeepsRecoverableRAM(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	a.fill(1, 0, p.pageSize, 91)
	before := allocated(t, p)
	p.mu.Lock()
	p.failSpill = true
	p.mu.Unlock()
	if err := p.evict(a.clients[1], 0); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected spill failure, got %v", err)
	}
	if allocated(t, p) != before {
		t.Fatal("failed spill released the only current copy")
	}
	a.read("read", 1, 0, p.pageSize, 91)
	p.mu.Lock()
	p.failSpill = false
	p.mu.Unlock()
	if err := p.evict(a.clients[1], 0); err != nil {
		t.Fatal(err)
	}
	a.read("read", 1, 0, p.pageSize, 91)
	checkPager(t, p)
}

func TestConcurrentAccessDuringEviction(t *testing.T) {
	p := newPager(t, 8)
	a, b := startClient(t, p, 1), startClient(t, p, 1)
	a.read("read", 0, 0, 1, 17)
	b.read("read", 0, 0, 1, 17)
	a.send(fmt.Sprintf("scan 0 0 %d 17 2000000", p.pageSize))
	b.send(fmt.Sprintf("scan 0 0 %d 17 2000000", p.pageSize))
	for range 100 {
		if err := p.evict(a.clients[0], 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	a.expect("scanned")
	b.expect("scanned")
	a.read("read", 0, 0, p.pageSize, 17)
	b.read("read", 0, 0, p.pageSize, 17)
	checkPager(t, p)
}

func TestKernelAccessAsFirstFault(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	for r := range 2 {
		a.read("gup", r, 0, p.pageSize, byte(17+r*17))
		if err := p.evict(a.clients[r], 0); err != nil {
			t.Fatal(err)
		}
		a.read("gup", r, 0, p.pageSize, byte(17+r*17))
	}
	checkPager(t, p)
}

func TestWritesDuringEviction(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	a.fill(1, 0, 8, 0)
	a.send("start-counter 1 0 8")
	a.expect("counter-started")
	for range 100 {
		if err := p.evict(a.clients[1], 0); err != nil {
			t.Fatal(err)
		}
		a.send("counter-progress")
		if line := a.line(); !strings.HasPrefix(line, "counter-value ") {
			t.Fatal(line)
		}
	}
	a.send("stop-counter")
	var count uint64
	if _, err := fmt.Sscanf(a.line(), "counter-stopped %d", &count); err != nil || count == 0 {
		t.Fatalf("counter did not run: count=%d err=%v", count, err)
	}
	if err := p.evict(a.clients[1], 0); err != nil {
		t.Fatal(err)
	}
	var want [8]byte
	binary.LittleEndian.PutUint64(want[:], count)
	a.send("read 1 0 8")
	a.expect("data " + hex.EncodeToString(want[:]))
	checkPager(t, p)
}

func TestFragmentedMappingsAndReuse(t *testing.T) {
	const pages = 32
	p := newPager(t, pages*6)
	a, b := startClient(t, p, pages), startClient(t, p, pages)
	var expected [2][pages]byte
	for r := range 2 {
		for i := range pages {
			expected[r][i] = byte(17 + r*17 + i)
		}
	}
	for round := range 3 {
		for r := range 2 {
			for step := range pages {
				i := step * 13 % pages // visit nonadjacent backing offsets
				if i%3 == round {
					expected[r][i] = byte(128 + round)
					a.fill(r, i*p.pageSize, p.pageSize, expected[r][i])
				}
				if err := p.evict(a.clients[r], i); err != nil {
					t.Fatal(err)
				}
				a.read("read", r, i*p.pageSize, 16, expected[r][i])
				a.read("read", r, (i+1)*p.pageSize-16, 16, expected[r][i])
				b.read("read", r, i*p.pageSize, 16, byte(17+r*17+i))
			}
		}
	}
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", a.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fragmented workload: %d logical pages, %d process VMAs (including runtime), %d remaps", pages*2, bytes.Count(maps, []byte{'\n'}), p.remaps.Load())
	checkPager(t, p)
}

func TestKVMSharingCOWAndRefault(t *testing.T) {
	p := newPager(t, 16)
	a, b := startClient(t, p, 1), startClient(t, p, 1)
	for r := range 2 {
		original := 17 + r*17
		for _, client := range []*process{a, b} {
			client.send(fmt.Sprintf("kvmread %d 0", r))
			client.expect(fmt.Sprintf("kvm %d", original))
		}
		if pfn(t, a, r, 0) != pfn(t, b, r, 0) {
			t.Fatal("KVM first reads did not preserve sharing")
		}
		// Leave the same KVM slots and vCPU alive through shared eviction.
		if err := p.evict(a.clients[r], 0); err != nil {
			t.Fatal(err)
		}
		for _, client := range []*process{a, b} {
			client.send(fmt.Sprintf("kvmread %d 0", r))
			client.expect(fmt.Sprintf("kvm %d", original))
		}
		a.send(fmt.Sprintf("kvmwrite %d 0 97", r))
		a.expect("kvm 97")
		b.send(fmt.Sprintf("kvmread %d 0", r))
		b.expect(fmt.Sprintf("kvm %d", original))
		if pfn(t, a, r, 0) == pfn(t, b, r, 0) {
			t.Fatal("KVM store did not make a private frame")
		}
		if err := p.evict(a.clients[r], 0); err != nil {
			t.Fatal(err)
		}
		a.send(fmt.Sprintf("kvmread %d 0", r))
		a.expect("kvm 97")
		// Guest first access after eviction can itself be a store.
		if err := p.evict(a.clients[r], 0); err != nil {
			t.Fatal(err)
		}
		a.send(fmt.Sprintf("kvmwrite %d 1 98", r))
		a.expect("kvm 98")
		a.send(fmt.Sprintf("kvmread %d 0", r))
		a.expect("kvm 97")
	}
	checkPager(t, p)
}

func TestMappingCommandValidation(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	a.read("read", 0, 0, 1, 17)
	p.mu.Lock()
	alias := a.clients[0].aliases[0]
	last := vmwire.Frame{Kind: vmwire.MapRange, ID: p.sequence, Length: uint64(p.pageSize), Backing: uint64(alias.page.slot * p.pageSize), Generation: alias.generation, Flags: shared}
	before := p.remaps.Load()
	response, err := p.command(a.clients[0], last)
	if err != nil || response.Flags != 0 {
		p.mu.Unlock()
		t.Fatalf("identical retry rejected: %+v %v", response, err)
	}
	if p.remaps.Load() != before {
		p.mu.Unlock()
		t.Fatal("retry repeated a mapping operation")
	}
	for _, mutate := range []func(*vmwire.Frame){
		func(f *vmwire.Frame) { f.Generation-- },
		func(f *vmwire.Frame) { f.Offset = 1 },
		func(f *vmwire.Frame) { f.Length = ^uint64(0) - uint64(p.pageSize) + 1 },
		func(f *vmwire.Frame) { f.Backing = ^uint64(0) - uint64(p.pageSize) + 1 },
	} {
		p.sequence++
		command := last
		command.ID = p.sequence
		command.Generation++
		mutate(&command)
		response, err := p.command(a.clients[0], command)
		if err != nil || response.Flags == 0 {
			p.mu.Unlock()
			t.Fatalf("invalid command accepted: %+v response=%+v err=%v", command, response, err)
		}
	}
	p.mu.Unlock()
	a.read("read", 0, 0, p.pageSize, 17)
	if err := p.evict(a.clients[0], 0); err != nil {
		t.Fatal(err)
	}
	a.read("read", 0, 0, p.pageSize, 17)
	checkPager(t, p)
}

func TestOrderlyShutdown(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	a.read("read", 0, 0, 1, 17)
	a.send("quit")
	a.expect("quiescent")
	p.mu.Lock()
	for _, c := range a.clients {
		p.sequence++
		if _, err := p.command(c, vmwire.Frame{Kind: vmwire.Stop, ID: p.sequence}); err != nil {
			p.mu.Unlock()
			t.Fatal(err)
		}
	}
	p.mu.Unlock()
	a.expect("bye")
	select {
	case <-a.done:
		if a.err != nil {
			t.Fatal(a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not stop")
	}
}

func TestControlLossTerminatesAdapter(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 1)
	a.clients[0].conn.Close()
	select {
	case <-a.done:
		if a.err == nil {
			t.Fatal("adapter treated unexpected control loss as successful shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter continued after losing control")
	}
}

// A range write-protect is what a seal costs. One ioctl covers a run of pages
// that are several separate mappings in the client, and it must take write
// access away without moving a frame or dropping a page table: reads go on
// through the same frames with no fault at all, and only the next store to each
// page traps, on the frame it already had.
func TestRangeWriteProtectSpansSeveralMappings(t *testing.T) {
	const pages = 4
	p := newPager(t, pages*2)
	a := startClient(t, p, pages)
	// Dirty the run backwards, so ascending addresses take descending arena
	// offsets and no two of these mappings can be one VMA.
	for page := pages - 1; page >= 0; page-- {
		a.fill(0, page*p.pageSize, p.pageSize, byte(100+page))
	}
	var frames [pages]uint64
	for page := range pages {
		frames[page] = pfn(t, a, 0, page*p.pageSize)
		if page > 0 && frames[page] == frames[page-1] {
			t.Fatalf("pages %d and %d share a frame", page-1, page)
		}
	}
	before := p.faults.Load()
	if err := vmwire.ProtectRange(a.clients[0].uffd.Fd(), a.base[0], uint64(pages*p.pageSize)); err != nil {
		t.Fatalf("range write-protect across %d mappings: %v", pages, err)
	}
	for page := range pages {
		a.read("read", 0, page*p.pageSize, 16, byte(100+page))
		if got := pfn(t, a, 0, page*p.pageSize); got != frames[page] {
			t.Fatalf("page %d moved from frame %d to %d", page, frames[page], got)
		}
	}
	if got := p.faults.Load(); got != before {
		t.Fatalf("reading the protected run took %d faults, want none", got-before)
	}
	for page := range pages {
		a.fill(0, page*p.pageSize, p.pageSize, byte(200+page))
		a.read("read", 0, page*p.pageSize, 16, byte(200+page))
		if got := pfn(t, a, 0, page*p.pageSize); got != frames[page] {
			t.Fatalf("the store into page %d moved it from frame %d to %d", page, frames[page], got)
		}
	}
	if got := p.faults.Load(); got != before+pages {
		t.Fatalf("the stores into the protected run took %d faults, want %d", got-before, pages)
	}
	checkPager(t, p)
}

// Exercise generation boundaries through the real Rust command processor.
// The client stays quiescent; these revokes have no pager alias side effects.
func TestMappingGenerationsPreserveFragmentedHistory(t *testing.T) {
	p := newPager(t, 8)
	a := startClient(t, p, 256)
	p.mu.Lock()
	defer p.mu.Unlock()
	generations := make([]uint64, 256)
	send := func(start, end int, next uint64, valid bool) {
		t.Helper()
		p.sequence++
		command := vmwire.Frame{Kind: vmwire.Revoke, ID: p.sequence, Offset: uint64(start * p.pageSize), Length: uint64((end - start) * p.pageSize), Generation: next}
		response, err := p.command(a.clients[0], command)
		if err != nil || (response.Flags == 0) != valid {
			t.Fatalf("range [%d,%d) generation %d valid=%t: %+v %v", start, end, next, valid, response, err)
		}
		if valid {
			for page := start; page < end; page++ {
				generations[page] = next
			}
		}
	}
	for i := range 512 {
		start := (i * 73) % 256
		end := min(256, start+1+i%19)
		next := generations[start] + 1
		valid := true
		for page := start; page < end; page++ {
			valid = valid && generations[page]+1 == next
		}
		send(start, end, next, valid)
		if !valid {
			send(start, start+1, next, true)
		}
		send(start, start+1, next, false) // retained history must reject an older generation
	}
	// Bring all pages to one generation, which also joins adjacent ranges.
	var target uint64
	for _, generation := range generations {
		target = max(target, generation)
	}
	for page := range 256 {
		for generations[page] < target {
			send(page, page+1, generations[page]+1, true)
		}
	}
	send(0, 256, target+1, true)
	send(127, 129, target+1, false)
	send(0, 256, target+2, true)
}
