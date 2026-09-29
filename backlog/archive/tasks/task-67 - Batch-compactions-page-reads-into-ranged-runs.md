---
id: TASK-67
title: Batch compaction's page reads into ranged runs
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-29 01:54'
labels: []
dependencies: []
ordinal: 75000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Publication.compact (checkpoint/publication.go) rewrites the live pages of mostly-dead checkpoints by calling loadPage once per page, serially: one ranged GET per member. The TASK-66 layout comparison measured about 13,000 serial GETs, roughly 130 s of modelled time, for one checkpoint compacting 64 MiB of 4 KiB pages, all before its commit can land. Reads already group a run's members into ranged extents and fetch them concurrently through the page cache (checkpoint/run.go).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Compaction fetches the pages it moves grouped into ranged extents per part, concurrently under readExtentConcurrency, through the page cache
- [ ] #2 Pages are written into the new parts in the same deterministic order as before, so a retried publication produces identical bytes
- [ ] #3 A test counts object-store GETs for a compaction of many adjacent 4 KiB pages and asserts a handful of requests, and none for pages already cached
- [ ] #4 go test ./checkpoint/... ./volume/... passes
<!-- AC:END -->
