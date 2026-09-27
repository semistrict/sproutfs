//go:build linux && (amd64 || arm64)

package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ioctls of <linux/kvm.h> that every architecture shares, with KVMIO 0xAE.
// Those that take no argument are _IO(KVMIO, nr); the memory slot's is
// _IOW(KVMIO, 0x46, struct kvm_userspace_memory_region), 32 bytes.
const (
	kvmGetAPIVersion       = 0xAE00
	kvmCreateVM            = 0xAE01
	kvmGetVCPUMmapSize     = 0xAE04
	kvmCreateVCPU          = 0xAE41
	kvmSetUserMemoryRegion = 0x4020AE46
	kvmRun                 = 0xAE80
)

// kvmAPIVersion is the one version of the API Linux has answered with since
// 2.6.22; any other is a KVM this witness does not know.
const kvmAPIVersion = 12

// The exit reasons of struct kvm_run that an L2 of this witness can end on,
// named so that one that should not have happened says which it was.
const (
	exitHLT  = 5
	exitMMIO = 6
)

var exitNames = map[uint32]string{
	0: "an unknown exit", 1: "an exception", 2: "port I/O", exitHLT: "HLT", exitMMIO: "MMIO",
	8: "a shutdown", 9: "a failed VM entry", 10: "an interrupted run", 17: "an internal error",
	24: "a system event",
}

// kvmRunData is where struct kvm_run's union of exit details begins: after
// the request and exit bytes, the exit reason, the flags, cr8 and apic_base.
const kvmRunData = 32

func ioctl(fd int, request, argument uintptr) (uintptr, error) {
	result, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, argument)
	if errno != 0 {
		return 0, errno
	}
	return result, nil
}

func ioctlPointer(fd int, request uintptr, argument unsafe.Pointer) (uintptr, error) {
	result, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
	if errno != 0 {
		return 0, errno
	}
	return result, nil
}

// openKVM opens /dev/kvm and holds it to the one API version there is.
func openKVM() (int, error) {
	device, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: "/dev/kvm", Err: err}
	}
	version, err := ioctl(device, kvmGetAPIVersion, 0)
	if err != nil {
		return -1, errors.Join(fmt.Errorf("asking /dev/kvm for its API version: %w", err), unix.Close(device))
	}
	if version != kvmAPIVersion {
		return -1, errors.Join(fmt.Errorf("/dev/kvm answers with API version %d, not %d", version, kvmAPIVersion),
			unix.Close(device))
	}
	return device, nil
}

// createVM opens /dev/kvm, asks it for its API version and creates one VM on
// it, which it closes at once. The VM is of the default type and has no memory
// and no processors: its creation is the whole question.
func createVM() (string, error) {
	device, err := openKVM()
	if err != nil {
		return "", err
	}
	defer unix.Close(device)
	vm, err := ioctl(device, kvmCreateVM, 0)
	if err != nil {
		return "", fmt.Errorf("creating a VM on /dev/kvm: %w", err)
	}
	if err := unix.Close(int(vm)); err != nil {
		return "", fmt.Errorf("closing the VM created on /dev/kvm: %w", err)
	}
	return fmt.Sprintf("created a VM on KVM API version %d", kvmAPIVersion), nil
}

// l2 is one VM of this guest's own, with one memory slot and one vCPU. Its
// memory is the caller's, which maps it before and unmaps it after.
type l2 struct {
	kvm, vm, vcpu int
	memory        []byte
	// run is the vCPU's struct kvm_run, which says why KVM_RUN returned.
	run []byte
}

