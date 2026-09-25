//go:build linux && (amd64 || arm64)

package vmtest

// The isolated arena of plans/arena-by-trust-2026-09-25.md gives a VMM the
// files other memory regions map as read-only descriptors. The kernel will not
// register a shared mapping of a read-only file with userfaultfd, so the VMM
// maps them MAP_PRIVATE. The tests in this file prove on the running kernel
// what that design needs, before any of it is built:
//
//   - A read-only descriptor refuses every way to write the file, and cannot
//     be made writable.
//   - Another user cannot reopen it for writing once the file's mode is 0600.
//   - A private mapping of it registers for missing, minor and write-protect
//     faults, and each fault arrives with its own flags.
//   - UFFDIO_CONTINUE installs the pager's own page, write-protected. pagemap
//     shows one physical page in the pager and in the VMM.
//   - A store traps on the write protection, from a thread and from a vCPU. A
//     store the pager lets through copies into anonymous memory, and the file
//     keeps its bytes.
//   - A private HugeTLB mapping with MAP_NORESERVE reserves no pool pages.
//   - How many mappings adjacent runs leave, when each is mapped and installed
//     by its own command.
//
// The test process plays the pager. A copy of the test binary plays the VMM
// (TestReadOnlyVMM). It maps runs as the client does, and sends its UFFD to
// the pager, which resolves every fault.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

const (
	hugePage = checkpoint.PageSize2MiB
	// The flags of a UFFD_EVENT_PAGEFAULT message.
	faultWrite = 1
	faultWP    = 2
	faultMinor = 4
	// The bits of a pagemap entry.
	pagemapPresent = 1 << 63
	pagemapFile    = 1 << 61
	pagemapUffdWP  = 1 << 57
	pagemapFrame   = 1<<55 - 1
)

// nobody is the user a jailed VMM runs as.
var nobody = &syscall.Credential{Uid: 65534, Gid: 65534}

// requireQualification skips a test outside the Lima and GCE qualification,
// which runs it as root with KVM and a HugeTLB pool.
func requireQualification(t *testing.T) {
	t.Helper()
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("run scripts/test-vm-memory-lima.sh for real Linux memory tests")
	}
}

// forEachPage runs test at RAM's 4 KiB page over an ordinary memfd and at
// PMEM's 2 MiB page over a HugeTLB memfd.
func forEachPage(t *testing.T, test func(t *testing.T, pageSize int)) {
	for _, pageSize := range []int{checkpoint.PageSize4KiB, hugePage} {
		t.Run(fmt.Sprintf("%dKiB", pageSize>>10), func(t *testing.T) { test(t, pageSize) })
	}
}

// readOnlyFile is the pager's side of one file: the memfd it writes, the
// read-only reopen a VMM receives, and the pager's own mapping of each page it
// filled.
type readOnlyFile struct {
	t        *testing.T
	pageSize int
	file     *os.File
	readOnly *os.File
	// created is the mode the memfd was created with.
	created os.FileMode
	views   map[int][]byte
}

// newReadOnlyFile makes a file as the pager would: a memfd of the page's kind,
// sealed against shrinking, with mode 0600, and reopened read-only through
// /proc/self/fd.
func newReadOnlyFile(t *testing.T, pageSize, pages int) *readOnlyFile {
	t.Helper()
	requireQualification(t)
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING
	if pageSize == hugePage {
		flags |= unix.MFD_HUGETLB | unix.MFD_HUGE_2MB
	}
	fd, err := unix.MemfdCreate("sproutfs-read-only", flags)
	if err != nil {
		t.Fatal(err)
	}
	f := &readOnlyFile{t: t, pageSize: pageSize, file: os.NewFile(uintptr(fd), "memfd"), views: make(map[int][]byte)}
	t.Cleanup(func() { f.file.Close() })
	if err := f.file.Truncate(int64(pageSize * pages)); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK); err != nil {
		t.Fatal(err)
	}
	info, err := f.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	f.created = info.Mode().Perm()
	if err := f.file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if f.readOnly, err = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), os.O_RDONLY, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.readOnly.Close() })
	return f
}

