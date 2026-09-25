//go:build linux && (amd64 || arm64)

package vmtest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/internal/vmwire"
)

// vmmRole makes this test binary the VMM of readonly_linux_test.go. Its value
// is the page size and the memory region's length in pages.
const vmmRole = "SPROUTFS_VMTEST_VMM"

// The descriptors a VMM process starts with.
const (
	// vmmSocket is the pager's socket. The UFFD goes to the pager over it.
	vmmSocket = 3
	// vmmReadOnly is the file, opened read-only.
	vmmReadOnly = 4
	// vmmReadWrite is the file, opened read-write, when a test gives it.
	vmmReadWrite = 5
)

// The userfaultfd API, as the client uses it.
const (
	uffdAPI      = 0xaa
	uffdioAPI    = 0x3f
	uffdRegister = 0x00
	// uffdFeatures is every feature the client requires: REMAP events,
	// write-protect fault flags, and missing, minor and write-protect faults
	// on HugeTLB and shmem.
	uffdFeatures = 1<<2 | 1<<0 | 1<<4 | 1<<9 | 1<<5 | 1<<10 | 1<<12
	// uffdModes registers a mapping for missing, write-protect and minor
	// faults.
	uffdModes = 1 | 2 | 4
	// uffdIoctls is the ioctls a resident run needs: WAKE, WRITEPROTECT and
	// CONTINUE.
	uffdIoctls = 1<<2 | 1<<6 | 1<<7
)

// vmm is the state of the VMM process.
type vmm struct {
	pageSize int
	// region is the memory region: an anonymous reservation that runs of the
	// file are moved into.
	region  []byte
	uffd    int
	machine *machine
}

// TestReadOnlyVMM is the VMM process of the tests in readonly_linux_test.go.
// It is not a test of its own. It reads one command per line from standard
// input and answers each with one line.
func TestReadOnlyVMM(t *testing.T) {
	role := os.Getenv(vmmRole)
	if role == "" {
		return
	}
	// KVM's vCPU stays on one thread.
	runtime.LockOSThread()
	var pageSize, pages int
	if _, err := fmt.Sscanf(role, "%d %d", &pageSize, &pages); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	v, err := newVMM(pageSize, pages)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Printf("ready %d %d\n", os.Getpid(), address(v.region))
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		reply, err := v.command(strings.Fields(scanner.Text()))
		if err != nil {
			reply = "error " + err.Error()
		}
		fmt.Println(reply)
	}
	os.Exit(0)
}

// address is where b starts. b must be memory mapped outside Go's heap.
func address(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}

func newVMM(pageSize, pages int) (*vmm, error) {
	length := pageSize * pages
	reservation, err := unix.Mmap(-1, 0, length+hugePage, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE)
	if err != nil {
		return nil, err
	}
	// A HugeTLB run can only be placed at a multiple of its page.
	skip := int(-address(reservation) & (hugePage - 1))
	return &vmm{pageSize: pageSize, region: reservation[skip : skip+length], uffd: -1}, nil
}

func (v *vmm) command(words []string) (string, error) {
	if len(words) == 0 {
		return "", errors.New("empty command")
	}
	name, arguments := words[0], words[1:]
	if name == "map" || name == "reopen" {
		if len(arguments) == 0 {
			return "", fmt.Errorf("%s needs an argument", name)
		}
		arguments = arguments[1:]
	}
	numbers := make([]int, len(arguments))
	for i, word := range arguments {
		number, err := strconv.Atoi(word)
		if err != nil {
			return "", err
		}
		numbers[i] = number
	}
	arity := map[string]int{"uffd": 0, "map": 3, "load": 1, "store": 2, "kvm": 0, "kvmload": 1, "kvmstore": 2,
		"mappings": 2, "reopen": 0, "reopen-own": 0, "fchmod": 0}
	if want, ok := arity[name]; !ok || len(numbers) != want {
		return "", fmt.Errorf("unknown command %q", strings.Join(words, " "))
	}
	switch name {
	case "uffd":
		return "uffd", v.openUFFD()
	case "map":
		return v.mapRun(words[1], numbers[0], numbers[1], numbers[2])
	case "load":
		return fmt.Sprintf("loaded %d", v.region[numbers[0]]), nil
	case "store":
		v.region[numbers[0]] = byte(numbers[1])
		return "stored", nil
	case "kvm":
		var err error
		v.machine, err = newMachine(v.region)
		return "kvm", err
	case "kvmload":
		loaded, err := v.machine.access(uint64(numbers[0]), false, 0)
		return fmt.Sprintf("loaded %d", loaded), err
	case "kvmstore":
		loaded, err := v.machine.access(uint64(numbers[0]), true, byte(numbers[1]))
		return fmt.Sprintf("stored %d", loaded), err
	case "mappings":
		return v.mappings(numbers[0], numbers[1])
	case "reopen":
		return outcome(reopen(words[1]))
	case "reopen-own":
		return outcome(reopenOwn())
	case "fchmod":
		return outcome(unix.Fchmod(vmmReadOnly, 0o666))
	}
	panic("unreachable")
}

