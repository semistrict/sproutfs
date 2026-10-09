---
id: TASK-122
title: Bring a guest's PostgreSQL benchmark within reach of plain GCE
status: In Progress
assignee: []
created_date: '2026-10-08 23:40'
updated_date: '2026-10-09 09:25'
labels:
  - performance
dependencies: []
priority: high
type: task
ordinal: 156000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder ran PostgreSQL in a sproutfs guest (8 GiB, 4 vCPUs, an 80 GiB DAX root on 2 MiB PMEM pages, a host with a 16 GiB arena 60% RAM and 32 GiB of spill on a 500 GB pd-ssd) and measured 2,503 tps against 8,738 on the same machine without a VM; on a later build the guest froze. scripts/demo-gce.sh postgres (scripts/lib/demo-postgres.sh) runs their benchmark on one host shaped as theirs, and SPROUTFS_POSTGRES_PLAIN=1 runs it on the node itself, held to 8 GiB and 4 CPUs in a systemd slice. Each gap the two runs show is a subtask; this task closes when the guest is within reasonable distance of plain on every phase.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The sproutfs and plain runs are recorded side by side in docs/measurements
- [ ] #2 Every gap between them has a subtask that closed it or explains why it stays
- [ ] #3 The guest never stops answering its probes for more than a few seconds
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Design: plans/local-writeback-2026-10-09.md. The pager works as the system it was ported from: Zircon's page layer under a user pager that writes back to a local disk, with checkpoints to object storage as the tier behind it.
Done: the benchmark beside plain GCE (217b540f); the jailed VMA budget freeze (0e6a9b62, TASK-122.5); dirty budgets default to the spill file (68beaa9d).
Now, small fixes that stand on their own: TASK-122.4 (a refused mapping never parks a fault blind), TASK-122.1 (HugeTLB allotment checked at start).
Steps: 1 the log (TASK-122.8); 2 keep what is written, refault from the log (TASK-122.2); 3 write back (TASK-122.9); 4 checkpoints from the log (TASK-122.10, absorbs TASK-122.3); 5 batch the page-table commands (TASK-122.7); 6 the root at 4 KiB with adaptive read-ahead (TASK-110); 7 the arena's split for DAX guests (TASK-122.6).
Every step: a test or campaign seed that fails before it, guards, the benchmark beside plain recorded in docs/measurements.
<!-- SECTION:PLAN:END -->