// fill allocates page and fills it with value, through a mapping of the
// pager's own that it keeps.
func (f *readOnlyFile) fill(page int, value byte) {
	f.t.Helper()
	offset := int64(page * f.pageSize)
	for {
		err := unix.Fallocate(int(f.file.Fd()), 0, offset, int64(f.pageSize))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			f.t.Fatal(err)
		}
		break
	}
	view, err := unix.Mmap(int(f.file.Fd()), offset, f.pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { unix.Munmap(view) })
	copy(view, bytes.Repeat([]byte{value}, f.pageSize))
	f.views[page] = view
}

// holds fails the test unless every byte of page is value.
func (f *readOnlyFile) holds(page int, value byte) {
	f.t.Helper()
	if !bytes.Equal(f.views[page], bytes.Repeat([]byte{value}, f.pageSize)) {
		f.t.Fatalf("the file's page %d changed from %d", page, value)
	}
}

// pagemap is the pagemap entry of the host page at address in process pid.
func pagemap(t *testing.T, pid string, address uint64) uint64 {
	t.Helper()
	f, err := os.Open("/proc/" + pid + "/pagemap")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.ReadAt(b[:], int64(address/uint64(os.Getpagesize())*8)); err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

// hugePages is the HugeTLB pool's free and reserved pages.
func hugePages(t *testing.T) (free, reserved int) {
	t.Helper()
	info, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(info)) {
		fields := strings.Fields(line)
		switch fields[0] {
		case "HugePages_Free:":
			free, err = strconv.Atoi(fields[1])
		case "HugePages_Rsvd:":
			reserved, err = strconv.Atoi(fields[1])
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return free, reserved
}

// wantErr fails the test unless err is want. A nil want is success.
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: %v, want %v", what, err, want)
	}
}

type uffdFault struct {
	address, flags uint64
}

func (f uffdFault) String() string { return fmt.Sprintf("%#x flags %d", f.address, f.flags) }

// readOnlyVMM is the pager's side of one VMM process.
type readOnlyVMM struct {
	t      *testing.T
	file   *readOnlyFile
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  chan string
	stderr *lockedBuffer
	pid    string
	base   uint64
	socket int
	uffd   int
	faults chan uffdFault
	remaps atomic.Int64
	mu     sync.Mutex
	err    error
}

// vmmExecutable is this test binary. A process that runs as another user gets
// a copy it can read.
func vmmExecutable(t *testing.T, owner *syscall.Credential) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return executable
	}
	dir, err := os.MkdirTemp("", "sproutfs-vmtest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "vmtest.test")
	if err := os.WriteFile(copied, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	return copied
}

// startReadOnlyVMM starts a VMM process with a memory region of pages. It runs
// as owner, or as this process's user when owner is nil. It holds the file's
// read-only descriptor, and its read-write one when readWrite is set.
func startReadOnlyVMM(t *testing.T, f *readOnlyFile, pages int, owner *syscall.Credential, readWrite bool) *readOnlyVMM {
	t.Helper()
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	v := &readOnlyVMM{t: t, file: f, socket: sockets[0], uffd: -1, lines: make(chan string, 16),
		faults: make(chan uffdFault, 64), stderr: &lockedBuffer{}}
	t.Cleanup(func() { unix.Close(v.socket) })
	if err := unix.SetsockoptTimeval(v.socket, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 10}); err != nil {
		t.Fatal(err)
	}
	theirs := os.NewFile(uintptr(sockets[1]), "vmm socket")
	defer theirs.Close()
	var writable *os.File
	if readWrite {
		writable = f.file
	}
	executable := vmmExecutable(t, owner)
	v.cmd = exec.Command(executable, "-test.run=^TestReadOnlyVMM$")
	v.cmd.Dir = filepath.Dir(executable)
	// A preemption signal would make a thread blocked in a fault take it
	// again, and the pager would read a second message for it.
	v.cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d %d", vmmRole, f.pageSize, pages), "GODEBUG=asyncpreemptoff=1")
	v.cmd.ExtraFiles = []*os.File{theirs, f.readOnly, writable}
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Credential: owner}
	v.cmd.Stderr = v.stderr
	if v.input, err = v.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	output, err := v.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			v.lines <- scanner.Text()
		}
		close(v.lines)
	}()
	go func() { v.cmd.Wait(); close(done) }()
	t.Cleanup(func() { v.input.Close(); v.cmd.Process.Kill(); <-done })
	var pid int
	if _, err := fmt.Sscanf(v.line(), "ready %d %d", &pid, &v.base); err != nil || pid != v.cmd.Process.Pid {
		t.Fatalf("invalid ready message: %v (%s)", err, v.stderr.String())
	}
	v.pid = strconv.Itoa(pid)
	return v
}

