---
id: TASK-104.8
title: >-
  Fsync journal step 8: prove in the simulation that an answered flush survives
  its host
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 19:10'
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Bugs the step 8 campaign found, each fixed on its branch with a test: a recovery with an unreachable journal holder failed (edeb0ad9); a commit over a quarter of the ring could wait an hour (f700fa3c); room was asked of the VMs holding the most bytes, not the oldest (41b54e1d); a VMM's re-sent held flushes were answered before the destination registered it (6c424c28, data loss on a migration); a commit that did not fit beside a failed range waited forever (50439501); commits placed ahead could use a commit's room and nobody asked again (01aea76a); a VM whose journal held nothing waited forever once its disk was let go (d02fd5df). Open: fingerprint arm, create and delete failures in the campaign, Gremlins.
<!-- SECTION:NOTES:END -->
