---
id: TASK-57
title: Make nested KVM safe for guests with managed RAM
status: In Progress
assignee: []
created_date: '2026-09-27 01:51'
updated_date: '2026-09-27 01:59'
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Research 2026-09-27, plan in plans/nested-kvm-2026-09-27.md: mainline 7.3-rc4 and kvm-x86 next still map the nested APIC-access, virtual-APIC and posted-interrupt pages with kvm_vcpu_map (outside the MMU notifier). Griffoul's pfn-cache series (v4, 2026-01-02) fixes that but is unmerged. Firecracker saves no nested state. Recommended: carry the series in the host kernel + save nested state in Firecracker. Waiting on the owner: can hosts run a patched kernel; Intel only or AMD too.
<!-- SECTION:NOTES:END -->
