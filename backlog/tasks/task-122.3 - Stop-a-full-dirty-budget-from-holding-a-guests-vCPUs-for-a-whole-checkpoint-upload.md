---
id: TASK-122.3
title: >-
  Stop a full dirty budget from holding a guest's vCPUs for a whole checkpoint
  upload
status: To Do
assignee: []
created_date: '2026-10-09 00:08'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: bug
ordinal: 159000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A store that finds the dirty budget full waits in Host.takeSpill (vmmemory/pressure.go) until a checkpoint lands anywhere on the host, which is its whole upload to the object store. The default PMEM dirty budget is min(arena, spill) pages, so on the embedder's shape it is the 6.4 GiB arena, and a guest whose dirty set nears that writes at the speed of its uploads. In the PostgreSQL benchmark (TASK-122) pgbench's 140 s saw 43,691 dirty waits and 7 checkpoints asked for by pressure, 2.2 GB uploaded, and 1,136 tps against plain GCE's 6,614 at the same 8 GiB and 4 CPUs; the 12 GiB file phase saw 10,796. A vCPU held in such a wait holds the guest's kernel with it: Firecracker drops a vsock CONNECT the guest does not accept in time, so the host's exec gets EOF ('the guest is not answering'), which is the embedder's health probe failing and, sustained, their freeze. Options to measure: a dirty budget that defaults to the spill file rather than the arena; giving reservations back as each sealed page lands rather than at the publication's end; pacing stores rather than parking them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The benchmark's dirty waits, probe failures and tps are measured before and after
- [ ] #2 No probe of a guest under the benchmark fails or takes more than a second for want of dirty budget
- [ ] #3 docs/vm-memory.md states what a full dirty budget costs a guest
<!-- AC:END -->
