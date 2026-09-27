---
id: TASK-57
title: Make nested KVM safe for guests with managed RAM
status: To Do
assignee: []
created_date: '2026-09-27 01:51'
updated_date: '2026-09-27 01:51'
labels:
  - security
  - embedder
dependencies: []
priority: medium
ordinal: 64000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TASK-53 found that KVM's kvm_vcpu_map maps for a nested guest write guest RAM without going through the host page tables: on Intel the virtual-APIC page, posted-interrupt descriptor and APIC-access page while L2 runs, on AMD vmcb12 and hsave around VMRUN. A seal, a settle, a copy-on-write, an eviction or a give-back can then miss such a write. Firecracker also never saves nested state, so a guest running a nested hypervisor breaks at every capture, migration and fork. The owner wants nested virtualisation to work safely rather than be hidden (the alternative is TASK-54). Find out what current kernels do (whether these maps now use gfn_to_pfn_cache or another path the MMU notifier invalidates), and what the pager and Firecracker must do so that every write reaches the pager and every snapshot holds the nested state.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every nested-KVM write to guest RAM is either seen by the pager's write-protection or the pager keeps those pages out of seal, copy-on-write, eviction and give-back, with the reason written in docs/vm-memory.md
- [ ] #2 Firecracker saves and restores nested state (KVM_GET/SET_NESTED_STATE) in every managed snapshot, so capture, fork and migration of a guest running a nested VM keep that VM running
- [ ] #3 A Linux test runs a nested guest inside a managed-RAM VM through a capture, a fork and a migration, and its memory reads back what it wrote
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Fallback if this proves infeasible: TASK-54 (hide VMX/SVM).
<!-- SECTION:NOTES:END -->
