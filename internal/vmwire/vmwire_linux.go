//go:build linux && (amd64 || arm64)

// This file is the half of vmwire that is syscalls: descriptor passing, the
// arenas a host allocates, and the userfaultfd ioctls that resolve, wake and
// protect a range. The frames and the geometry are in vmwire.go, which builds
// everywhere.
package vmwire

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/semistrict/sproutfs/internal/checkpoint"
)

// The retry schedule of one userfaultfd ioctl: a few yields, which is all the
// ordinary race needs, then a doubling sleep so a longer one costs a sleeping
// goroutine rather than a core, and a bound on the whole wait. The caller's
// command timeout is the outer bound, and this one keeps a wedged address space
// from spinning a processor until it fires.
const (
	ioctlYields     = 16
	ioctlBackoff    = 20 * time.Microsecond
	ioctlMaxBackoff = 2 * time.Millisecond
	ioctlMaxWait    = 10 * time.Second
)

// ioctlRetrying issues one userfaultfd ioctl, retrying the transient EAGAIN the
// kernel returns while a concurrent mapping change on the same address space is
// in flight. Concurrent fault workers on one region make that race ordinary.
//
// A failure carries the name of the ioctl. What the pager reports as the reason
// a VM's memory ended is whatever these return, and an errno on its own is the
// same word for four different calls over three different ranges: the errno
// says the kernel refused, and the name says which range of which kind of
// mapping it refused to install, wake or protect.
func ioctlRetrying(name string, fd uintptr, number uint64, args []uint64, write bool) error {
	backoff, started := ioctlBackoff, time.Now()
	for attempt := 0; ; attempt++ {
		err := IOCtl(fd, number, args, write)
		if err != nil && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err == nil {
			return nil
		}
		if attempt < ioctlYields {
			runtime.Gosched()
			continue
		}
		if time.Since(started) >= ioctlMaxWait {
			return fmt.Errorf("%s: %w", name, err)
		}
		time.Sleep(backoff)
		backoff = min(2*backoff, ioctlMaxBackoff)
	}
}

// ReceiveFD reads a frame that carries exactly one descriptor.
func ReceiveFD(c *net.UnixConn) (Frame, *os.File, error) {
	b, oob := make([]byte, FrameBytes), make([]byte, syscall.CmsgSpace(4)*4)
	n, on, flags, _, err := c.ReadMsgUnix(b, oob)
	if err != nil {
		return Frame{}, nil, err
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:on])
	if err != nil {
		return Frame{}, nil, err
	}
	var fds []int
	closeFDs := func() {
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
	}
	for _, msg := range messages {
		rights, err := syscall.ParseUnixRights(&msg)
		if err != nil {
			closeFDs()
			return Frame{}, nil, err
		}
		fds = append(fds, rights...)
	}
	if flags&syscall.MSG_CTRUNC != 0 || len(fds) != 1 || n == 0 {
		closeFDs()
		return Frame{}, nil, errors.New("expected one UFFD descriptor")
	}
	syscall.CloseOnExec(fds[0])
	f := os.NewFile(uintptr(fds[0]), "userfaultfd")
	if _, err = io.ReadFull(c, b[n:]); err != nil {
		_ = f.Close()
		return Frame{}, nil, err
	}
	return Decode(b), f, nil
}

// SendFD writes a frame together with one descriptor.
func SendFD(c *net.UnixConn, f Frame, file *os.File) error {
	b := f.Bytes()
	n, _, err := c.WriteMsgUnix(b, syscall.UnixRights(int(file.Fd())), nil)
	if err != nil {
		return err
	}
	if n == 0 {
		return io.ErrShortWrite
	}
	return WriteBytes(c, b[n:])
}

// HugeMemfd creates a size-sealed anonymous file over explicit 2 MiB HugeTLB
// pages from the host's provisioned pool. Writes and punching stay allowed;
// resizing and further seals do not.
func HugeMemfd(name string, size int64) (*os.File, error) {
	if size <= 0 || size%(2<<20) != 0 {
		return nil, syscall.EINVAL
	}
	return memfd(name, size, 3|4|(21<<26))
}

// SharedMemfd creates the same size-sealed anonymous file over ordinary shared
// memory, whose page is the host's own 4 KiB. It is what a RAM arena is made
// of: a slot is replaceable at 4 KiB, the memory is charged to the pod rather
// than to the HugeTLB pool, and a host with swap may swap it.
func SharedMemfd(name string, size int64) (*os.File, error) {
	if size <= 0 || size%checkpoint.PageSize4KiB != 0 {
		return nil, syscall.EINVAL
	}
	return memfd(name, size, 3)
}

