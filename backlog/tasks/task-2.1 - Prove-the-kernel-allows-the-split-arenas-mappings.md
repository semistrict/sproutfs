---
id: TASK-2.1
title: Prove the kernel allows the split arena's mappings
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-25 23:07'
labels:
  - security
  - testing
dependencies: []
parent_task_id: TASK-2
priority: high
type: spike
ordinal: 37000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 1 of plans/isolated-arena-2026-09-25.md. A gate: read-only private mappings of a shared memfd registered with userfaultfd, UFFDIO_CONTINUE installing them read-only, stores trapping from a thread and from KVM, pagemap showing one physical page in both processes, HugeTLB MAP_NORESERVE reserving no pool pages, another user's reopen through /proc/self/fd failing after fchmod 0600, and mapping counts.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 vmtest cases prove each property on the Lima kernel
- [ ] #2 Each property that fails is reported to the owner before step 4 starts
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Add internal/vmtest/readonly_linux_test.go: the test process plays the pager (memfd, fchmod 0600, O_RDONLY reopen through /proc/self/fd), and a re-executed copy of the test binary plays the VMM (MAP_PRIVATE|MAP_NORESERVE runs built away from the live range, registered for missing, minor and write-protect with the client's UFFD feature set, write-protected, then mremapped in). The VMM sends its UFFD to the pager, which drains REMAP events and resolves faults with vmwire.Resolve.
2. Cases, each at 4 KiB (shmem memfd) and 2 MiB (HugeTLB memfd): read-only descriptor refuses writable shared mmap, mprotect PROT_WRITE, write, fallocate, ftruncate, F_ADD_SEALS, and F_SETFL cannot add O_RDWR; a missing, a minor and a write-protect fault each arrive with exact flags; CONTINUE installs the pager's page read-only and pagemap shows the same PFN in both processes; a thread's store traps with WP; clearing WP lets the store copy into anonymous memory and the file stays unchanged.
3. A KVM vCPU in the VMM process (minimal Go KVM for arm64 and amd64, as the Rust fixture's kvm.rs) loads and stores through the private mapping: the store traps with WP.
4. HugePages_Rsvd before and after a private HugeTLB mapping with MAP_NORESERVE (no change), and without it (reserves one per page) as a control.
5. A VMM process running as nobody: reopening its read-only descriptor through /proc/self/fd for writing works at mode 0777 and fails with EACCES at 0600; fchmod fails with EPERM.
6. VMA count after N adjacent runs mapped and resolved one command each, ascending, descending and interleaved, against MAP_SHARED of a read-write descriptor.
7. Run in Lima under the shared lock with SPROUTFS_VM_MEMORY_RUN; record results in the task and plan step 1.
<!-- SECTION:PLAN:END -->
