---
id: TASK-58
title: >-
  Offer nested KVM as an experimental per-VM mode with a safe subset of
  operations
status: Done
assignee: []
created_date: '2026-09-27 02:21'
updated_date: '2026-09-27 20:06'
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
- [x] #2 A guest in nested mode sees VMX (or SVM) in CPUID; a guest without it sees neither, proven by a Firecracker test
- [x] #3 Capture, suspend, fork, live migration and keep are refused for a nested VM with an error that says why; a drain stops it cold and starts it elsewhere
- [x] #4 The pager never evicts, spills, gives back or moves a nested VM's RAM, and admits the VM only with its whole RAM resident
- [x] #5 Each limitation has a comment next to it explaining why, and docs/hosting.md and docs/vm-memory.md describe the mode
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented 2026-09-27. Checkpoint root records nested (field 11, inherited like vcpus); create sets it (--nested, API nested); vmmachine offers VMX/SVM in CPUID only to nested VMs and hides both otherwise (vmmachine/nested.go, amd64 only, refused elsewhere); nested RAM attaches as a fixed region (vmmemory/fixed.go: never sealed, evicted, given back or moved; admitted only with room left to evict); host refuses capture, suspend, fork, capture-into and live migration (host/nested.go, 400); orchestrator migrate/drain of a nested VM stops it and boots it cold on the destination (rebooted). Proven: pager, host, orchestrator, CLI and configuration unit tests; Lima: a nested VM is refused on aarch64. AC2 (a nested x86 guest sees VMX/SVM, a plain one neither) waits on the x86 run of TestOnlyANestedGuestIsOfferedHardwareVirtualisation.

x86 GCE run 2026-09-27 (3rd): a plain guest sees no vmx/svm (passes); the nested guest also sees none, and its kernel logs nothing about VMX; host kvm_intel.nested=Y. Cause: the test's guest kernel is Firecracker's CI 6.18 kernel, built with CONFIG_VIRTUALIZATION unset, so no CONFIG_KVM_INTEL. Linux's feat_ctl.c then leaves VMX disabled in IA32_FEATURE_CONTROL and clears X86_FEATURE_VMX silently (it prints only when KVM_INTEL is built). So /proc/cpuinfo cannot show the flag whatever CPUID offers. Next: the nested test boots a guest kernel with KVM (and KVM_INTEL/KVM_AMD) and proves the nested guest can open /dev/kvm and create a VM, and that a plain one has no vmx.

AC2 fix, 2026-09-27. The x86 failure was the guest kernel, not the CPUID. The Firecracker CI 6.18 kernel has CONFIG_VIRTUALIZATION off, so feat_ctl.c never enables VMX and clears the vmx flag. The old plain-guest check was vacuous on x86 for the same reason. scripts/lib/nested-kernel.sh builds kernel.org linux 6.18.44, checked by sha256, from the CI config with VIRTUALIZATION, KVM, KVM_INTEL and KVM_AMD built in, and fails if olddefconfig drops any.
bench-memory-linux.sh builds that kernel only when the GCE qualification runs the nested test or everything, and passes it as SPROUTFS_FIRECRACKER_NESTED_KERNEL. Other tests keep the CI kernel. A narrowed run no longer installs nextest or clippy. The test boots both guests on that kernel. The nested one must show the vmx or svm of the host, and witness kvm, a new subcommand doing KVM_GET_API_VERSION and KVM_CREATE_VM, must create a VM. The plain one must show neither flag and have no /dev/kvm. The test skips only when the variable is unset.
Proven locally: go test, go vet for linux amd64 and arm64, just check, bash -n and shellcheck. On Lima, TestANestedVMIsRefusedOffX86 passes with the stronger plain-guest check, witness kvm creates a VM on the real /dev/kvm, and nested-kernel.sh cross-builds an x86-64 vmlinux with all four options set to y in about 7 min on 8 cores. AC2 still waits on the GCE x86 run.

x86 GCE 2026-09-27 (n2-standard-8, Ubuntu 26.04 7.0.0-1011-gcp, kvm_intel.nested=Y), guest kernel built by scripts/lib/nested-kernel.sh with KVM: TestOnlyANestedGuestIsOfferedHardwareVirtualisation PASS — the nested guest sees vmx and creates a VM on its /dev/kvm; a plain guest sees neither flag and has no /dev/kvm.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A create can ask for nested mode (host API, orchestrator, CLI; documented experimental). A nested VM is offered VMX in CPUID and every other VM has VMX/SVM hidden; nested is x86_64-only. Its RAM is a fixed pager region (never sealed, evicted, spilled, given back or moved), and capture, suspend, fork, capture-into and live migration are refused with ErrNested, while a drain stops it and cold-boots it elsewhere; each limit is commented with its reason and documented in docs/hosting.md and docs/vm-memory.md. Verified by pager, host, orchestrator and CLI unit tests, Lima (refused off x86) and x86 GCE (nested guest sees VMX and creates a VM, plain guest sees none). TASK-57 lifts the limits.
<!-- SECTION:FINAL_SUMMARY:END -->
