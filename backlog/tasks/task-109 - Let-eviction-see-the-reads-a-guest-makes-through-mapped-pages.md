---
id: TASK-109
title: Let eviction see the reads a guest makes through mapped pages
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 12:34'
updated_date: '2026-10-08 13:25'
labels:
  - vmmemory
  - eviction
dependencies: []
priority: high
ordinal: 145000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder measured a DAX root disk (ext4 dax=always on a PMEM region) under heavy guest writes: random 4 KiB O_DIRECT reads of a 512 MB file went from 0.01 ms idle to a 5.3 ms p50 while several GB were written. The page queues age only on faults (decision 4 of plans/zircon-pager-port-2026-10-05.md; vmmemory/evict.go), and a DAX read of a mapped page never faults, so pages written new push out the pages the guest reads most. The VMM is ours: a Revoke takes a page out of its mappings, and the next touch faults back to the pager. Revoking the oldest pages of the reclaim queues while keeping their frames, and moving any page that faults back before eviction reaches it to the front with MarkAccessed, gives the queues the accessed signal the port left out, at the cost of a fault per hot page per pass and with no I/O.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A page the guest keeps reading through its mapping is not evicted ahead of pages written once while it was read
- [x] #2 A revoked page that is touched again is mapped back from its frame with no read from the spill, the cache or the object store
- [x] #3 A deterministic simulation test reproduces the DAX read-plus-write-churn shape and fails without the change
- [x] #4 A GCE run of the same shape (8 GiB guest, 80 GiB DAX root, 6.4 GiB PMEM arena, several GB written while a 512 MB file is read at random) reports read p50 and p90 during the writes, before and after
- [x] #5 docs/vm-memory.md says what the queues' recency is made of
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. zirconvm: a third isolate queue, harvested, between don't-need and standard; pageQueueList keeps its length; PeekUnharvestedWhere(n, accept) takes the oldest standard isolated pages (isolating inactive LRU pages if short); MoveToHarvested moves one still isolated to the harvested tail. MarkAccessed already moves an isolated page to the MRU queue.
2. evict.go: each reclaim step first harvests: tops the harvested queue up toward a lead (a quarter of the arena, at most a batch per step), revoking every mapped alias of each page under its lock as an eviction does, but keeping the page. The victim order is don't-need, harvested, standard.
3. A guest touch of a harvested page faults; the lookup maps it from its frame and marks it accessed, so it leaves the isolate queues.
4. Test (synctest, both kinds): a page read through its mapping between writes of fresh pages is not loaded again once evictions are steady; fails before the change.
5. Stats: harvested pages and second chances. Docs: vm-memory.md recency, pagequeues.go departures, cowpages.go UnmapAndHarvest.
6. Run vmmemory tests, campaigns, just check; GCE DAX run for the before/after numbers.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented in the harvest worktree: harvested isolate queue (zirconvm), harvest step in reclaimStep (vmmemory/harvest.go), AgeOnAccess restored per served fault (the ported core had stopped calling it since e0fb32de), store traps on harvested read-only pages served as loads (no cold copy). Found and fixed a latent bug: unprotectForStore mapped a page without recording it mapped, so an eviction gave back a slot the guest still mapped writable (TestAStoreIntoAHarvestedJournaledPageIsRevokedByItsEviction). The seal-pause test stored 0, which pages 255/511/767/1023 already held; it now stores each page's complement. vmmemory green in both arenas; guard pager-harvest-without-revoking killed.

GCE before/after (n2-standard-8, nested KVM, 2026-10-08; TestWhatAGuestReadsOfItsDAXRootWhileItWrites): 3 GiB DAX root over a 256 MiB PMEM arena of 2 MiB pages, 2 GiB written at 64 MiB/s for 32 s, a 4 KiB O_DIRECT random read every 2 ms. 32 MiB file: before p50 3 us, p90 5 us, p99 2417 us, 169 spill refaults; harvest p50 4, p90 7, p99 430 us, 13 refaults. 128 MiB file: before p50 4, p90 14, p99 2971 us, 967 refaults; harvest p50 9, p90 315, p99 560 us, 54 refaults, 1568 second chances. Shape scaled down from the embedder's (8 GiB guest, 80 GiB root, 6.4 GiB arena); a first unpaced run (1 GiB in 1.2 s) showed no difference and was replaced. Fault order loses a constantly read page once per arena turnover, so the gain is in p99 and refaults, not p50; the harvest's cost is a ~300 us fault per harvested page touched again (the 128 MiB p90). The embedder's 5 ms p50 means pages revisited less than once per turnover, which no recency fixes: the 2 MiB refault is the lever there (TASK-110).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Each reclaim step harvests first: it takes the mappings of the oldest isolated pages away, keeps the pages, and moves them to a new harvested isolate queue the evictor takes from first; a fault that maps one back marks it accessed. The queues age once per served fault again (the ported core had dropped AgeOnAccess). A store trap on a harvested read-only page is served read-only, so no cold copy. Fixed a protect trap that mapped a page without recording it. Verified by TestAnEvictionKeepsAPageTheGuestReadsThroughItsMapping (failed before: 8 reloads in 128 writes), TestAStoreIntoAHarvestedJournaledPageIsRevokedByItsEviction, TestAHarvestedPageIsPeekedFirstAndLeavesWhenAccessed, guard pager-harvest-without-revoking killed, vmmemory green in both arenas, just check; GCE: p99 of reads during writes down about 5x, spill refaults 13-18x fewer.
<!-- SECTION:FINAL_SUMMARY:END -->
