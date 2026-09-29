---
id: TASK-66
title: Compare an LSVD-style log layout against the index-object layout
status: Done
assignee:
  - '@claude'
created_date: '2026-09-29 01:23'
updated_date: '2026-09-29 01:54'
labels: []
dependencies: []
references:
  - 'https://doi.org/10.1145/3492321.3524271'
ordinal: 74000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Ramon wants to know whether the storage format from Hajkazemi et al., 'Beating the I/O Bottleneck: A Case for Log-Structured Virtual Disks' (EuroSys '22), would serve Sproutfs better than the current one. In LSVD, data objects describe themselves in a header, there is no per-commit index, and the in-memory map is rebuilt on mount by replaying headers from the last map checkpoint. Sproutfs writes a segmented page table and a root into an index object on every checkpoint. LSVD's write path, which logs every block write to local SSD, does not apply here: Sproutfs disks are PMEM mapped through the pager and there is no write stream. So the experiment covers the object format and its costs: publish, open, cold read, fork and storage.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 An experimental log layout publishes, opens (by replay from its last map object), reads, locates and compacts checkpoints, and reads back the same bytes as the index layout under the same workloads, including forks
- [x] #2 A comparison runs both layouts over identical page workloads on the simulated object store and reports requests, bytes and modelled latency for publish, open, cold read and fork, and stored bytes over time
- [x] #3 A dated measurement report in docs/measurements records the numbers and what they mean for the choice
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Part tables may carry a LogRecord (root without segments, parent, map sequence, zeroed pages); only the last part of a log-layout checkpoint sets it.
2. checkpoint.Config.Log selects the log layout: Commit writes no index object; the record rides in the last part; a map object (an index object holding every segment) is written for a VM's first checkpoint, a fork's first, and every MapEvery checkpoints.
3. A log-layout Index holds every segment decoded, shared copy-on-write with its parent, so Locate, ReadPages and compaction run unchanged.
4. Open lists the VM's objects, reads the last map at or before the checkpoint and every part table after it, and replays the records along the parent chain.
5. Reclaim in the log layout deletes only what precedes the current map and nothing names.
6. Correctness test: both layouts read back the byte model across generations, forks, zeroed pages, shrinks and compaction, fresh and reopened.
7. Comparison test (env-gated): identical workloads on the sim store in a synctest bubble; report requests, bytes and modelled latency for publish, open, cold read, fork, and stored bytes.
8. Write docs/measurements report.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Log layout lives in checkpoint/log.go behind checkpoint.Config.Log; the log record rides in PartTable.log of a checkpoint's last part. Both layouts now share the parent's decoded segments copy-on-write (Publication.segmentFor): without it every index-layout checkpoint refetched each segment it rewrote serially, ~30 GETs and ~300 ms. Two tests changed expectation for that: TestCompactionMeasuresLivenessFromTheRootAlone now starts its last checkpoint from a reopened index, and TestCacheSharesInheritedPagesAndAccountsHits expects one hit fewer. Compaction reads each moved page with its own serial GET in both layouts (~13k GETs at 32k dirty pages); flagged as separate work, not fixed here.
Validation: TestLogLayoutMatchesIndexLayout (lockstep byte model and identical Locate across layouts, reopened roots byte-identical; mutation of replay's zeroed-page and shrink handling both caught), TestLogOpenSkipsAnUnselectedCheckpoint, TestLogOpenReadsATableLongerThanTheTail, TestLogOpenOfAbsentCheckpoint; go test ./... green; SPROUTFS_ARENA=shared subset green; go vet linux+darwin; buf lint; determinism. just compare-layouts ran in 132 s.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added an experimental LSVD-style log layout to the checkpoint package (no index object; log record in the last part; map object every MapEvery checkpoints; open replays from the map) and a comparison harness (just compare-layouts). Report: docs/measurements/layouts-2026-09-28.md. Log layout halves PUTs and commit time and nearly eliminates metadata bytes (48% -> 4% of upload at 1 s intervals, 3% -> 0% at 60 s) but opens cost 1-2 more round trips, hold the whole page table in memory, and free space only at each map. Verified by lockstep correctness tests against the index layout and the full Go suite.
<!-- SECTION:FINAL_SUMMARY:END -->
