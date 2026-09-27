package host

import (
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/volume"
)

// A nested VM is one whose guest may run VMs of its own. It is experimental,
// and only an x86_64 host runs one (see vmmachine's nested.go).
//
// Why it is limited. While a guest runs a VM of its own, KVM writes the pages
// that guest names in its VMCS — the virtual-APIC page, the APIC-access page
// and the posted-interrupt descriptor on Intel, the vmcb12 on AMD — through a
// pin that the host page tables do not govern. Everything that captures a
// guest's RAM, or moves it, works through the page tables: a seal
// write-protects the pages, and the pager evicts, moves and gives pages back
// by changing where the guest's mappings point. Each would miss a write KVM
// makes, and the guest would come back with memory older than what it wrote.
// Firecracker also saves no nested state, so a resumed guest would lose the
// VM it was running.
//
// So a nested VM keeps its RAM resident and in place (vmmemory's fixed
// regions), and this host refuses everything that captures its RAM: a
// capture, a suspending stop, a fork, a capture into a new VM, and a live
// migration. What it can do is everything that never touches its RAM: run,
// take the interval's checkpoints of its disks, stop and start cold, recover
// by a cold boot, and be created from a checkpoint of its disks. A drain
// stops it cold and starts it on another host, which is a reboot for its
// guest.
//
// The limits are the kernel's. Once hosts run a kernel whose KVM puts those
// pages behind the MMU notifier (plans/nested-kvm-2026-09-27.md, TASK-57),
// and Firecracker saves nested state, a nested VM can be captured and moved
// like any other.

// ErrNested refuses an operation that captures or moves a nested VM's RAM.
var ErrNested = errors.New("host: a nested VM's RAM is never captured or moved (nested VMs are experimental)")

// refuseNested refuses what captures or moves the RAM of a nested VM, before
// the VM is touched. what names the operation for the error.
func refuseNested(vm *volume.VM, what string) error {
	if vm != nil && vm.Nested() {
		return fmt.Errorf("%w: %s of %s", ErrNested, what, vm.ID())
	}
	return nil
}