func (v *readOnlyVMM) line() string {
	v.t.Helper()
	select {
	case line, ok := <-v.lines:
		if !ok {
			v.t.Fatalf("the VMM's output closed: %s; pager: %v", v.stderr.String(), v.failure())
		}
		return line
	case <-time.After(15 * time.Second):
		v.t.Fatalf("the VMM stalled: %s; pager: %v", v.stderr.String(), v.failure())
		return ""
	}
}

func (v *readOnlyVMM) send(command string) {
	v.t.Helper()
	if _, err := fmt.Fprintln(v.input, command); err != nil {
		v.t.Fatalf("VMM input: %v (%s)", err, v.stderr.String())
	}
}

func (v *readOnlyVMM) expect(want string) {
	v.t.Helper()
	if got := v.line(); got != want {
		v.t.Fatalf("the VMM answered %q, want %q (%s)", got, want, v.stderr.String())
	}
}

// do sends command and expects want as its answer.
func (v *readOnlyVMM) do(command, want string) {
	v.t.Helper()
	v.send(command)
	v.expect(want)
}

func (v *readOnlyVMM) failure() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

func (v *readOnlyVMM) fail(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err == nil {
		v.err = err
	}
}

// attach has the VMM open its UFFD and send it to the pager, which reads it
// from then on.
func (v *readOnlyVMM) attach() {
	v.t.Helper()
	v.send("uffd")
	b, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(v.socket, b, oob, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		v.t.Fatalf("receiving the UFFD: %v (%s)", err, v.stderr.String())
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(messages) != 1 {
		v.t.Fatalf("receiving the UFFD: %d messages, %v", len(messages), err)
	}
	fds, err := unix.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 {
		v.t.Fatalf("receiving the UFFD: %d descriptors, %v", len(fds), err)
	}
	v.uffd = fds[0]
	v.expect("uffd")
	var stop atomic.Bool
	done := make(chan struct{})
	go v.readFaults(&stop, done)
	v.t.Cleanup(func() { stop.Store(true); <-done; unix.Close(v.uffd) })
}

// readFaults reads the UFFD until stop is set. It drains REMAP events, which
// the VMM's mremap waits for, and queues every fault.
func (v *readOnlyVMM) readFaults(stop *atomic.Bool, done chan struct{}) {
	defer close(done)
	message := make([]byte, 32)
	poll := []unix.PollFd{{Fd: int32(v.uffd), Events: unix.POLLIN}}
	for !stop.Load() {
		n, err := unix.Read(v.uffd, message)
		if errors.Is(err, unix.EAGAIN) {
			if _, err := unix.Poll(poll, 100); err != nil && !errors.Is(err, unix.EINTR) {
				v.fail(fmt.Errorf("UFFD poll: %w", err))
				return
			}
			continue
		}
		if err != nil || n != len(message) {
			v.fail(fmt.Errorf("UFFD read: n=%d err=%v", n, err))
			return
		}
		switch message[0] {
		case 0x12: // UFFD_EVENT_PAGEFAULT
			v.faults <- uffdFault{address: binary.LittleEndian.Uint64(message[16:]), flags: binary.LittleEndian.Uint64(message[8:])}
		case 0x14: // UFFD_EVENT_REMAP
			v.remaps.Add(1)
		default:
			v.fail(fmt.Errorf("unexpected UFFD event %#x", message[0]))
			return
		}
	}
}

// address is where page of the memory region is in the VMM.
func (v *readOnlyVMM) address(page int) uint64 {
	return v.base + uint64(page*v.file.pageSize)
}

// mapRun maps a run of the file into the memory region and checks that the
// registration offers the ioctls a resident run needs.
func (v *readOnlyVMM) mapRun(mode string, page, filePage, pages int) {
	v.t.Helper()
	v.send(fmt.Sprintf("map %s %d %d %d", mode, page, filePage, pages))
	var ioctls uint64
	if line := v.line(); !strings.HasPrefix(line, "mapped ") {
		v.t.Fatalf("mapping %s run at page %d: %q (%s)", mode, page, line, v.stderr.String())
	} else if _, err := fmt.Sscanf(line, "mapped %d", &ioctls); err != nil {
		v.t.Fatal(err)
	}
	if ioctls&uffdIoctls != uffdIoctls {
		v.t.Fatalf("the registration offers ioctls %#x, want %#x among them", ioctls, uffdIoctls)
	}
}

// expectFault waits for the next fault and requires it to be want.
func (v *readOnlyVMM) expectFault(page int, flags uint64) {
	v.t.Helper()
	want := uffdFault{address: v.address(page), flags: flags}
	select {
	case got := <-v.faults:
		if got != want {
			v.t.Fatalf("fault %v, want %v", got, want)
		}
	case <-time.After(15 * time.Second):
		v.t.Fatalf("no fault, want %v: %s; pager: %v", want, v.stderr.String(), v.failure())
	}
}

// idle fails the test if a fault is queued or the reader failed.
func (v *readOnlyVMM) idle() {
	v.t.Helper()
	select {
	case got := <-v.faults:
		v.t.Fatalf("unexpected fault %v", got)
	default:
	}
	if err := v.failure(); err != nil {
		v.t.Fatal(err)
	}
}

// resolve installs pages from page with UFFDIO_CONTINUE, write-protected, and
// wakes any fault in them, as the pager resolves a shared page.
func (v *readOnlyVMM) resolve(page, pages int) {
	v.t.Helper()
	size := uint64(v.file.pageSize)
	if err := vmwire.Resolve(uintptr(v.uffd), v.address(page), uint64(pages)*size, size, false); err != nil {
		v.t.Fatal(err)
	}
}

// letThrough clears the write protection of page and wakes the store that
// waits on it. The pager of the isolated arena never does this.
func (v *readOnlyVMM) letThrough(page int) {
	v.t.Helper()
	if err := vmwire.IOCtl(uintptr(v.uffd), 6, []uint64{v.address(page), uint64(v.file.pageSize), 0}, true); err != nil {
		v.t.Fatalf("UFFDIO_WRITEPROTECT: %v", err)
	}
}

// ends are the first and last host pages of a page, as offsets in it.
func (v *readOnlyVMM) ends() []uint64 {
	return []uint64{0, uint64(v.file.pageSize - os.Getpagesize())}
}

// shares requires page of the memory region to map the pager's own physical
// page of the file, present and write-protected.
func (v *readOnlyVMM) shares(page int) {
	v.t.Helper()
	for _, offset := range v.ends() {
		mine := pagemap(v.t, v.pid, v.address(page)+offset)
		pager := pagemap(v.t, "self", uint64(address(v.file.views[page]))+offset)
		if got, want := mine&(pagemapPresent|pagemapFile|pagemapUffdWP), uint64(pagemapPresent|pagemapFile|pagemapUffdWP); got != want {
			v.t.Fatalf("page %d at %#x: pagemap %#x, want present, file and write-protected", page, offset, mine)
		}
		if mine&pagemapFrame == 0 || mine&pagemapFrame != pager&pagemapFrame {
			v.t.Fatalf("page %d at %#x: the VMM maps frame %#x and the pager %#x", page, offset, mine&pagemapFrame, pager&pagemapFrame)
		}
	}
}

// ownsCopy requires page of the memory region to map an anonymous copy of its
// own, not the pager's page.
func (v *readOnlyVMM) ownsCopy(page int) {
	v.t.Helper()
	for _, offset := range v.ends() {
		mine := pagemap(v.t, v.pid, v.address(page)+offset)
		pager := pagemap(v.t, "self", uint64(address(v.file.views[page]))+offset)
		if got := mine & (pagemapPresent | pagemapFile | pagemapUffdWP); got != pagemapPresent {
			v.t.Fatalf("page %d at %#x: pagemap %#x, want present, anonymous and writable", page, offset, mine)
		}
		if mine&pagemapFrame == 0 || mine&pagemapFrame == pager&pagemapFrame {
			v.t.Fatalf("page %d at %#x: the VMM maps frame %#x and the pager %#x", page, offset, mine&pagemapFrame, pager&pagemapFrame)
		}
	}
}

// A read-only descriptor refuses a writable shared mapping, mprotect to
// writable, write, fallocate in both modes, ftruncate and new seals. F_SETFL
// ignores an access mode, so the descriptor stays read-only. The pager's own
// descriptor can still seal, so the refusal is the descriptor's and not a
// seal's.
func TestReadOnlyDescriptorRefusesEveryWrite(t *testing.T) {
	forEachPage(t, func(t *testing.T, pageSize int) {
		f := newReadOnlyFile(t, pageSize, 2)
		f.fill(0, 17)
		f.fill(1, 18)
		fd := int(f.readOnly.Fd())
		length := 2 * pageSize
		_, err := unix.Mmap(fd, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		wantErr(t, "a writable shared mapping", err, unix.EACCES)
		view, err := unix.Mmap(fd, 0, length, unix.PROT_READ, unix.MAP_SHARED)
		wantErr(t, "a read-only shared mapping", err, nil)
		wantErr(t, "mprotect to writable", unix.Mprotect(view, unix.PROT_READ|unix.PROT_WRITE), unix.EACCES)
		wantErr(t, "unmapping", unix.Munmap(view), nil)
		_, err = unix.Pwrite(fd, []byte{1}, 0)
		wantErr(t, "write", err, unix.EBADF)
		wantErr(t, "fallocate", unix.Fallocate(fd, 0, 0, int64(pageSize)), unix.EBADF)
		wantErr(t, "punching a hole", unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, int64(pageSize)), unix.EBADF)
		wantErr(t, "ftruncate", unix.Ftruncate(fd, int64(4*pageSize)), unix.EINVAL)
		_, err = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SEAL)
		wantErr(t, "a seal", err, unix.EPERM)
		_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_RDWR)
		wantErr(t, "F_SETFL O_RDWR", err, nil)
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		wantErr(t, "F_GETFL", err, nil)
		if flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatalf("F_SETFL made the descriptor's access mode %#x", flags&unix.O_ACCMODE)
		}
		_, err = unix.Pwrite(fd, []byte{1}, 0)
		wantErr(t, "write after F_SETFL", err, unix.EBADF)
		_, err = unix.FcntlInt(f.file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL)
		wantErr(t, "the pager's seal", err, nil)
		f.holds(0, 17)
		f.holds(1, 18)
	})
}

