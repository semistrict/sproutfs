//go:build linux && (amd64 || arm64)

package vmtest

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/semistrict/sproutfs/internal/vmwire"
)

// The tests in this file hand the real client a read-only file, as an
// isolated arena does with the pages it shares. The client maps such a file
// private, installs the pager's own pages in it read-only, and write-protects
// it before any address of it is exposed. So a store traps to the pager and
// never copies into memory the pager does not see.

// pool is what the host's 2 MiB pool holds free and reserved.
type pool struct{ free, reserved int }

func poolNow(t *testing.T) pool {
	t.Helper()
	free, reserved := hugePages(t)
	return pool{free, reserved}
}

// ownPFN maps one slot of a file the pager holds, reads the byte the page is
// filled with, and reports the physical page behind it in this process.
func ownPFN(t *testing.T, p *pager, pg *page, want byte) uint64 {
	t.Helper()
	mapped, err := syscall.Mmap(int(p.files[pg.file].file.Fd()), int64(pg.slot*p.pageSize), p.pageSize,
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(mapped)
	if mapped[0] != want {
		t.Fatalf("the pager's page holds %d, want %d", mapped[0], want)
	}
	entry := pagemap(t, "self", uint64(uintptr(unsafe.Pointer(&mapped[0]))))
	if entry>>63 != 1 || entry&((1<<55)-1) == 0 {
		t.Fatalf("cannot read this process's own physical page: pagemap=%#x (run the dedicated test as root)", entry)
	}
	return entry & ((1 << 55) - 1)
}

// mappingsOf is the client's mappings of the file named name, in address
// order: the offset each maps from, and whether it is private ('p') or shared
// ('s').
func mappingsOf(t *testing.T, c *process, name string) []string {
	t.Helper()
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", c.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(maps), "\n") {
		if !strings.Contains(line, "/memfd:"+name+" ") {
			continue
		}
		fields := strings.Fields(line)
		offset, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, fmt.Sprintf("%d %c", offset, fields[1][3]))
	}
	return found
}

// descriptorsOf is how many descriptors the client holds of the file named
// name.
func descriptorsOf(t *testing.T, c *process, name string) int {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", c.cmd.Process.Pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(dir + "/" + entry.Name())
		if err == nil && target == "/memfd:"+name+" (deleted)" {
			count++
		}
	}
	return count
}

// A page of a read-only file is the pager's own page in the client: pagemap
// shows the page the pager holds. The client maps it private, and that
// mapping reserves nothing from the HugeTLB pool. A store into it, from a
// thread or from KVM, traps to the pager, which gives the guest a private copy
// of its own: the pool loses the one page the pager allocated for it and no
// other, and the read-only file keeps its bytes.
func TestAReadOnlyFilePageIsThePagersOwn(t *testing.T) {
	p := newArenaPager(t, 8, isolatedArena)
	a := startClient(t, p, 1)
	for memoryRegion := range 2 {
		original := byte(17 + memoryRegion*17)
		before := poolNow(t)
		a.read("read", memoryRegion, 0, p.pageSize, original)
		if after := poolNow(t); after != (pool{before.free - 1, before.reserved}) {
			t.Fatalf("reading a page of the read-only file took the pool from %+v to %+v, want one page"+
				" allocated and none reserved", before, after)
		}
		p.mu.Lock()
		pg := a.clients[memoryRegion].aliases[0].page
		p.mu.Unlock()
		if pg.file != vmwire.SharedFile {
			t.Fatalf("the page read is in file %d, want the read-only file %d", pg.file, vmwire.SharedFile)
		}
		// Each region's first page is the next slot of the read-only file.
		want := []string{fmt.Sprintf("%d p", pg.slot*p.pageSize)}
		if got := mappingsOf(t, a, "sproutfs-page-shared"); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the client maps the read-only file as %q, want %q", got, want)
		}
		own := ownPFN(t, p, pg, original)
		if got := pfn(t, a, memoryRegion, 0); got != own {
			t.Fatalf("the client reads physical page %d, the pager holds %d", got, own)
		}

		stored := byte(99 + memoryRegion)
		before = poolNow(t)
		if memoryRegion == 0 {
			a.fill(memoryRegion, 0, p.pageSize, stored)
		} else {
			a.send(fmt.Sprintf("kvmwrite %d 0 %d", memoryRegion, stored))
			a.expect(fmt.Sprintf("kvm %d", stored))
		}
		if after := poolNow(t); after != (pool{before.free - 1, before.reserved}) {
			t.Fatalf("a store into the read-only file's page took the pool from %+v to %+v, want the"+
				" pager's one private page", before, after)
		}
		a.read("read", memoryRegion, 0, 1, stored)
		if got := pfn(t, a, memoryRegion, 0); got == own {
			t.Fatal("a store left the client on the read-only file's page")
		}
		p.mu.Lock()
		kept, err := p.read(pg)
		p.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(kept, bytes.Repeat([]byte{original}, p.pageSize)) {
			t.Fatal("a store reached the read-only file")
		}
		if got := mappingsOf(t, a, "sproutfs-page-shared"); len(got) != 0 {
			t.Fatalf("the client still maps the read-only file after the store: %q", got)
		}
	}
	checkPager(t, p)
}

