package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The two ioctls of <linux/kvm.h> that say whether this guest can run a VM of
// its own: _IO(KVMIO, 0x00) and _IO(KVMIO, 0x01), with KVMIO 0xAE. Neither
// takes an argument, so the numbers are the same on every architecture.
const (
	kvmGetAPIVersion = 0xAE00
	kvmCreateVM      = 0xAE01
)

// kvmAPIVersion is the one version of the API Linux has answered with since
// 2.6.22; any other is a KVM this witness does not know.
const kvmAPIVersion = 12

// createVM opens /dev/kvm, asks it for its API version and creates one VM on
// it, which it closes at once. The VM is of the default type and has no memory
// and no processors: its creation is the whole question.
func createVM() (string, error) {
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer device.Close()
	version, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return "", fmt.Errorf("asking /dev/kvm for its API version: %w", errno)
	}
	if version != kvmAPIVersion {
		return "", fmt.Errorf("/dev/kvm answers with API version %d, not %d", version, kvmAPIVersion)
	}
	vm, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return "", fmt.Errorf("creating a VM on /dev/kvm: %w", errno)
	}
	if err := unix.Close(int(vm)); err != nil {
		return "", fmt.Errorf("closing the VM created on /dev/kvm: %w", err)
	}
	return fmt.Sprintf("created a VM on KVM API version %d", version), nil
}