// newL2 makes a VM of memory, which must be l2MemoryBytes, with program at
// l2Code and one vCPU about to run it. The vCPU runs on the thread that calls
// enter, which must be locked to its goroutine.
func newL2(memory, program []byte) (_ *l2, err error) {
	if len(memory) != l2MemoryBytes {
		return nil, fmt.Errorf("an L2's memory is %d bytes, not %d", len(memory), l2MemoryBytes)
	}
	copy(memory[l2Code:], program)
	m := &l2{kvm: -1, vm: -1, vcpu: -1, memory: memory}
	defer func() {
		if err != nil {
			err = errors.Join(err, m.close())
		}
	}()
	if m.kvm, err = openKVM(); err != nil {
		return nil, err
	}
	vm, err := ioctl(m.kvm, kvmCreateVM, 0)
	if err != nil {
		return nil, fmt.Errorf("creating a VM on /dev/kvm: %w", err)
	}
	m.vm = int(vm)
	if err := prepareVM(m.vm); err != nil {
		return nil, err
	}
	// struct kvm_userspace_memory_region: slot, flags, guest_phys_addr,
	// memory_size, userspace_addr. Slot 0 at guest-physical 0.
	var region [32]byte
	binary.LittleEndian.PutUint64(region[16:], uint64(len(memory)))
	binary.LittleEndian.PutUint64(region[24:], uint64(uintptr(unsafe.Pointer(&memory[0]))))
	if _, err := ioctlPointer(m.vm, kvmSetUserMemoryRegion, unsafe.Pointer(&region[0])); err != nil {
		return nil, fmt.Errorf("giving the L2 its memory: %w", err)
	}
	vcpu, err := ioctl(m.vm, kvmCreateVCPU, 0)
	if err != nil {
		return nil, fmt.Errorf("creating the L2's vCPU: %w", err)
	}
	m.vcpu = int(vcpu)
	size, err := ioctl(m.kvm, kvmGetVCPUMmapSize, 0)
	if err != nil {
		return nil, fmt.Errorf("asking /dev/kvm how large a vCPU's run structure is: %w", err)
	}
	if m.run, err = unix.Mmap(m.vcpu, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED); err != nil {
		return nil, fmt.Errorf("mapping the L2 vCPU's run structure: %w", err)
	}
	if err := prepareVCPU(m.vm, m.vcpu); err != nil {
		return nil, err
	}
	return m, nil
}

// close gives the VM back. The memory stays the caller's.
func (m *l2) close() error {
	var errs []error
	if m.run != nil {
		errs = append(errs, unix.Munmap(m.run))
	}
	for _, fd := range []int{m.vcpu, m.vm, m.kvm} {
		if fd >= 0 {
			errs = append(errs, unix.Close(fd))
		}
	}
	return errors.Join(errs...)
}

// enter runs the vCPU until KVM hands it back, and reports why. A run a
// signal interrupted is entered again: the Go runtime signals its threads to
// preempt them, and that says nothing about L2.
func (m *l2) enter() (uint32, error) {
	for {
		_, err := ioctl(m.vcpu, kvmRun, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("running the L2: %w", err)
		}
		return binary.LittleEndian.Uint32(m.run[8:]), nil
	}
}

// toStop enters the vCPU and requires it to come back at the stop its program
// ends on or rings to say it has begun.
func (m *l2) toStop() error {
	reason, err := m.enter()
	if err != nil {
		return err
	}
	if !stopped(m.run, reason) {
		return fmt.Errorf("the L2 ended on %s, not at its stop", m.describe(reason))
	}
	return nil
}

// describe says what an exit was, with its details for whoever has to decode
// them: a failed entry's reason is the first word.
func (m *l2) describe(reason uint32) string {
	name, ok := exitNames[reason]
	if !ok {
		name = "an exit"
	}
	return fmt.Sprintf("%s (exit reason %d, exit data % x)", name, reason, m.run[kvmRunData:kvmRunData+32])
}

