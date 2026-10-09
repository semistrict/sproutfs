---
id: TASK-122.7
title: >-
  Keep free slots ahead of a 4 KiB pager's faults, so a full arena still writes
  ahead
status: Done
assignee: []
created_date: '2026-10-09 02:16'
updated_date: '2026-10-09 12:50'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 163000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
At 4 KiB PMEM pages the benchmark's 12 GiB sequential write ran at 8 MiB/s (1,468 s; 454 MiB/s on plain GCE, 293-698 at 2 MiB). Until the 6.4 GiB arena filled, write-ahead worked: 553 faults made 549,000 pages dirty, about 1,000 each. After it filled, every fault evicted one page and copied one page (309,000 evictions, 315,000 copy-on-writes, 323,000 faults at 1.6 ms each): write-ahead takes only free slots, and the evictor frees one slot inside the fault that needs it. A pager whose evictor keeps a reserve of free slots ahead of its faults, evicting in batches, would let a fault write a run ahead again; at 2 MiB the same cost is spread over 512 times the bytes, which is why it did not show there. This blocks measuring 4 KiB pages for TASK-110 and TASK-122's random reads.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A 4 KiB guest writing sequentially through a full arena faults about once per write-ahead run, not once per page
- [x] #2 The benchmark's 12 GiB write at 4 KiB is measured before and after
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Looked at the fix (2026-10-09). The evictor port has an asynchronous path (zirconvm.Evictor.EvictAsynchronous) the pager never calls, and Host.makeRoom gives up only idle pages, so a store's write-ahead run shrinks to one page once the arena holds none. But moving the eviction behind the fault does not change its cost: each victim is one revocation round trip and one 4 KiB spill write (reclaimStep reclaims one page a step), which is about the 1.6 ms a fault took. A 4 KiB pager that keeps up with sequential writes needs its evictions batched — one revocation command for a run of victims, their spill writes coalesced into one I/O — and an asynchronous reserve on top of that. The asynchronous request has to be told apart from a fault's (R's zero value is a nil *evictionRequest) and must not cancel prefetches.

Landed 96eeb7aa (with ffec92ae, 740260bd): a step evicts up to 2 MiB of victims or a sixteenth of the arena together, one revocation pass per region and one spill write; a write-ahead run with no room has a step free a batch first. TestASequentialWriteThroughAFullArenaFaultsOncePerRun (48 faults for 3,072 pages, was 3,072) with guards pager-evict-one-victim-a-step and pager-write-ahead-after-one-victim. GCE benchmark at 4 KiB PMEM, 2026-10-09: 12 GiB fill 42 MiB/s (was 8; 2 MiB 335; plain 454), file phase 416 s (was 1,600). Random reads 252/s p50 3.1 ms; 2,848,434 of 2,864,554 loaded pages came from spill versions. But pgbench 434 tps, pgbench -i 330 s, 175 of 339 probes failed: 539,457 dirty waits (publication per page cannot keep up at 4 KiB), mean fault 2.4 ms, 6.9M evictions for about 6M pages in (2.66M prefetched).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Evictions are batched and write-ahead runs are placed in what a batch frees, so a 4 KiB sequential write faults once per run; the benchmark's 4 KiB fill went from 8 to 42 MiB/s. What now limits 4 KiB is the dirty wall (TASK-122.10) and the fault path's cost.
<!-- SECTION:FINAL_SUMMARY:END -->
