---
id: TASK-2.6
title: Measure both arena modes on GCE
status: Done
assignee: []
created_date: '2026-09-25 23:02'
updated_date: '2026-09-28 01:30'
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
- [x] #2 The owner decides the default from the numbers
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26, recorded in docs/measurements/arena-modes-2026-09-26.md. Both modes measured on the demo cluster with scripts/demo-gce.sh arena (new, scripts/lib/demo-arena.sh), switching by SPROUTFS_DEMO_ARENA=isolated redeploy. Shared vs isolated: fan-out of 5 pause 0.111/0.116 s, total 9.23/10.24 s, first output 9.99/10.93 s; saved on the fan-out host RAM 1470/1466 MiB, PMEM 62/66 MiB; mappings per VMM 118 mean 131 most / 112 mean 116 most; 1 GiB capture pause 5-8 ms both, upload mean 7.07/7.63 s, host CPU mean 11.86/12.84 s (+8%); restore away start 0.77/0.82 s, first output 2.11/2.51 s, 1 GiB read back 14.5/14.7 s, 371/373 mappings; restore back start 0.76/1.64 s. One run each, RAM at 2 MiB pages; the 16 GiB capture at 4 KiB was not possible on the demo node. The demo runs VMMs as root, so isolated there protects nothing. Default left at shared. Found and fixed on the way: saved bytes wrapped to ~16 EiB with idle pages (4f1ea4a9).

Worst-case run 2026-09-26, recorded in docs/measurements/arena-worst-case-2026-09-26.md (scripts/demo-gce.sh arena-worst, scripts/lib/demo-arena-worst.sh; both modes and both RAM pages in one 19-minute run). Shared vs isolated, 2 MiB, median of 3: fork of 357 MiB unpublished into 3 local children, total 10.71/11.43 s, 236 fork copies; first inheritance of 786 MiB by 2 children, fork 1.70/2.50 s, 428 moves, owner re-read 1.58/2.06 s with 17/410 faults; restore back start 0.756/1.652 s (real: populate maps the 549 pages left in the old private file and moves 519 of them); 3.4 GiB all-dirty capture upload 16.65/18.67 s, CPU 32.9/36.0 s (once). 4 KiB, once each: inheritance children read 6.5/35.2 s (224,316 moves, one revocation per page), capture upload 30.6/65.7 s and CPU 67/138 s (cause not found; needs a profile), fork 19.6/33.9 s. Open: after a move the owner's next access to each page is a store fault and copies it (403 copy-on-writes for 428 moves), so a move saves no memory (saved 812 vs 1248 MiB; 4 KiB host pod at 7985 of 8192 MiB). In both modes a local fork of unpublished pages shares nothing: each child re-uploads and re-reads its inherited pages. Fixed: pager deadlock between eachBinding and joinsRun (d12ed98d). Default left at shared.

Worst-case run 2026-09-26: docs/measurements/arena-worst-case-2026-09-26.md. Follow-ups: move copies (high), local fork sharing (high, both modes), batched moves, 4 KiB CPU profile.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Both modes measured on GCE (docs/measurements/arena-modes-2026-09-26.md, arena-worst-case-2026-09-26.md). The owner chose isolated as the default on 2026-09-27: ArenaIsolated is the zero value, SPROUTFS_ARENA defaults to isolated, and just check runs the mode-sensitive packages in shared too (032a8e9c). Isolated's extra CPU at 4 KiB is TASK-51/52, deferred.
<!-- SECTION:FINAL_SUMMARY:END -->