// ArenaMemfd creates the arena an instance of the given page runs on: the
// HugeTLB pool's for a 2 MiB page, ordinary shared memory for a 4 KiB one.
// Nothing else decides which, so a pager and the memory behind it cannot
// disagree.
func ArenaMemfd(name string, pageSize uint64, size int64) (*os.File, error) {
	backing, err := BackingFor(pageSize)
	if err != nil {
		return nil, err
	}
	if backing == BackingHugeTLB {
		return HugeMemfd(name, size)
	}
	return SharedMemfd(name, size)
}

func memfd(name string, size int64, flags uintptr) (*os.File, error) {
	number := uintptr(319) // Linux x86_64 __NR_memfd_create
	if runtime.GOARCH == "arm64" {
		number = 279
	}
	raw, _ := syscall.BytePtrFromString(name)
	fd, _, errno := syscall.Syscall(number, uintptr(unsafe.Pointer(raw)), flags, 0) // CLOEXEC | ALLOW_SEALING
	runtime.KeepAlive(raw)
	if errno != 0 {
		return nil, errno
	}
	f := os.NewFile(fd, name)
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return nil, err
	}
	_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, 1033, 1|2|4) // F_ADD_SEALS
	if errno != 0 {
		_ = f.Close()
		return nil, errno
	}
	return f, nil
}

// IOCtl issues one userfaultfd ioctl, encoding the asm-generic request number.
func IOCtl(fd uintptr, number uint64, args []uint64, write bool) error {
	direction := uint64(2) // _IOR for UFFDIO_WAKE
	if write {
		direction = 3
	}
	request := (direction << 30) | (uint64(len(args)*8) << 16) | (0xaa << 8) | number
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(request), uintptr(unsafe.Pointer(&args[0])))
	runtime.KeepAlive(args)
	if errno != 0 {
		return errno
	}
	return nil
}

// Resolve installs the page tables of a whole range of a shared mapping from
// the backing it maps, sets or clears its write protection, and only then wakes
// any thread that faulted anywhere in it. One call serves a single faulting
// page and a populated run alike, whatever number of backing pages each spans.
// Page tables already present are kept. The range need not have faulted.
func Resolve(fd uintptr, address, size, pageSize uint64, writable bool) error {
	mode := uint64(1) // CONTINUE_MODE_DONTWAKE
	wp := uint64(2)   // WRITEPROTECT_MODE_DONTWAKE, clearing the protection
	if !writable {
		mode |= 2 // install write protection atomically
		wp = 1    // WP and DONTWAKE cannot be combined for this ioctl.
	}
	if err := continueRange(fd, address, size, pageSize, mode); err != nil {
		return err
	}
	if err := ioctlRetrying("UFFDIO_WRITEPROTECT", fd, 6, []uint64{address, size, wp}, true); err != nil {
		return err
	}
	return ioctlRetrying("UFFDIO_WAKE", fd, 2, []uint64{address, size}, false)
}

// continueRange installs a range with one CONTINUE when none of it is present.
// EEXIST reports only that one backing page already was, not that the rest of the
// range is, so the range is then finished one backing page at a time. A partial
// CONTINUE reports EAGAIN, whose retry finds its first page present and so
// takes the same path; a concurrent mapping change is the other EAGAIN.
func continueRange(fd uintptr, address, size, pageSize, mode uint64) error {
	err := ioctlRetrying("UFFDIO_CONTINUE", fd, 7, []uint64{address, size, mode, 0}, true)
	if !errors.Is(err, syscall.EEXIST) {
		return err
	}
	for offset := uint64(0); offset < size; offset += pageSize {
		err := ioctlRetrying("UFFDIO_CONTINUE", fd, 7, []uint64{address + offset, pageSize, mode, 0}, true)
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	return nil
}

// WakeRange releases faults after a replacement whose PTEs were populated by
// the client. Installing those PTEs does not wake waiters on the old mapping.
func WakeRange(fd uintptr, address, size uint64) error {
	return ioctlRetrying("UFFDIO_WAKE", fd, 2, []uint64{address, size}, false)
}

// ProtectRange takes write access away from a whole registered range with one
// ioctl, leaving its mappings, contents and page tables in place: the next
// store traps with UFFD_PAGEFAULT_FLAG_WP and nothing else changes. The range
// may span several mappings of the client, since the kernel applies the mode to
// every registered VMA it covers, and it may already be protected. This is what
// a seal costs instead of replacing one mapping per run.
func ProtectRange(fd uintptr, address, size uint64) error {
	return ioctlRetrying("UFFDIO_WRITEPROTECT", fd, 6, []uint64{address, size, 1}, true) // WRITEPROTECT_MODE_WP
}
