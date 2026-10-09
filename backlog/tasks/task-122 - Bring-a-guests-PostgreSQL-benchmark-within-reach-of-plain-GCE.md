---
id: TASK-122
title: Bring a guest's PostgreSQL benchmark within reach of plain GCE
status: In Progress
assignee: []
created_date: '2026-10-08 23:40'
updated_date: '2026-10-09 09:11'
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
Phase 0 (done): the benchmark on GCE beside plain at the same limits (217b540f); the jailed VMA budget freeze fixed (0e6a9b62, TASK-122.5); dirty budgets default to the spill file (68beaa9d). Plain 6,614 tps / sproutfs 2,290-2,346; random reads 23,825/s against ~80/s.
Phase 1, stop the stalls (2 MiB, what the embedder hits): TASK-122.3 release dirty reservations as each sealed part lands, so a store never waits for a whole upload; TASK-122.4 a refused mapping never parks a fault blind; TASK-122.1 refuse a host whose HugeTLB allotment cannot hold its arenas. Gate: no probe over 1 s for the whole benchmark, pgbench not worse.
Phase 2, refault locally (2 MiB): TASK-122.2 keep every VM's own publications and the pages it reads back on the host's disk, paced behind the guest's I/O and off the checkpoint's critical path, with a cache write budget the spill files do not spend. Gate: random reads at the 2 MiB ceiling (~130/s), checkpoints no slower, pgbench not worse. Decision point after it: 4 KiB pager (phase 3) or a non-DAX root for private database disks.
Phase 3, the root at 4 KiB as a page cache (RAM stays 2 MiB): 3a batched eviction, one revocation per run of victims and coalesced spill writes, with an asynchronous free reserve (TASK-122.7); 3b write-behind, dirty pages written to the spill in large sequential I/O ahead of need so evicting them is a drop; 3c adaptive read-ahead, one page for a random miss and growing runs for sequential; 3d the per-page fault cost after a seal measured and cut; the disk cache then holds 4 KiB pages. Gates: 12 GiB sequential write >= 300 MiB/s at 4 KiB; random reads >= 10,000/s; pgbench >= 80% of plain.
Phase 4, give a DAX guest's arena to its disk: TASK-122.6 admission by the RAM a guest holds, or one arena both pagers share. Gate: measured at the split it chooses.
Every step: a simulation campaign seed or test that fails before it, guards for its bugs, the benchmark (scripts/demo-gce.sh postgres, and SPROUTFS_POSTGRES_PLAIN=1) recorded in docs/measurements.
<!-- SECTION:PLAN:END -->
