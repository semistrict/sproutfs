---
id: TASK-29
title: Read the pages compaction rescues as extents
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:18'
updated_date: '2026-09-29 01:58'
labels:
  - performance
  - deferred
dependencies: []
priority: low
type: enhancement
ordinal: 29000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Compaction reads the pages it rescues one at a time.** A read of a range of
a volume now fetches a run of members as one ranged read per extent. But
compaction walks a segment's pages and calls `Store.loadPage` for each page it
moves. So rewriting a mostly dead checkpoint of 4 KiB pages costs one request
per page, up to `compactionBudget`. That budget is 64 MiB, which is 16,384
requests (`checkpoint/publication.go`, `compact`). The pages it moves
are consecutive within a segment, and their members are adjacent in the part
they came from, so the same grouping would apply. It was left as it is
because compaction runs after the guest has resumed and off the fault path.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Compaction fetches the pages it moves grouped into ranged extents per part, concurrently under readExtentConcurrency, through the page cache
- [x] #2 Pages are written into the new parts in the same deterministic order as before, so a retried publication produces identical bytes
- [x] #3 A test counts object-store GETs for a compaction of many adjacent 4 KiB pages and asserts a handful of requests, and none for pages already cached
- [x] #4 go test ./checkpoint/... ./volume/... passes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Factor readRun into loadPages, which returns each page's decoded member through the cache (getAll + fetchMembers: grouped extents, readExtentConcurrency) without zero-extending, and readRun on top of it.
2. compact collects each segment's pages to move in ascending order, loads them in batches of at most maximumRunBytes of pages, and adds them to the writer in that order.
3. Test: a checkpoint of 1,024 adjacent 4 KiB pages, a successor overwriting most of them; count GETs during the successor's commit without a cache, and with every page already cached.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Measured by TASK-66's layout comparison: about 13,000 serial GETs, roughly 130 s of modelled time, for one checkpoint compacting 64 MiB of 4 KiB pages, before its commit lands.

readRun now sits on loadPages, which returns each page's decoded member through the cache and fetches the rest in grouped extents (fetchMembers); compact collects each segment's pages to move in ascending order and moves them in batches of maximumRunBytes of pages (Publication.move). loadPage had no callers left and is gone; its note on identity keying moved to loadPages. Validation: TestCompactionReadsTheRescuedPagesAsExtents (424 adjacent pages: 1 GET uncached, 0 cached; 424 GETs with one page per batch, so the test catches a regression); go test ./checkpoint/... ./volume/... green; retry byte-identity tests unchanged and green; just compare-layouts at 32,768 dirty pages: 12,932 -> 82 compaction GETs and 129 s -> 383 ms mean commit, part bytes identical. Duplicate TASK-67 archived.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Compaction fetches the pages it rescues the way a read fetches a run: through the page cache, and in ranged extents read concurrently for the rest, written back in page order so retries stay byte-identical. Verified by a GET-counting test (1 GET for 424 adjacent pages, 0 when cached) and the checkpoint and volume suites; the layout comparison's large checkpoints went from 12,932 GETs and 129 s to 82 GETs and 383 ms.
<!-- SECTION:FINAL_SUMMARY:END -->
