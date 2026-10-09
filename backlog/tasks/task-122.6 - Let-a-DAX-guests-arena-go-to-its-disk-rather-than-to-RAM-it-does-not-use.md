---
id: TASK-122.6
title: Let a DAX guest's arena go to its disk rather than to RAM it does not use
status: To Do
assignee: []
created_date: '2026-10-09 01:43'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: medium
type: task
ordinal: 162000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
With rootflags=dax=always a guest's file cache is the PMEM arena, and the guest's own RAM holds little but its processes. But a host admits a VM only when its RAM arena has the guest's whole RAM free ('no host has 8589934592 bytes of memory free' at a 25% RAM share), so the embedder's 16 GiB arena gives RAM 60% and the guest's disk 6.4 GiB, against plain GCE's 8 GiB of page cache. At the 52% share that still admits an 8 GiB guest, the benchmark's 12 GiB write went from 329 to 698 MiB/s (plain: 454) and random reads from p50 16 ms to 2.6 ms; pgbench did not move (2,290 to 2,346 tps). Decide whether admission should charge a guest's RAM by what it holds rather than its size, or the two pagers share one arena.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The benchmark is measured at the split this chooses
- [ ] #2 docs/hosting.md says how to divide an arena for DAX guests
<!-- AC:END -->
