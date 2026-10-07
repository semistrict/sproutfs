---
id: TASK-104.15
title: A host serves a journal disk assigned to it up to 30 s late
status: To Do
assignee: []
created_date: '2026-10-07 18:56'
labels:
  - journal
  - membership
dependencies: []
references:
  - membership/view.go
  - host/journaldisks.go
  - docs/measurements
parent_task_id: TASK-104
priority: medium
type: bug
ordinal: 140000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found in the step 9 GCE run. A host re-reads the membership every 30 s (membership/view.go). After a journal disk was attached to it, it served the disk 26 s later on a recovery and 17 s later on a scale-up. The controller also took about 24 s of five-second passes to move a lost member's disk. A recovery after a powered-off node took 86.5 s, almost all of it the disk's move.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A host reads the membership again at once when the cloud attaches a journal disk to its machine, or the controller tells it; the delay is measured
- [ ] #2 The controller's steps for a lost member's journal disk take fewer passes, and the time from host loss to the disk being served is measured on GCE
<!-- AC:END -->
