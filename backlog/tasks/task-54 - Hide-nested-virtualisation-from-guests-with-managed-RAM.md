---
id: TASK-54
title: Hide nested virtualisation from guests with managed RAM
status: To Do
assignee: []
created_date: '2026-09-27 00:09'
updated_date: '2026-09-27 01:51'
labels:
  - security
dependencies: []
priority: medium
ordinal: 61000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found by TASK-53: KVM's kvm_vcpu_map maps for a nested guest (Intel: the virtual-APIC page, posted-interrupt descriptor and APIC-access page while L2 runs; AMD: vmcb12 and hsave around VMRUN) write guest RAM without going through the host page tables. So a seal, a settle, a copy-on-write, an eviction and the give-back can all miss such a write. The harm stays inside the guest that runs a nested hypervisor, and Firecracker never saves nested state, so such a guest already breaks at every capture. Firecracker passes the host's VMX through when no CPU template is set, and kvm_intel.nested is on by default. aarch64 is not exposed. Hiding VMX/SVM changes what guests can run, so the owner decides.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner has decided whether guests with managed RAM may see VMX/SVM
- [ ] #2 If hidden: a guest with managed RAM sees no VMX or SVM in CPUID on x86, proven by a Firecracker test
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Alternative to TASK-56 (make nested KVM safe). The owner prefers making it safe; this stays as the fallback.
<!-- SECTION:NOTES:END -->
