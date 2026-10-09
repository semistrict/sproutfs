---
id: TASK-122.9
title: >-
  Write dirty pages back to the log behind the guest, so eviction drops clean
  pages
status: To Do
assignee: []
created_date: '2026-10-09 09:25'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 165000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of plans/local-writeback-2026-10-09.md. A writer moves Dirty pages to the log in runs, oldest first, before the evictor needs their slots; a page written back is Written and write-protected again, so the next store makes it Dirty, as a filesystem page is after writeback. The evictor takes Written pages and drops them, and its asynchronous path keeps free slots ahead of the faults; writing a page from inside a fault is the fallback.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 On the benchmark's write phases no eviction runs inside a fault
- [ ] #2 A campaign seed in which a writeback, a seal and an eviction meet fails before this and passes after
<!-- AC:END -->
