---
id: TASK-122.10
title: 'Take checkpoints from the log, so a guest never waits for an upload'
status: To Do
assignee: []
created_date: '2026-10-09 09:25'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 166000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of plans/local-writeback-2026-10-09.md. A seal write-protects what is still Dirty and lets the writer write it; the checkpoint is a list of log versions; a store into a page the checkpoint holds writes a new version instead of copying in memory; the upload reads the log, paced behind the guest. The dirty budget bounds only pages the writer has not reached; the log's capacity and the loss window bound unpublished data. Absorbs TASK-122.3.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 No probe of the benchmark's guest takes more than 1 s
- [ ] #2 Copies after a seal fall to near zero on pgbench
- [ ] #3 docs/architecture.md and docs/vm-memory.md say where a checkpoint's bytes come from
<!-- AC:END -->
