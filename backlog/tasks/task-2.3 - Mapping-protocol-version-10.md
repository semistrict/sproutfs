---
id: TASK-2.3
title: Mapping protocol version 10
status: To Do
assignee: []
created_date: '2026-09-25 23:02'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: task
ordinal: 39000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of plans/arena-by-trust-2026-09-25.md: FILE, DROP_FILE and the file number in MAP; the Rust client's file table and private mappings of read-only files; Firecracker rebuilt with the crate and the seccomp change. The pager still sends one read-write file.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Frame tests on both sides, Rust unit tests and new vmtest cases pass
- [ ] #2 The hostile suite and the Firecracker suite pass in Lima
<!-- AC:END -->
