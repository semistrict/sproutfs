---
id: TASK-104.8
title: >-
  Fsync journal step 8: prove in the simulation that an answered flush survives
  its host
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - testing
dependencies:
  - TASK-104.7
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 132000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 8 of the plan, and the evidence for TASK-104 criterion 2. The deterministic simulation kills hosts at the moments that matter and opens the VM elsewhere, so a lost flushed write shows up as a wrong byte in a named test, under a seed that reproduces it.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 With the durable flush mode on, TestAFlushAnsweredJustBeforeItsHostDiesIsThereWhereTheVMOpensNext kills the host right after a flush is answered, recovers the VM on another host, and finds every block stored before the flush at its value or a later one
- [ ] #2 Variants kill between the sync and the answer, during a seal upload, during a post-copy (source, then destination), during a scale-down wait, and keep a fenced host running cut off
- [ ] #3 TestJournalsSurviveTheirFaultsAndReachTheirProbes runs Buggify sites for a slow write, a failed write, a torn batch, a detach mid-batch, a full ring, a capture racing a seal, a disk moving during recovery and a failed create, asserts each probe is reached, and asserts every flush either succeeds and survives or fails with EIO
- [ ] #4 A fingerprint arm with journals is stable under shake; guards journal-replay-any-epoch and journal-drop-source-early and those of steps 2, 4 and 5 are in scripts/mutation/guards.json and killed by check-guards.py; Gremlins before and after are recorded
<!-- AC:END -->
