---
id: TASK-122.11
title: 'Read ahead by streams, so a 4 KiB fault at random reads only its page'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-09 13:32'
updated_date: '2026-10-09 13:32'
labels:
  - vmmemory
dependencies: []
parent_task_id: TASK-122
priority: high
ordinal: 167000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
On GCE (2026-10-09, 4 KiB PMEM pages) the embedder's fio random 8 KiB reads loaded 44 pages and evicted as many per fault: a 4 KiB pager sees each 8 KiB read as two adjacent faults, and the second fault in a read-ahead window prefetched the whole 2,048-page window. Zircon maps at most 16 present pages and leaves how much to read to the user pager (Fxfs: aligned 128 KiB). Decide how far a fault reads by Linux-style streams; keep mapping resident pages of the window as before.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A stream's first two faults read their pages alone; its third reads 4 ahead and each later one 4x more, up to its window
- [x] #2 Faults at random do not push out a stream that has gone on
- [x] #3 Guards for each behaviour, killed by tests
- [ ] #4 A 4 KiB GCE run of the embedder's PostgreSQL benchmark beside plain records random reads, fill and pgbench
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. readahead.go: streams decide the pages a fault reads; window rule still decides what it plans and maps. 2. Tests + guards. 3. Full suite, just check, push. 4. GCE 4 KiB benchmark beside plain.
<!-- SECTION:PLAN:END -->