// A memfd is created with mode 0777, so another user can reopen a read-only
// descriptor of it for writing through /proc/self/fd. Mode 0600 stops that,
// and the other user cannot change the mode back. It never reaches the pager's
// descriptors through /proc/<pid>/fd. A VMM that runs as the file's owner can
// reopen its own file for writing whatever the mode.
func TestReadOnlyFileCannotBeReopenedForWritingByAnotherUser(t *testing.T) {
	forEachPage(t, func(t *testing.T, pageSize int) {
		f := newReadOnlyFile(t, pageSize, 1)
		if f.created != 0o777 {
			t.Fatalf("the memfd was created with mode %#o, want 0777", f.created)
		}
		f.fill(0, 17)
		if err := f.file.Chmod(0o777); err != nil {
			t.Fatal(err)
		}
		v := startReadOnlyVMM(t, f, 1, nobody, false)
		own := fmt.Sprintf("reopen /proc/self/fd/%d", vmmReadOnly)
		v.do(own, "ok")
		v.do(fmt.Sprintf("reopen /proc/%d/fd/%d", os.Getpid(), f.file.Fd()), "refused EACCES")
		if err := f.file.Chmod(0o600); err != nil {
			t.Fatal(err)
		}
		v.do(own, "refused EACCES")
		v.do("fchmod", "refused EPERM")
		v.do(own, "refused EACCES")
		v.do("reopen-own", "ok")
		f.holds(0, 17)
	})
}

