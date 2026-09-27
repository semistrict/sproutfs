package vmmemory

import "errors"

// A fixed memory region is the RAM of a nested VM: a guest that may run VMs of
// its own. It is experimental, and plans/nested-kvm-2026-09-27.md is the whole
// story.
//
// Why it exists. When a guest runs a VM of its own, KVM maps the pages that
// guest names in its VMCS — on Intel the virtual-APIC page, the APIC-access
// page and the posted-interrupt descriptor, on AMD the vmcb12 — with
// kvm_vcpu_map, and it and the CPU write them through that mapping for as long
// as the nested VM runs. The mapping holds a pin on the physical page; it is
// not in the host page tables, and the MMU notifier does not reach it. So
// everything this pager does by changing the host page tables misses those
// writes:
//
//   - a seal write-protects the page, and KVM's write lands anyway, so a
//     checkpoint publishes bytes the guest has since changed;
//   - an eviction punches the page out of the arena, and KVM goes on writing
//     the pinned page nobody maps, so the guest's next read gets old bytes;
//   - a move and a give-back point the guest at another page, and KVM goes on
//     writing the old one.
//
// A fixed region therefore does none of those. Its pages stay resident, in the
// slot they were given, until it detaches: it is never sealed, never chosen as
// a victim, never given back and never moved. A copy-on-write is still safe,
// because KVM asks for a page writable before it pins it, and that write fault
// is what makes the copy; the page KVM pins is always the region's own.
//
// The limits are the kernel's, not this pager's. Upstream work to put those
// mappings behind the MMU notifier (David Woodhouse's nvmx-gpc and nsvm-gpc
// branches, after Fred Griffoul's series) would let every write reach the pager
// as a fault, and then a nested VM could be sealed, evicted and moved like any
// other. Until a host runs a kernel with it, this is the safe subset.
//
// A PMEM region is never fixed. A guest could name a page of its own disk in
// its VMCS, and then a seal of that disk could miss KVM's write; the harm stays
// in that guest's own disk, because no page of another VM is written.

// ErrFixed refuses what a fixed memory region must never do: be sealed, moved
// or given back. See the comment above for why.
var ErrFixed = errors.New("managed-memory region is fixed in place for a nested VM")

// Fixed reports a memory region whose pages stay resident and in place until
// it detaches.
func (r *MemoryRegion) Fixed() bool { return r.fixed }

// heldInPlaceLocked reports a resident page some fixed memory region maps,
// which nothing may evict. Caller holds h.mu.
func (pg *resident) heldInPlaceLocked() bool {
	for b := range pg.aliases.all() {
		if b.memoryRegion.fixed {
			return true
		}
	}
	return false
}
