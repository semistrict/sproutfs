---
id: TASK-122.8
title: Replace the spill file with a log of page versions on the host's local disk
status: To Do
assignee: []
created_date: '2026-10-09 09:25'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 164000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 1 of plans/local-writeback-2026-10-09.md. zirconvm.SpillStorage becomes a log: segments appended in order, each page version with its CRC32C, an index from page to version, and collection of versions superseded and published. It holds only what the spill holds now, so nothing else changes; it keeps the spill's promise that space it hands out was allocated before.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The pager spills to the log, and every vmmemory suite and campaign passes as before
- [ ] #2 A test drives a log through append, supersede, collect and a full disk
- [ ] #3 docs/vm-memory.md describes the log where it describes the spill file
<!-- AC:END -->