// A private mapping of a read-only file registers for missing, minor and
// write-protect faults. A load of a page the file holds is a minor fault, and
// a load of a hole a missing one. UFFDIO_CONTINUE installs the pager's own
// physical page, write-protected. A store traps on the write protection, and a
// store as the first access takes a minor fault and then a write-protect
// fault. A store the pager lets through copies into anonymous memory and
// leaves the file as it was. A shared mapping of the read-only file cannot be
// registered at all.
func TestReadOnlyPrivateMappingTrapsEveryFault(t *testing.T) {
	forEachPage(t, func(t *testing.T, pageSize int) {
		const pages = 4
		f := newReadOnlyFile(t, pageSize, pages)
		for page := range pages - 1 {
			f.fill(page, byte(17+page))
		}
		_, reserved := hugePages(t)
		v := startReadOnlyVMM(t, f, pages+1, nil, false)
		v.attach()
		v.do(fmt.Sprintf("map readonly-shared %d 0 1", pages), "failed UFFDIO_REGISTER EPERM")
		v.mapRun("private", 0, 0, pages)
		if got := v.remaps.Load(); got != 1 {
			t.Fatalf("moving the run into place sent %d REMAP events, want 1", got)
		}

		v.send("load 0")
		v.expectFault(0, faultMinor)
		v.resolve(0, 1)
		v.expect("loaded 17")
		v.shares(0)

		v.send(fmt.Sprintf("load %d", 3*pageSize))
		v.expectFault(3, 0)
		f.fill(3, 20)
		free, _ := hugePages(t)
		v.resolve(3, 1)
		v.expect("loaded 20")
		v.shares(3)
		// Installing the page took no pool page, and mapping the run
		// reserved none.
		if gotFree, gotReserved := hugePages(t); gotFree != free || gotReserved != reserved {
			t.Fatalf("HugeTLB pool: %d free and %d reserved, want %d and %d", gotFree, gotReserved, free, reserved)
		}

		v.send("store 0 99")
		v.expectFault(0, faultWrite|faultWP)
		v.letThrough(0)
		v.expect("stored")
		v.do("load 0", "loaded 99")
		v.ownsCopy(0)
		f.holds(0, 17)

		v.send(fmt.Sprintf("store %d 55", pageSize))
		v.expectFault(1, faultWrite|faultMinor)
		v.resolve(1, 1)
		v.expectFault(1, faultWrite|faultWP)
		v.letThrough(1)
		v.expect("stored")
		v.do(fmt.Sprintf("load %d", pageSize), "loaded 55")
		v.ownsCopy(1)
		f.holds(1, 18)
		v.idle()
	})
}

