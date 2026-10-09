---
id: TASK-122.8
title: 'Keep a published page''s version in the spill file, and load it from there'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-09 09:25'
updated_date: '2026-10-09 09:53'
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
Step 1 of plans/local-writeback-2026-10-09.md, after the decision there that the log is the spill file's fixed slots rather than an append log. A checkpoint's page whose bytes are in its reservation when the checkpoint retires keeps that slot under its identity instead of freeing it, and a load reads a version the spill holds before the store. Published versions are dropped oldest first whenever a reservation wants a slot. A slot's bytes count as a version only while no store can have changed the page since they were written, so a refault that hands the guest its page writable drops them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A test publishes a spilled page, evicts it and refaults it from the spill file, with no backing read
- [ ] #2 A reservation takes the slot of the oldest published version when no slot is free, and the dirty budget is unchanged
- [ ] #3 A refault that hands the guest its page writable drops the slot's bytes, and a test proves a stale version is never loaded
- [ ] #4 Every vmmemory suite and campaign passes; docs/vm-memory.md describes the published versions
<!-- AC:END -->
