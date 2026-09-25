---
id: TASK-2.1
title: Prove the kernel allows the split arena's mappings
status: To Do
assignee: []
created_date: '2026-09-25 23:02'
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
