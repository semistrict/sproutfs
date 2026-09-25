package vmmachine

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
	// VCPUs is a booted guest's processors. A restore carries its own.
	VCPUs int
	// VsockCID is the guest context id of the VM's virtio-vsock device, and
	// zero a VM without one. The device's host end is a socket in the process's
	// directory. A restore carries the device in its state and is only told
	// where its new socket is, so a fork and a migrated VM are reachable at
	// their own path without the guest noticing. The CID is the guest's own
	// name for itself and never leaves it, so every VM may use the same one.
	VsockCID uint32
}

// Boots reports a Firecracker configured with a kernel.
func (f *Firecracker) Boots() bool { return f.Kernel != "" }
