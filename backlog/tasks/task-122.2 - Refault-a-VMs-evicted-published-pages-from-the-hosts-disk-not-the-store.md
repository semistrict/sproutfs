---
id: TASK-122.2
title: 'Refault a VM''s evicted published pages from the host''s disk, not the store'
status: To Do
assignee: []
created_date: '2026-10-08 23:57'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 158000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Outside the share the cluster cache is on for, a host keeps a VM's published pages on its disk only for a VM marked to pull its memory (docs/volumes.md, The page cache's disk). Every other VM's page that a checkpoint published and the evictor then dropped is read back from the object store on its next fault. In the PostgreSQL benchmark (TASK-122) the guest wrote a 12 GiB file over a 6.4 GiB PMEM arena and read it at random: 5,464 PMEM pages were loaded in 165 s, 7,244 GETs carrying 10.3 GB at 59 ms each, while the cache disk's 141 GB held nothing (sproutfs_cache_disk_used_bytes 0, no fills from reads or publications). Reads ran at 74 a second, p50 76 ms; plain GCE did 23,825 a second, p50 0.4 ms. A host alone should keep what its own VMs publish, and what it reads back, on its disk, as a pulled VM's are.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A VM that is not pulled refaults a page its own checkpoint published from the host's disk, which the benchmark's counters show (disk hits, no store GET for it)
- [ ] #2 A test proves a publication's pages reach the disk and a later fault reads them there
- [ ] #3 The benchmark's random reads are measured before and after
<!-- AC:END -->
