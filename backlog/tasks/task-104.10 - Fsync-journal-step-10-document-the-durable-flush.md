---
id: TASK-104.10
title: 'Fsync journal step 10: document the durable flush'
status: To Do
assignee: []
created_date: '2026-10-07 00:49'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - docs
dependencies:
  - TASK-104.9
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: docs
ordinal: 134000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 10 of the plan. The documents describe a flush as durable only within the flush bound. Once the journal is built and measured they must say what it guarantees, where it lives and what it costs. This waits for the docs trim that is under way on 2026-10-06.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 docs/architecture.md states the durable flush mode, its guarantee, its EIO on failure and the narrower meaning of the loss window; docs/context.md defines durable flush mode, journal, journal disk, block, digest, entry, batch, position, capture, unjournaled and covered position
- [ ] #2 docs/metadata.md describes format 6; docs/hosting.md journal disks and their scaling; docs/vm-memory.md the capture; docs/migration.md the handoff; docs/testing.md the tests; a property in docs/properties/
- [ ] #3 deploy/README.md sizes journal disks from the step 9 measurement and lists the permissions the mode needs
<!-- AC:END -->
