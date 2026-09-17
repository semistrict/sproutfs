//go:build linux && (amd64 || arm64)

// Package vmwire carries the host side of the managed-memory control protocol
// described in docs/vm-memory.md: fixed 56-byte little-endian frames over a
// Unix socket, SCM_RIGHTS descriptor passing, sealed memfd arenas, and the
// userfaultfd ioctls that resolve one fault. It is shared by the production
// host in vmmemory and by the Go pager fixture that qualifies the Rust client.
package vmwire

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
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
func ioctlRetrying(fd uintptr, number uint64, args []uint64, write bool) error {
	backoff, started := ioctlBackoff, time.Now()
	for attempt := 0; ; attempt++ {
		err := IOCtl(fd, number, args, write)
		if !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if attempt < ioctlYields {
			runtime.Gosched()
			continue
		}
		if time.Since(started) >= ioctlMaxWait {
			return err
		}
		time.Sleep(backoff)
		backoff = min(2*backoff, ioctlMaxBackoff)
	}
}

// Frame kinds. These control messages are separate from Linux's native
// 32-byte UFFD events.
const (
	Hello    = 1
	Region   = 2
	Attach   = 3
	MapRange = 4
	Revoke   = 5
	Ack      = 6
	Stop     = 7
	// Seal asks the host to take the session's checkpoint: it write-protects the
	// dirty set and answers, without moving a byte. There is no durability request
	// in this protocol — a guest's flush makes nothing durable and the device
	// completes it itself.
	Seal     = 8
	Result   = 9
	MapBatch = 10
	Ready    = 11
	MapZero  = 12
)

const (
	// Version 6 dropped two fields at once: the region, because a session
	// carries exactly one, and the page size, because the page is 2 MiB on both
	// ends and the version is what says so.
	Version      = 6
	MaxBatchRuns = 1024
)

// FrameBytes is the encoded size of one control message.
const FrameBytes = 56

// Frame is one 56-byte control message. A session carries exactly one region,
// so no frame names one.
type Frame struct{ Kind, ID, Offset, Length, Backing, Generation, Flags uint64 }

// Bytes encodes the frame as seven little-endian words.
func (f Frame) Bytes() []byte {
	b := make([]byte, FrameBytes)
	for i, v := range []uint64{f.Kind, f.ID, f.Offset, f.Length, f.Backing, f.Generation, f.Flags} {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}

// Decode reads a frame out of exactly FrameBytes encoded bytes.
func Decode(b []byte) Frame {
	var v [7]uint64
	for i := range v {
		v[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return Frame{v[0], v[1], v[2], v[3], v[4], v[5], v[6]}
}

// Read consumes one whole frame, returning the zero frame on any error.
func Read(r io.Reader) (Frame, error) {
	var b [FrameBytes]byte
	_, err := io.ReadFull(r, b[:])
	if err != nil {
		return Frame{}, err
	}
	return Decode(b[:]), nil
}

// WriteBytes writes b in full, tolerating short writes.
func WriteBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// Write sends one encoded frame.
func Write(w io.Writer, f Frame) error { return WriteBytes(w, f.Bytes()) }

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
	if err := ioctlRetrying(fd, 6, []uint64{address, size, wp}, true); err != nil {
		return err
	}
	return ioctlRetrying(fd, 2, []uint64{address, size}, false)
}

// continueRange installs a range with one CONTINUE when none of it is present.
// EEXIST reports only that one backing page already was, not that the rest of the
// range is, so the range is then finished one backing page at a time. A partial
// CONTINUE reports EAGAIN, whose retry finds its first page present and so
// takes the same path; a concurrent mapping change is the other EAGAIN.
func continueRange(fd uintptr, address, size, pageSize, mode uint64) error {
	err := ioctlRetrying(fd, 7, []uint64{address, size, mode, 0}, true)
	if !errors.Is(err, syscall.EEXIST) {
		return err
	}
	for offset := uint64(0); offset < size; offset += pageSize {
		err := ioctlRetrying(fd, 7, []uint64{address + offset, pageSize, mode, 0}, true)
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	return nil
}

// WakeRange releases faults after a replacement whose PTEs were populated by
// the client. Installing those PTEs does not wake waiters on the old mapping.
func WakeRange(fd uintptr, address, size uint64) error {
	return ioctlRetrying(fd, 2, []uint64{address, size}, false)
}

// ProtectRange takes write access away from a whole registered range with one
// ioctl, leaving its mappings, contents and page tables in place: the next
// store traps with UFFD_PAGEFAULT_FLAG_WP and nothing else changes. The range
// may span several mappings of the client, since the kernel applies the mode to
// every registered VMA it covers, and it may already be protected. This is what
// a seal costs instead of replacing one mapping per run.
func ProtectRange(fd uintptr, address, size uint64) error {
	return ioctlRetrying(fd, 6, []uint64{address, size, 1}, true) // WRITEPROTECT_MODE_WP
}