// The client takes from a read-only file only what the file allows: a MAP that
// would write it, names a file it was never given, or reaches past the file's
// length is refused and changes nothing. A DROP_FILE, sent once the file's
// mappings are revoked, closes the client's descriptor, and a MAP of it is
// refused from then on.
func TestAClientTakesFromAFileOnlyWhatItAllows(t *testing.T) {
	const slots = 4
	p := newArenaPager(t, slots, isolatedArena)
	a := startClient(t, p, 1)
	a.read("read", 0, 0, 1, 17)
	c := a.clients[0]
	p.mu.Lock()
	alias := c.aliases[0]
	valid := vmwire.Frame{Kind: vmwire.MapRange, Offset: 0, Length: uint64(p.pageSize),
		Backing: uint64(alias.page.slot * p.pageSize), Generation: alias.generation + 1,
		Flags: vmwire.MapFlags(vmwire.SharedFile, true)}
	refuse := func(name string, command vmwire.Frame) {
		t.Helper()
		p.sequence++
		command.ID = p.sequence
		response, err := p.command(c, command)
		if err != nil {
			p.mu.Unlock()
			t.Fatalf("%s: %v", name, err)
		}
		if response.Flags != uint64(syscall.EINVAL) {
			p.mu.Unlock()
			t.Fatalf("%s was answered with %d, want EINVAL", name, response.Flags)
		}
	}
	refuse("a writable map of the read-only file", func() vmwire.Frame {
		f := valid
		f.Flags = vmwire.MapFlags(vmwire.SharedFile, false)
		return f
	}())
	refuse("a map of a file never given", func() vmwire.Frame {
		f := valid
		f.Flags = vmwire.MapFlags(2, true)
		return f
	}())
	refuse("a map past the read-only file", func() vmwire.Frame {
		f := valid
		f.Backing = uint64(slots * p.pageSize)
		return f
	}())
	p.mu.Unlock()
	// Nothing moved: the page reads as it did, from the same page.
	a.read("read", 0, 0, p.pageSize, 17)

	if err := p.evict(c, 0); err != nil {
		t.Fatal(err)
	}
	if got := descriptorsOf(t, a, "sproutfs-page-shared"); got != 2 {
		t.Fatalf("the client holds %d descriptors of the read-only file, want one per session", got)
	}
	p.mu.Lock()
	for _, session := range a.clients {
		if err := vmwire.Write(session.conn, vmwire.DropFileFrame(vmwire.SharedFile)); err != nil {
			p.mu.Unlock()
			t.Fatal(err)
		}
	}
	// The drop takes no answer, so a command after it is what says the
	// client has read it.
	valid.Generation = alias.generation + 1
	refuse("a map of a dropped file", valid)
	p.sequence++
	other := valid
	other.ID = p.sequence
	other.Generation = a.clients[1].aliases[0].generation + 1
	response, err := p.command(a.clients[1], other)
	p.mu.Unlock()
	if err != nil || response.Flags != uint64(syscall.EINVAL) {
		t.Fatalf("a map of a dropped file on the other session: %+v %v", response, err)
	}
	if got := descriptorsOf(t, a, "sproutfs-page-shared"); got != 0 {
		t.Fatalf("the client holds %d descriptors of the dropped file, want none", got)
	}
	checkPager(t, p)
}
