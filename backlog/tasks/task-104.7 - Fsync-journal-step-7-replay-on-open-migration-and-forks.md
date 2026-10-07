---
id: TASK-104.7
title: 'Fsync journal step 7: replay on open, migration and forks'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - host
dependencies:
  - TASK-104.5
  - TASK-104.6
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 131000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 7 of the plan. After a host loss the VM must not run anywhere until the flushed blocks its record names are back in its disks. A migration must keep the source journal named until the destination holds every page (owner decision 5), and a fork child has nothing to replay onto until its root is published.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An open other than a migration reads the entries of every journal the record names, in order and after each covered position, writes them into the volumes, cold boots if it wrote any, and publishes them in the Host.Starting checkpoint before the guest starts
- [ ] #2 An open fails with ErrJournalPending while a named disk is not served, and with ErrJournalLost when its generation differs; only an operator discard opens it then, and it logs what it discards
- [ ] #3 The handoff carries the unjournaled runs; the destination adds its journal at open, answers flushes only after the post-copy is done, asks for a checkpoint out of turn then, and drops the source journal at its first selection sealed after that
- [ ] #4 The source keeps a handed-off VM entries until the record no longer names its journal; a handoff is refused while the record names two journals
- [ ] #5 A fork child answers its flushes only after its root is selected
<!-- AC:END -->
