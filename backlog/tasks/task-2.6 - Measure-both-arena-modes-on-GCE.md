---
id: TASK-2.6
title: Measure both arena modes on GCE
status: To Do
assignee: []
created_date: '2026-09-25 23:02'
updated_date: '2026-09-26 19:18'
labels:
  - measurement
  - gce
dependencies: []
parent_task_id: TASK-2
priority: high
type: task
ordinal: 42000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 6 of plans/isolated-arena-2026-09-25.md: one GCE run comparing shared and isolated on the same workloads, then the docs.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Fan-out first output, checkpoint pause, upload time and CPU, restore time, mappings per guest and saved memory are recorded for both modes
- [ ] #2 The owner decides the default from the numbers
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26, recorded in docs/measurements/arena-modes-2026-09-26.md. Both modes measured on the demo cluster with scripts/demo-gce.sh arena (new, scripts/lib/demo-arena.sh), switching by SPROUTFS_DEMO_ARENA=isolated redeploy. Shared vs isolated: fan-out of 5 pause 0.111/0.116 s, total 9.23/10.24 s, first output 9.99/10.93 s; saved on the fan-out host RAM 1470/1466 MiB, PMEM 62/66 MiB; mappings per VMM 118 mean 131 most / 112 mean 116 most; 1 GiB capture pause 5-8 ms both, upload mean 7.07/7.63 s, host CPU mean 11.86/12.84 s (+8%); restore away start 0.77/0.82 s, first output 2.11/2.51 s, 1 GiB read back 14.5/14.7 s, 371/373 mappings; restore back start 0.76/1.64 s. One run each, RAM at 2 MiB pages; the 16 GiB capture at 4 KiB was not possible on the demo node. The demo runs VMMs as root, so isolated there protects nothing. Default left at shared. Found and fixed on the way: saved bytes wrapped to ~16 EiB with idle pages (4f1ea4a9).
<!-- SECTION:NOTES:END -->
