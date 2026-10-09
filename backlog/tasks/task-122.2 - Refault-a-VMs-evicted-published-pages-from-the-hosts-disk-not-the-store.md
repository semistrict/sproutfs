---
id: TASK-122.2
title: 'Refault a VM''s evicted published pages from the host''s disk, not the store'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 23:57'
updated_date: '2026-10-09 11:32'
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
- [x] #1 A VM that is not pulled refaults a page its own checkpoint published from the host's disk, which the benchmark's counters show (disk hits, no store GET for it)
- [x] #2 A test proves a publication's pages reach the disk and a later fault reads them there
- [x] #3 The benchmark's random reads are measured before and after
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Done as steps 1 and 2 of plans/local-writeback-2026-10-09.md rather than through the page cache's disk: the spill file keeps published versions (at retire, TASK-122.8, and at eviction of an identity root's page), and every load reads a version before the backing. Measure the benchmark's random reads on GCE with scripts/demo-gce.sh postgres beside the 2026-10-09 baseline (74 reads/s, p50 76 ms) and plain (23,825/s).
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Measured 2026-10-09 with a pulled VM and the cache's write budget removed (the deployment's 1 TiB/day budget counts the spill files' writes on the device counter, so it had refused every cache write): random 8 KiB reads went from 73/s p50 75 ms to 155/s p50 13.8 ms, which is about what 2 MiB misses allow on a 240 MB/s disk. But writing every publication through to the disk (46 GB held after one run) on the disk the spill files use made checkpoints slow enough that pgbench -i took 532 s instead of 45 and pgbench ran at 1 tps with 357,046 dirty waits. Keeping published pages on the disk has to be paced behind the guest's own I/O and must not lengthen the checkpoint a waiting store depends on; and the write budget must not be spent by the spill files.

Benchmark 2026-10-09 on GCE (n2-highmem-8, pd-ssd 500 GB, us-east1-b), main at 0ce90b26 (steps 1 and 2): fio random 8 KiB reads 229/s, p50 2.4 ms, p90 141.6 ms, p99 367 ms (before: 74/s, p50 76 ms; plain 23,825/s, p50 0.4 ms); pgbench 2,643 tps (before 2,290; plain 6,614); pgbench -i 44 s; probes p50 0.23 s, max 5.1 s (before 25 s), 2 of 65 failed. The median read is served locally; p90 still goes to the store. Next: rerun with the version counters exported (d7c5be5f) to see why those reads miss.

Second run with the counters (d7c5be5f), same node: fio reads 283/s, p50 2.1 ms, p90 86.5 ms, p99 333 ms; pgbench 2,532 tps; probes max 14.8 s, none failed. PMEM pager over the whole run: 15,129 versions kept at retire and 7,407 written at eviction, but only 3,283 held at the end; 12,172 loads came from versions against 4,042 backing reads. Versions are pushed out by dirty reservations: the dirty budget is the whole spill file (68beaa9d) and checkpoints do not drain fast enough (4,375 dirty waits), so each new reservation drops the oldest version. That is the dirty wall of step 4 (TASK-122.10): with checkpoints taken from the spill file and a small dirty budget, the file's slots become versions. Node deleted.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A VM's published pages now refault from the host's spill file rather than the store: versions kept at retire (TASK-122.8) and written at eviction (01685e56), read before the backing; counters exported (d7c5be5f). On the benchmark, random reads went from 74/s p50 76 ms to 283/s p50 2.1 ms, with 12,172 loads from versions against 4,042 store reads. The tail still reaches the store because dirty reservations push versions out; that is TASK-122.10.
<!-- SECTION:FINAL_SUMMARY:END -->