// A vCPU's load of a page of a private mapping of a read-only file is a minor
// fault, and UFFDIO_CONTINUE installs the pager's physical page. A vCPU's
// store traps on the write protection, and a store as its first access takes
// a minor fault and then a write-protect fault.
func TestReadOnlyPrivateMappingTrapsKVMStores(t *testing.T) {
	forEachPage(t, func(t *testing.T, pageSize int) {
		f := newReadOnlyFile(t, pageSize, 2)
		f.fill(0, 17)
		f.fill(1, 18)
		v := startReadOnlyVMM(t, f, 2, nil, false)
		v.attach()
		v.mapRun("private", 0, 0, 2)
		v.do("kvm", "kvm")

		v.send("kvmload 0")
		v.expectFault(0, faultMinor)
		v.resolve(0, 1)
		v.expect("loaded 17")
		v.shares(0)

		v.send("kvmstore 0 97")
		v.expectFault(0, faultWrite|faultWP)
		v.letThrough(0)
		v.expect("stored 97")
		v.ownsCopy(0)
		f.holds(0, 17)

		v.send(fmt.Sprintf("kvmstore %d 98", pageSize))
		v.expectFault(1, faultWrite|faultMinor)
		v.resolve(1, 1)
		v.expectFault(1, faultWrite|faultWP)
		v.letThrough(1)
		v.expect("stored 98")
		v.ownsCopy(1)
		f.holds(1, 18)
		v.idle()
	})
}

