---
id: TASK-57
title: Make nested KVM safe for guests with managed RAM
status: In Progress
assignee: []
created_date: '2026-09-27 01:51'
updated_date: '2026-09-27 19:54'
labels:
  - security
  - embedder
dependencies: []
priority: high
ordinal: 64000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TASK-53 found that KVM's kvm_vcpu_map maps for a nested guest write guest RAM without going through the host page tables: on Intel the virtual-APIC page, posted-interrupt descriptor and APIC-access page while L2 runs, on AMD vmcb12 and hsave around VMRUN. A seal, a settle, a copy-on-write, an eviction or a give-back can then miss such a write. Firecracker also never saves nested state, so a guest running a nested hypervisor breaks at every capture, migration and fork. The owner requires nested virtualisation to work for guests with managed RAM; hiding it is not an option. Find out what current kernels do (whether these maps now use gfn_to_pfn_cache or another path the MMU notifier invalidates), and what the pager, KVM and Firecracker must do so that every write reaches the pager and every snapshot holds the nested state.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every nested-KVM write to guest RAM is either seen by the pager's write-protection or the pager keeps those pages out of seal, copy-on-write, eviction and give-back, with the reason written in docs/vm-memory.md
- [ ] #2 Firecracker saves and restores nested state (KVM_GET/SET_NESTED_STATE) in every managed snapshot, so capture, fork and migration of a guest running a nested VM keep that VM running
- [ ] #3 A Linux test runs a nested guest inside a managed-RAM VM through a capture, a fork and a migration, and its memory reads back what it wrote
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Firecracker fork (branch sproutfs): for a nested VM, after CPUID, narrow MSR_IA32_VMX_TRUE_PINBASED_CTLS (clear posted interrupts), MSR_IA32_VMX_TRUE_PROCBASED_CTLS (clear TPR shadow) and MSR_IA32_VMX_PROCBASED_CTLS2 (clear virtualize APIC accesses, APIC-register virtualization, virtual-interrupt delivery, virtualize x2APIC mode) with KVM_SET_MSRS, and refuse to start if KVM will not take them.
2. Fork: save and restore the VMX capability MSRs and KVM_GET/SET_NESTED_STATE in every snapshot of a nested VM, capabilities before nested state.
3. sproutfs: drop the experimental-mode limits that exist only because of those pins (fixed regions, refused capture/suspend/fork/migrate, reboot-on-drain), keeping the mode Intel-only and create-time.
4. Tests: a guest-witness subcommand that runs a tiny L2 (a few instructions and HLT) through KVM_RUN inside a nested VM; x86 GCE tests that L2 runs, that L1 sees the narrowed controls, that a vmcs12 setting TPR shadow fails entry, and that a nested VM running L2 survives capture, fork and migration with its memory intact.
5. Docs: docs/vm-memory.md writers section and docs/hosting.md nested section.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Research 2026-09-27, plan in plans/nested-kvm-2026-09-27.md: mainline 7.3-rc4 and kvm-x86 next still map the nested APIC-access, virtual-APIC and posted-interrupt pages with kvm_vcpu_map (outside the MMU notifier). Griffoul's pfn-cache series (v4, 2026-01-02) fixes that but is unmerged. Firecracker saves no nested state. Recommended: carry the series in the host kernel + save nested state in Firecracker. Waiting on the owner: can hosts run a patched kernel; Intel only or AMD too.

Owner decisions 2026-09-27: hosts cannot run a patched kernel (so no Woodhouse pfncache series); an L1 guest kernel may be patched; Intel only; slower L2 (more exits, notably interrupts) is acceptable. Route: no host kernel change. The Firecracker fork lowers the VMX capability MSRs it gives a nested VM's vCPUs (KVM lets userspace clear allowed-1 bits before the vCPU runs) so L1 is never offered the features that make L0 map L1 pages behind the host page tables (TPR shadow/virtual-APIC page, APIC-access virtualization, posted interrupts, MSR bitmaps, VMCS shadowing, and whatever else the audit finds); L0 then fails VM entry for any L1 that sets them. Plus KVM_GET/SET_NESTED_STATE in the fork for capture and migration. Stand-in host kernel: Ubuntu 26.04 GCE 7.0.0-1011-gcp. First step: audit that kernel's arch/x86/kvm/vmx/nested.c for every kvm_vcpu_map / gfn_to_pfn_cache / pin of L1 memory and the VMCS12 control that gates it.

Audit of Linux v7.0 (stand-in for 7.0.0-1011-gcp), arch/x86/kvm: the only long-lived maps of L1 memory outside the MMU notifier are in vmx/nested.c nested_get_vmcs12_pages: APIC-access page (vmcs12 SECONDARY_EXEC_VIRTUALIZE_APIC_ACCESSES), virtual-APIC page (CPU_BASED_TPR_SHADOW), posted-interrupt descriptor (PIN_BASED_POSTED_INTR); plus the enlightened VMCS map, only when userspace enables Hyper-V eVMCS (Firecracker does not). The MSR bitmap is mapped read-only only for the length of one nested entry (nested_vmx_prepare_msr_bitmap maps, copies, unmaps). Every other L1 access (vmcs12, shadow vmcs12, I/O and VMREAD/VMWRITE bitmaps, MSR load/store lists, PML, EPTP list) copies through the hva. Outside nested, x86.c/lapic.c use only gfn_to_pfn_cache (MMU-notifier invalidated). vmx_set_vmx_msr lets userspace narrow the true pin/proc controls and PROCBASED_CTLS2 to a subset while the vCPU is not in VMX operation, and nested_check_vm_execution_controls fails any vmcs12 that sets a bit outside them. Firecracker cannot do it through a CPU template (msr_modifiers only apply to the boot MSR set) and leaves VMX MSRs out of snapshots (UNDUMPABLE_MSR_RANGES), so the fork needs code.
<!-- SECTION:NOTES:END -->
