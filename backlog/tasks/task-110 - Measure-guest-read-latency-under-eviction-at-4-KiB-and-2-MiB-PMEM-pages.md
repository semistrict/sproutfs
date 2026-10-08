---
id: TASK-110
title: Measure guest read latency under eviction at 4 KiB and 2 MiB PMEM pages
status: To Do
assignee: []
created_date: '2026-10-08 12:34'
labels:
  - vmmemory
  - measurement
dependencies:
  - TASK-109
priority: medium
ordinal: 146000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A refault of an evicted PMEM page reads the whole page: at the 2 MiB default (host.DefaultPMEMPageSize) a 4 KiB guest read costs a 2 MiB read from the spill file or the cache, about 5-7 ms on a GCE persistent disk. The 4 KiB PMEM page was measured only for upload bytes (2026-10-07), never for read latency while pages are evicted, so the default has not been weighed on that.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A GCE run of the DAX read-plus-write-churn shape reports read p50 and p90 during the writes at 4 KiB and at 2 MiB PMEM pages, with and without TASK-109
- [ ] #2 The comment on DefaultPMEMPageSize in host/supervisor.go states the result and the default it chose
<!-- AC:END -->