// A private mapping of a HugeTLB file reserves a pool page for every page it
// maps, for copies a store might make. With MAP_NORESERVE it reserves none,
// and reading every page through it takes none either.
func TestReadOnlyHugeTLBMappingWithoutReserveTakesNoPoolPages(t *testing.T) {
	const pages = 4
	f := newReadOnlyFile(t, hugePage, pages)
	for page := range pages {
		f.fill(page, byte(17+page))
	}
	fd := int(f.readOnly.Fd())
	free, reserved := hugePages(t)
	pool := func(what string, wantReserved int) {
		t.Helper()
		if gotFree, gotReserved := hugePages(t); gotFree != free || gotReserved != wantReserved {
			t.Fatalf("%s: %d free and %d reserved pool pages, want %d and %d", what, gotFree, gotReserved, free, wantReserved)
		}
	}
	mapping, err := unix.Mmap(fd, 0, pages*hugePage, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_NORESERVE)
	wantErr(t, "a private mapping with MAP_NORESERVE", err, nil)
	pool("mapped with MAP_NORESERVE", reserved)
	for page := range pages {
		if got := mapping[page*hugePage]; got != byte(17+page) {
			t.Fatalf("page %d reads %d, want %d", page, got, 17+page)
		}
	}
	pool("read through the mapping", reserved)
	wantErr(t, "unmapping", unix.Munmap(mapping), nil)
	mapping, err = unix.Mmap(fd, 0, pages*hugePage, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	wantErr(t, "a private mapping", err, nil)
	pool("mapped without MAP_NORESERVE", reserved+pages)
	wantErr(t, "unmapping", unix.Munmap(mapping), nil)
	pool("unmapped", reserved)
}

// Adjacent runs of a file, each mapped and installed by its own command, and
// adjacent in the file too. At 4 KiB they become one mapping when they are
// shared, in any order. Private runs of a read-only file take an anon_vma when
// UFFDIO_CONTINUE installs a page, and a run between two mappings with
// different ones merges with only one of them. So the order the runs arrive in
// decides how many mappings they leave: interleaved, each later run joins the
// run before it, and half remain. At 2 MiB no two runs ever merge, shared or
// private, because the kernel never merges HugeTLB mappings. The counts are
// logged for the plan.
func TestReadOnlyAdjacentRunsMappingCount(t *testing.T) {
	const runs = 8
	orders := []struct {
		name  string
		order []int
	}{
		{"ascending", []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"descending", []int{7, 6, 5, 4, 3, 2, 1, 0}},
		{"interleaved", []int{0, 2, 4, 6, 1, 3, 5, 7}},
	}
	want := map[int]map[string][]int{
		checkpoint.PageSize4KiB: {"shared": {1, 1, 1}, "private": {1, 1, 4}},
		hugePage:                {"shared": {8, 8, 8}, "private": {8, 8, 8}},
	}
	forEachPage(t, func(t *testing.T, pageSize int) {
		f := newReadOnlyFile(t, pageSize, runs)
		for page := range runs {
			f.fill(page, byte(17+page))
		}
		for _, mode := range []string{"shared", "private"} {
			v := startReadOnlyVMM(t, f, len(orders)*(runs+1), nil, true)
			v.attach()
			for k, order := range orders {
				first := k * (runs + 1)
				for _, run := range order.order {
					v.mapRun(mode, first+run, run, 1)
					v.resolve(first+run, 1)
				}
				for run := range runs {
					v.do(fmt.Sprintf("load %d", (first+run)*pageSize), fmt.Sprintf("loaded %d", 17+run))
				}
				v.send(fmt.Sprintf("mappings %d %d", first, runs))
				var got int
				if _, err := fmt.Sscanf(v.line(), "mappings %d", &got); err != nil {
					t.Fatal(err)
				}
				t.Logf("%d %s runs mapped %s leave %d mappings", runs, mode, order.name, got)
				if got != want[pageSize][mode][k] {
					t.Errorf("%d %s runs mapped %s leave %d mappings, want %d", runs, mode, order.name, got, want[pageSize][mode][k])
				}
			}
			v.idle()
		}
	})
}