// runL2 runs the program that stores l2Stored and stops, and checks that the
// store landed in the memory the L2 was given.
func runL2() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	memory, err := unix.Mmap(-1, 0, l2MemoryBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return "", fmt.Errorf("mapping the L2's memory: %w", err)
	}
	defer unix.Munmap(memory)
	m, err := newL2(memory, storeProgram())
	if err != nil {
		return "", err
	}
	defer m.close()
	if err := m.toStop(); err != nil {
		return "", err
	}
	if got := binary.LittleEndian.Uint32(memory[l2Data:]); got != l2Stored {
		return "", fmt.Errorf("the L2 stopped with %#x at %#x, want the %#x it stores", got, l2Data, l2Stored)
	}
	return fmt.Sprintf("L2 ran to its stop and stored %#x at %#x", l2Stored, l2Data), nil
}

// loopStartTimeout bounds how long `kvm loop` waits for the resident half to
// say its L2 has begun. It is the bound on a witness that is wedged: creating
// a VM and entering it once takes milliseconds.
const loopStartTimeout = time.Minute

// startL2Loop starts the resident half, which runs an L2 that counts for as
// long as this guest runs, and returns once that L2 has begun. The half's
// stdout is a pipe back to here, and its one line says whether it began.
func startL2Loop(parsed kvmOptions) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this binary: %w", err)
	}
	log, err := os.OpenFile(parsed.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("the L2 log %s: %w", parsed.log, err)
	}
	defer log.Close()
	quiet, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer quiet.Close()
	said, told, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("a pipe for the resident L2 to answer on: %w", err)
	}
	defer said.Close()
	process, err := os.StartProcess(self,
		[]string{self, "kvm", serveLoopVerb, "--file", parsed.file, "--log", parsed.log},
		&os.ProcAttr{Files: []*os.File{quiet, told, log},
			// A session of its own, so the shell that ran this hanging up does
			// not hang up the L2.
			Sys: &syscall.SysProcAttr{Setsid: true}})
	closed := told.Close()
	if err != nil {
		return "", errors.Join(fmt.Errorf("starting the resident L2: %w", err), closed)
	}
	// It is nobody's child: this process exits once it has answered, and the
	// guest's init reaps it.
	if err := errors.Join(closed, process.Release()); err != nil {
		return "", err
	}
	if err := said.SetReadDeadline(time.Now().Add(loopStartTimeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(said).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("the resident L2 did not say it began: %w%s", err, lastWords(parsed.log))
	}
	answer := strings.TrimSpace(line)
	if rest, failed := strings.CutPrefix(answer, "error "); failed {
		return "", errors.New(rest)
	}
	return strings.TrimPrefix(answer, "ok "), nil
}

// serveL2Loop is the resident half: it maps the file as the L2's memory, runs
// the program that counts, says on stdout that the L2 has begun once it rings
// its stop, and then runs it until something ends it. It never returns while
// the L2 runs, and what ended it goes to the log.
func serveL2Loop(parsed kvmOptions) error {
	runtime.LockOSThread()
	answer := os.Stdout
	fail := func(err error) error {
		fmt.Fprintf(answer, "error %v\n", err)
		return err
	}
	// A file that is already there is another L2's memory, which truncating
	// would take out from under it.
	file, err := os.OpenFile(parsed.file, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fail(fmt.Errorf("the counting L2's memory: %w", err))
	}
	if err := file.Truncate(l2MemoryBytes); err != nil {
		return fail(errors.Join(fmt.Errorf("sizing %s: %w", parsed.file, err), file.Close()))
	}
	memory, err := unix.Mmap(int(file.Fd()), 0, l2MemoryBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fail(errors.Join(fmt.Errorf("mapping %s: %w", parsed.file, err), file.Close()))
	}
	if err := file.Close(); err != nil {
		return fail(err)
	}
	m, err := newL2(memory, countProgram())
	if err != nil {
		return fail(err)
	}
	if err := m.toStop(); err != nil {
		return fail(err)
	}
	fmt.Fprintf(answer, "ok L2 is counting at %#x of %s\n", l2Data, parsed.file)
	if err := answer.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: L2 counting at %#x of %s\n", l2Data, parsed.file)
	reason, err := m.enter()
	if err != nil {
		return err
	}
	return fmt.Errorf("the counting L2 ended on %s", m.describe(reason))
}
