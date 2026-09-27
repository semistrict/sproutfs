---
id: TASK-58
title: >-
  Offer nested KVM as an experimental per-VM mode with a safe subset of
  operations
status: In Progress
assignee: []
created_date: '2026-09-27 02:21'
updated_date: '2026-09-27 18:09'
labels:
  - embedder
  - security
dependencies: []
priority: high
ordinal: 65000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Until the host kernel carries the nested pfn-cache work (TASK-57, plans/nested-kvm-2026-09-27.md), KVM writes the pages a nested guest names in its VMCS (virtual-APIC, APIC-access, posted-interrupt descriptor; vmcb12 on AMD) through a long-term pin, behind the host page tables. The owner decided on 2026-09-27 to offer nested KVM now as an experimental mode chosen at create, restricted to operations that never write-protect, free or replace a page of the VM's RAM: running, disk checkpoints, cold stop and start, recovery by cold boot, create from a disk-only checkpoint, and a drain that reboots the VM elsewhere. Capture, suspend, fork, live migration and keeping a checkpoint with memory are refused, and the pager never evicts, spills, gives back or moves the VM's RAM, which stays fully resident. When the mode is off, the guest must not be offered VMX or SVM at all. The code must say, next to each limitation, why it exists.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A create can ask for nested mode (host API, orchestrator, CLI), marked experimental in the API docs and the CLI help; the mode persists with the VM across stop, start and recovery
- [ ] #2 A guest in nested mode sees VMX (or SVM) in CPUID; a guest without it sees neither, proven by a Firecracker test
- [x] #3 Capture, suspend, fork, live migration and keep are refused for a nested VM with an error that says why; a drain stops it cold and starts it elsewhere
- [x] #4 The pager never evicts, spills, gives back or moves a nested VM's RAM, and admits the VM only with its whole RAM resident
- [x] #5 Each limitation has a comment next to it explaining why, and docs/hosting.md and docs/vm-memory.md describe the mode
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented 2026-09-27. Checkpoint root records nested (field 11, inherited like vcpus); create sets it (--nested, API nested); vmmachine offers VMX/SVM in CPUID only to nested VMs and hides both otherwise (vmmachine/nested.go, amd64 only, refused elsewhere); nested RAM attaches as a fixed region (vmmemory/fixed.go: never sealed, evicted, given back or moved; admitted only with room left to evict); host refuses capture, suspend, fork, capture-into and live migration (host/nested.go, 400); orchestrator migrate/drain of a nested VM stops it and boots it cold on the destination (rebooted). Proven: pager, host, orchestrator, CLI and configuration unit tests; Lima: a nested VM is refused on aarch64. AC2 (a nested x86 guest sees VMX/SVM, a plain one neither) waits on the x86 run of TestOnlyANestedGuestIsOfferedHardwareVirtualisation.

x86 GCE run 2026-09-27 (3rd): a plain guest sees no vmx/svm (passes); the nested guest also sees none, and its kernel logs nothing about VMX; host kvm_intel.nested=Y. Cause: the test's guest kernel is Firecracker's CI 6.18 kernel, built with CONFIG_VIRTUALIZATION unset, so no CONFIG_KVM_INTEL. Linux's feat_ctl.c then leaves VMX disabled in IA32_FEATURE_CONTROL and clears X86_FEATURE_VMX silently (it prints only when KVM_INTEL is built). So /proc/cpuinfo cannot show the flag whatever CPUID offers. Next: the nested test boots a guest kernel with KVM (and KVM_INTEL/KVM_AMD) and proves the nested guest can open /dev/kvm and create a VM, and that a plain one has no vmx.
<!-- SECTION:NOTES:END -->
