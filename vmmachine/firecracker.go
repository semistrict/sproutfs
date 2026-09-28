package vmmachine

import "sync"

// Firecracker is this package's own Starter: Firecracker run directly as a
// child of this process, with the guest's serial console kept in memory and a
// vsock in the process's directory. It is what a host runs when nothing embeds
// it, and what the tests of this package start their guests with.
type Firecracker struct {
	// Binary is the VMM and SeccompFilter its compiled policy, which must
	// include the VMM's memory-worker policy.
	Binary, SeccompFilter string
	// Kernel, Initrd and BootArgs are what a cold boot boots. A Firecracker
	// without a kernel can only restore.
	Kernel, Initrd, BootArgs string
	// VCPUs is a booted guest's processors when its VM records none. A VM that
	// records a count boots with that, and a restore carries its own.
	VCPUs int
	// VsockCID is the guest context id of the VM's virtio-vsock device, and
	// zero a VM without one. The device's host end is a socket in the process's
	// directory. A restore carries the device in its state and is only told
	// where its new socket is, so a fork and a migrated VM are reachable at
	// their own path without the guest noticing. The CID is the guest's own
	// name for itself and never leaves it, so every VM may use the same one.
	VsockCID uint32
	// Jail, where it is set, is where each VMM runs: chrooted into a directory
	// this process builds, as a user of its own. Nil runs every VMM as this
	// process's user, which isolates no VM from another. See Jail.
	Jail *Jail
}

// Jail runs each VMM the way Firecracker's own jailer does, which is what the
// isolated arena needs of it (plans/isolated-arena-2026-09-25.md): not as
// root, not as the host's user, and with nothing of the host's to reach.
//
// Root is a directory this process builds the first time a VMM starts. It holds
// the VMM, its seccomp filter, the kernel and initrd a boot names, and a /dev
// of its own on a tmpfs, whose kvm and userfaultfd nodes the VMMs' group may
// open. Each VMM runs chrooted there, in a directory of its own under vms that
// only its user may enter. It has no /proc, so it cannot reopen a descriptor
// it holds read-only for writing, nor reach another process.
//
// Every running VMM has a user of its own out of UIDs users from FirstUID, so no
// VMM may signal, trace or read another, and they share the group GID, which
// owns the devices. A user is taken when a VMM starts and given back when it
// has exited, and a start with every user taken is refused. The VMM must be a
// static binary: the jail holds no libraries.
type Jail struct {
	Root                string
	FirstUID, UIDs, GID int

	mu    sync.Mutex
	built bool
	next  int
	inUse map[int]bool
}

// Boots reports a Firecracker configured with a kernel.
func (f *Firecracker) Boots() bool { return f.Kernel != "" }
