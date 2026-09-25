---
id: TASK-2.1
title: Prove the kernel allows the split arena's mappings
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-25 23:21'
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
- [x] #1 vmtest cases prove each property on the Lima kernel
- [x] #2 Each property that fails is reported to the owner before step 4 starts
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Lima kernel 7.0.0-29-generic aarch64. SPROUTFS_VM_MEMORY_RUN='^TestReadOnly' scripts/test-vm-memory-lima.sh passes, five runs in a row, and the whole vmtest suite passes beside it. Every property holds at 4 KiB (shmem memfd) and 2 MiB (HugeTLB memfd):
- O_RDONLY reopen: writable MAP_SHARED and mprotect PROT_WRITE EACCES; pwrite and fallocate (allocate, punch) EBADF; ftruncate EINVAL; F_ADD_SEALS EPERM while the pager's descriptor can still seal; F_SETFL O_RDWR succeeds and leaves the access mode O_RDONLY.
- memfd is created 0777. nobody reopens /proc/self/fd/N O_RDWR at 0777, gets EACCES at 0600; fchmod EPERM; /proc/<pager>/fd EACCES; a process reopens its own 0600 memfd for writing.
- UFFDIO_REGISTER of MAP_SHARED of the read-only file: EPERM. MAP_PRIVATE|MAP_NORESERVE registers missing|wp|minor with the client's features, offering CONTINUE, WRITEPROTECT, WAKE.
- Faults: load of a held page minor (flags 4), load of a hole missing (0), store to installed page WP (3), first-access store minor|write (5) then WP (3); from a thread and from a KVM vCPU. CONTINUE with WP installs the pager's frame: same pagemap frame in pager and VMM, file and uffd-wp bits set, at both ends of a 2 MiB page. Clearing WP lets the store copy into an anonymous page; the file is unchanged.
- HugeTLB private MAP_NORESERVE: HugePages_Rsvd and Free unchanged on map, read and CONTINUE. Without MAP_NORESERVE Rsvd rises by one per page.
- Eight adjacent runs, one map+CONTINUE each: 4 KiB shared 1/1/1 (ascending/descending/interleaved), 4 KiB private 1/1/4, 2 MiB shared and private 8/8/8 (HugeTLB VMAs never merge, so 2 MiB is unchanged from today).
Not proven: GCE x86_64 kernel. The amd64 KVM guest code builds and vets but has not run. No property failed.

No property failed on the Lima kernel, so there is nothing to stop step 4 for. The results go to the owner with this task's report.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added internal/vmtest/readonly_linux_test.go (pager side), readonlyvmm_linux_test.go (a VMM process re-executed from the test binary) and a minimal Go KVM guest (kvm_linux*_test.go, arm64 and amd64). They prove on Lima kernel 7.0.0-29-generic aarch64, at 4 KiB and 2 MiB, every property step 1 of plans/arena-by-trust-2026-09-25.md names; the results and mapping counts are recorded in the plan's step 1. Verified with SPROUTFS_VM_MEMORY_RUN='^TestReadOnly' scripts/test-vm-memory-lima.sh five times in a row and the full vmtest suite once, all passing. GCE x86_64 remains to run at the end.
<!-- SECTION:FINAL_SUMMARY:END -->
