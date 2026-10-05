---
id: TASK-92.13
title: 'Zircon port step 13: switch the pager to the new core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.12
references:
  - plans/zircon-pager-port-2026-10-05.md
  - docs/measurements/gce-fault-first-2026-10-04.md
  - docs/measurements/gce-random-fault-planning-2026-10-04.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 111000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 13 of the plan. The new core becomes the default only once it is measured against the old one on the runs the pager's design was tuned on: dependent and forward fault chains at 4 KiB and 2 MiB from the cluster and the store, the 4 KiB capture pause, a fork fan-out and a warm restore. The owner sets the gate (decision 5 of the plan).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A dated report in docs/measurements compares both cores on GCE for the runs the plan lists, with the median and spread of each
- [ ] #2 The results meet the gate the owner set, recorded in TASK-92
- [ ] #3 The default core is zircon, the old core runs in the second pass of just check, and the host logs show the new core on a GCE host
<!-- AC:END -->
