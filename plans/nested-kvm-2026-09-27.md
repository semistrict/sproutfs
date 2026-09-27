# Nested KVM with managed RAM — 2026-09-27

Status: research done, no code yet. Backlog: TASK-57.

## The problem

A guest with managed RAM may run its own VMs. Two things break when it does.

**1. KVM writes some of the guest's pages behind the page tables.** On Intel,
when L1 runs L2, KVM maps three pages that L1 names in its VMCS:

- the APIC-access page (`apic_access_addr`);
- the virtual-APIC page (`virtual_apic_page_addr`);
- the posted-interrupt descriptor (`posted_intr_desc_addr`).

`nested_get_vmcs12_pages()` maps them with `kvm_vcpu_map()` on every nested
VM-entry. `nested_put_vmcs12_pages()` unmaps them only when L2 exits to L1.
While they are mapped, KVM and the CPU write them through a kernel mapping of
the physical page. The MMU notifier does not reach that mapping. So a
write-protect, a punch or a remap of the host page changes nothing for KVM,
which keeps writing the old physical page.

On AMD, KVM maps vmcb12 and the host save area the same way, but only for the
length of one VMRUN or one nested exit.

This is still true in mainline 7.3-rc4 (`arch/x86/kvm/vmx/nested.c`, checked
2026-09-27) and on the kvm-x86 `next` and `vmx` branches.

For the pager, that means any of these can miss a write to one of those pages:

- a seal, which then publishes stale bytes or reports tampering;
- a copy-on-write, a move or a give-back, which leaves KVM writing a page the
  guest no longer maps;
- an eviction, which frees the page while KVM still writes it.

The harm stays inside the guest that runs L2. No other VM's page is written,
because a freed slot is punched and its memory is not reused while KVM holds a
reference to it.

**2. Firecracker never saves nested state.** It does not call
`KVM_GET_NESTED_STATE` or `KVM_SET_NESTED_STATE`. A capture, a fork or a
migration of a guest in the middle of running L2 therefore restores it without
L2's state.

## What upstream is doing

Fred Griffoul (Amazon) posted "KVM: nVMX: Improve performance for unmanaged
guest memory", v4 on 2026-01-02. It replaces `kvm_vcpu_map()` with
`gfn_to_pfn_cache` for the three APIC pages, the MSR bitmap and the eVMCS. A
pfn cache is invalidated by the MMU notifier and refilled through a GUP that
takes the userfaultfd fault. With it, every write in item 1 reaches the pager.
It is not merged as of 7.3-rc4.

## The options

**A. Carry the pfn-cache series in the host kernel.** This is the only
complete fix for item 1. The embedder's hosts must then run a kernel with it,
until upstream merges it.

**B. Keep those pages away from the pager's page moves, without a kernel
change.** At each pause, Firecracker reads every vCPU's nested state, which
holds the vmcs12 and so the three addresses, and tells the pager which frames
KVM may hold. The pager then:

- treats those frames as always dirty at a seal;
- never evicts, moves, copies or gives them back.

This leaves a window. L1 can point its VMCS at new pages between two pauses,
and the pager will not know until the next pause. So B narrows the defect but
does not close it.

**C. Save nested state in Firecracker.** This fixes item 2 whichever of A or B
is chosen. It is a change on our Firecracker branch: get and set nested state
per vCPU in the snapshot, and keep the VMX capability MSRs and CPUID the same
on both ends of a restore.

Recommended: A and C. B is not safe enough to ship on its own.

## Decisions for the owner

1. Can the embedder's hosts run a kernel that carries the pfn-cache series,
   and which kernel version do they run now?
2. AMD hosts too, or Intel only? The AMD maps are short-lived but still race
   with a running give-back or eviction.

## How it is tested

It needs three levels: our host kernel with KVM, a Firecracker guest with
managed RAM, and a small L2 inside that guest. A GCE VM cannot give that,
because it would itself be a guest. A Spot `c3-standard-192-metal` costs about
$1.17 an hour at the time of writing. The test:

1. The L2 writes its memory continuously and takes many interrupts, so the
   virtual-APIC page and the posted-interrupt descriptor are in use.
2. The pager seals, gives back and evicts under it, and the outer guest is
   captured, forked and migrated.
3. L2 must keep running, and must read back what it wrote.
4. The same test must fail without A and C, to show it reaches the defect.
