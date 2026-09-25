//go:build linux && (amd64 || arm64)

package vmtest

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// machine is a tiny KVM guest over one memory region, as the Rust fixture's
// kvm.rs builds it. Its code lives outside the memory region, so the guest
// touches the memory region only when told to.
type machine struct {
	vm, vcpu int
	run      []byte
}

const (
	// guestCode is where the guest's code is. A load starts at guestCode and a
	// store at guestCode+16.
	guestCode = 0x1000
	// guestData is where the memory region is in the guest.
	guestData = 0x1000_0000
	// guestOutput is where the arm64 guest writes the byte it read, as MMIO.
	// The x86 guest writes it to port 0x3f8.
	guestOutput = 0x3000_0000
)

// KVM's ioctl numbers, and the fields of struct kvm_run this file reads.
const (
	kvmCreateVM         = 0xae01
	kvmGetVCPUMmapSize  = 0xae04
	kvmCreateVCPU       = 0xae41
	kvmRun              = 0xae80
	kvmSetMemoryRegion  = 1<<30 | 32<<16 | 0xae46
	kvmRunImmediateExit = 1
	kvmRunExitReason    = 8
	kvmExitIO           = 2
	kvmExitMMIO         = 6
)

// memorySlot is struct kvm_userspace_memory_region.
type memorySlot struct {
	slot, flags   uint32
	guestAddress  uint64
	size          uint64
	userspaceAddr uint64
}

func kvmIoctl(fd int, request uintptr, argument unsafe.Pointer) (int, error) {
	result, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
	if errno != 0 {
		return 0, errno
	}
	return int(result), nil
}

// newMachine makes a VM with one vCPU. Its code is one host page of its own,
// and memory is its data. memory must be memory mapped outside Go's heap.
func newMachine(memory []byte) (*machine, error) {
	kvm, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(kvm)
	vm, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(kvm), kvmCreateVM, 0)
	if errno != 0 {
		return nil, fmt.Errorf("KVM_CREATE_VM: %w", errno)
	}
	size, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(kvm), kvmGetVCPUMmapSize, 0)
	if errno != 0 {
		return nil, fmt.Errorf("KVM_GET_VCPU_MMAP_SIZE: %w", errno)
	}
	m := &machine{vm: int(vm)}
	// The code is written before its slot is registered, and never again.
	code, err := unix.Mmap(-1, 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, err
	}
	copy(code, guestInstructions())
	for i, slot := range []struct {
		guest  uint64
		memory []byte
	}{{guestCode, code}, {guestData, memory}} {
		region := memorySlot{slot: uint32(i), guestAddress: slot.guest, size: uint64(len(slot.memory)),
			userspaceAddr: uint64(address(slot.memory))}
		if _, err := kvmIoctl(m.vm, kvmSetMemoryRegion, unsafe.Pointer(&region)); err != nil {
			return nil, fmt.Errorf("KVM_SET_USER_MEMORY_REGION %d: %w", i, err)
		}
	}
	vcpu, _, errno := unix.Syscall(unix.SYS_IOCTL, vm, kvmCreateVCPU, 0)
	if errno != 0 {
		return nil, fmt.Errorf("KVM_CREATE_VCPU: %w", errno)
	}
	m.vcpu = int(vcpu)
	if m.run, err = unix.Mmap(m.vcpu, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED); err != nil {
		return nil, err
	}
	if err := m.initVCPU(); err != nil {
		return nil, err
	}
	return m, nil
}

// access runs the guest once. It loads the byte at offset of the memory
// region, or stores value there and loads it back, and returns the byte the
// guest loaded.
func (m *machine) access(offset uint64, store bool, value byte) (byte, error) {
	pc := uint64(guestCode)
	if store {
		pc += 16
	}
	if err := m.setAccess(pc, guestData+offset, value); err != nil {
		return 0, err
	}
	// A signal ends KVM_RUN early. The vCPU then runs again from where it was.
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(m.vcpu), kvmRun, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return 0, fmt.Errorf("KVM_RUN: %w", errno)
		}
		break
	}
	loaded, err := m.output()
	if err != nil {
		return 0, err
	}
	// Complete the output before the registers change, so the next access
	// starts where it is told to.
	m.run[kvmRunImmediateExit] = 1
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(m.vcpu), kvmRun, 0)
	m.run[kvmRunImmediateExit] = 0
	if !errors.Is(errno, unix.EINTR) {
		return 0, fmt.Errorf("completing the output: %v, want EINTR", errno)
	}
	return loaded, nil
}
