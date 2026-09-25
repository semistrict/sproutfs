---
id: TASK-2.4
title: Private files and a read-only shared file
status: To Do
assignee: []
created_date: '2026-09-25 23:02'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: feature
ordinal: 40000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of plans/arena-by-trust-2026-09-25.md, in the trust mode only: a private file per memory region, the shared file sent read-only, the BLAKE3 digest in ReadDirty and the checked move, fork files, and the reach test.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 TestAHostileVMMReachesNoOtherVMsBytes passes in trust mode and fails in shared mode
- [ ] #2 Every suite passes in both modes
<!-- AC:END -->