// openUFFD opens a userfaultfd with the client's features, and sends it to the
// pager.
func (v *vmm) openUFFD() error {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		return fmt.Errorf("userfaultfd: %w", errno)
	}
	v.uffd = int(fd)
	api := []uint64{uffdAPI, uffdFeatures, 0}
	if err := vmwire.IOCtl(fd, uffdioAPI, api, true); err != nil {
		return fmt.Errorf("UFFDIO_API: %w", err)
	}
	if api[1]&uffdFeatures != uffdFeatures {
		return fmt.Errorf("UFFDIO_API features %#x, want %#x", api[1], uffdFeatures)
	}
	return unix.Sendmsg(vmmSocket, []byte{1}, unix.UnixRights(v.uffd), nil, 0)
}

// mapRun maps pages of the file at filePage into the memory region at page, as
// the client maps a run: it maps the run away from the memory region,
// registers it, write-protects it, and only then moves it into place. mode
// "private" maps the read-only file MAP_PRIVATE, as the isolated arena would.
// "shared" maps the read-write file MAP_SHARED, as the client does today.
// "readonly-shared" maps the read-only file MAP_SHARED, which the kernel does
// not let a writable mapping do.
//
// It answers with the ioctls the registration offers, or with the call the
// kernel refused and its errno.
func (v *vmm) mapRun(mode string, page, filePage, pages int) (string, error) {
	length := uintptr(pages * v.pageSize)
	if page < 0 || pages <= 0 || (page+pages)*v.pageSize > len(v.region) {
		return "", fmt.Errorf("pages [%d, %d) are outside the memory region", page, page+pages)
	}
	fd, prot, flags := vmmReadOnly, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_NORESERVE
	switch mode {
	case "private":
	case "shared":
		fd, flags = vmmReadWrite, unix.MAP_SHARED
	case "readonly-shared":
		prot, flags = unix.PROT_READ, unix.MAP_SHARED
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}
	staging, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, length, uintptr(prot), uintptr(flags), uintptr(fd),
		uintptr(filePage*v.pageSize))
	if errno != 0 {
		return "failed mmap " + unix.ErrnoName(errno), nil
	}
	unmap := func() { _, _, _ = unix.Syscall(unix.SYS_MUNMAP, staging, length, 0) }
	registration := []uint64{uint64(staging), uint64(length), uffdModes, 0}
	if err := vmwire.IOCtl(uintptr(v.uffd), uffdRegister, registration, true); err != nil {
		unmap()
		var refused unix.Errno
		if !errors.As(err, &refused) {
			return "", err
		}
		return "failed UFFDIO_REGISTER " + unix.ErrnoName(refused), nil
	}
	if err := vmwire.ProtectRange(uintptr(v.uffd), uint64(staging), uint64(length)); err != nil {
		unmap()
		return "", err
	}
	target := address(v.region) + uintptr(page*v.pageSize)
	_, _, errno = unix.Syscall6(unix.SYS_MREMAP, staging, length, length, unix.MREMAP_MAYMOVE|unix.MREMAP_FIXED, target, 0)
	if errno != 0 {
		unmap()
		return "", fmt.Errorf("mremap: %w", errno)
	}
	return fmt.Sprintf("mapped %d", registration[3]), nil
}

// mappings counts the mappings that cover any of pages of the memory region
// from page.
func (v *vmm) mappings(page, pages int) (string, error) {
	from := uint64(address(v.region)) + uint64(page*v.pageSize)
	to := from + uint64(pages*v.pageSize)
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return "", err
	}
	count := 0
	for line := range strings.Lines(string(maps)) {
		var start, end uint64
		if _, err := fmt.Sscanf(line, "%x-%x", &start, &end); err != nil {
			return "", fmt.Errorf("maps line %q: %w", line, err)
		}
		if start < to && end > from {
			count++
		}
	}
	return fmt.Sprintf("mappings %d", count), nil
}

// reopen opens path for reading and writing, and closes it.
func reopen(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err == nil {
		unix.Close(fd)
	}
	return err
}

// reopenOwn makes a memfd of this process's own, sets its mode to 0600, opens
// it read-only through /proc/self/fd, and reopens that for writing.
func reopenOwn() error {
	fd, err := unix.MemfdCreate("sproutfs-own", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return err
	}
	readOnly, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(readOnly)
	return reopen(fmt.Sprintf("/proc/self/fd/%d", readOnly))
}

// outcome answers "ok" for a call that worked, and "refused" and the errno
// for one the kernel refused.
func outcome(err error) (string, error) {
	var refused unix.Errno
	switch {
	case err == nil:
		return "ok", nil
	case errors.As(err, &refused):
		return "refused " + unix.ErrnoName(refused), nil
	}
	return "", err
}
