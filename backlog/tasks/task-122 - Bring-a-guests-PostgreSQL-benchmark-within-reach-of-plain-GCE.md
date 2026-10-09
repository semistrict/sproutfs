---
id: TASK-122
title: Bring a guest's PostgreSQL benchmark within reach of plain GCE
status: To Do
assignee: []
created_date: '2026-10-08 23:40'
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
