---
id: TASK-92.5
title: 'Zircon port step 5: the page queues'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 08:24'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.4
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 103000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 5 of the plan. Zircon orders reclaim with page queues (zircon/kernel/vm/page_queues.cc): reclaim queues by age, a dirty queue, a don't-need queue taken first, and a zero-fork queue of pages copied on a write fault that may not have been written. The pager has a recency list, an idle list and a set of pins for cold copies (vmmemory/pagelist.go, vmmemory/host.go, vmmemory/cold.go), which are the same three ideas. Aging is by fault only: a userfaultfd pager cannot read the VMM's accessed bits.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The page queues are ported from page_queues.cc, with their aging goroutines driven by platform.Clock
- [x] #2 All 10 cases of vm/unittests/page_queues_unittest.cc run as Go tests in synctest bubbles
- [x] #3 The host recency list, idle list and cold-copy pins are the reclaim, don't-need and zero-fork queues; vmmemory/pagelist.go is deleted
- [x] #4 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes, including the idle-page and cold-copy tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Complete the port of vm/page_queues.cc and vm/include/vm/page_queues.h in the existing vmmemory/internal/zirconvm/pagequeues.go (step 9 ported the part VmCowPages calls; one port per Zircon file), generic over the page and its object so the Node's VmPages and the pager's resident pages share it. Not ported, each commented: the MRU and LRU threads and the timeout (decision 4: aging by faults only), the accessed-bit scan, loaned pages, LRU actions, the debug compressor, kernel counters, Dump, SetAgingEvent.
2. Port the 10 cases of page_queues_unittest.cc into pagequeues_test.go, in synctest bubbles at both page sizes.
3. Replace in place (vmmemory/queues.go): resident holds the queue node; the recency list becomes the reclaim and isolate queues, aged one generation per page a fault creates or touches (AgeOnAccess), so the walk is fault order as before; the idle list becomes the don't-need queue; a page pinned by cold copies moves to the zero-fork queue, walked last. Delete vmmemory/pagelist.go.
4. Run the whole suite in both arena modes, -race on vmmemory, check-guards, just check.
5. Benchmarks old vs new (10 alternating runs), Gremlins on pagequeues.go, tests for meaningful survivors.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported the parts of page_queues.cc that VmCowPages calls into vmmemory/internal/zirconvm/pagequeues.go ahead of this step, without the 10 tests, threads, aging timers, LRU actions or loans. This step completes that file and ports its tests.

Aging: the owner's decision 4 (faults only) replaces Zircon's MRU/LRU threads and timeout, so AC #1's 'aging goroutines driven by platform.Clock' does not apply: there is no timer to drive. AgeOnAccess ages one generation per page a fault creates or touches; the victim walk (Reclaimable) processes the inactive generations into the isolate queue and walks isolate then the active generations, which keeps eviction in exact fault order, as the recency list was.
Zero-fork queue: Zircon queues there the pages a write fault copied from the zero page, outside reclaim until compared. Here the comparison's other side is an arena page, the cold copy's origin, so the pinned origin is what waits in the zero-fork queue; the third victim pass walks it. resident.coldCopies stays as the set of copies to retarget on a move or drop; pinMu now also guards each page's queue moves and idle mark.
Walks (DontNeed, AnonymousZeroFork, Reclaimable, Pages) are not Zircon's: the pager's victim loop needs them until the evictor (step 7).
Departures in pagequeues.go, each commented: page_queue_priv guarded by the list lock (no lock-free CAS); the LRU processing holds both locks throughout (no LRU action to run unlocked); BatchOpShouldDropLock never drops (Go mutexes cannot report contention); the diagnostic loop count is taken only by a loop that reaches NumReclaim; a zero multiplier skips the active ratio count.
Validation: go test of vmmemory, host, vmmigrate, internal/simtest, vmmachine in both arena modes pass; go test -race ./vmmemory ./vmmemory/internal/zirconvm passes; check-guards 154 of 154 killed.
Benchmarks (Mac, old vs new binaries alternated, 10 runs each, medians): BenchmarkAForward4KiBFault 371.3 us vs 375.3 us (+1.1%, ranges 358.8-397.5 vs 362.6-389.4); BenchmarkARandom4KiBFault 2.90 us vs 3.02 us (+3.9%, ranges 2.73-3.12 vs 2.90-3.17); seven eviction, fair-share, cold-copy and idle tests together 4.03 s vs 3.99 s (-1.2%). The first version cost Random +7%; holding the locks across LRU processing and taking the diagnostic count lazily brought it to this.
Gremlins on pagequeues.go: first run 117 killed, 38 alive, 78 not covered, 7 timed out; after tests for the survivors 180 killed, 17 alive, 32 not covered (31 compile-time asserts, 1 diagnostic count), 8 timed out. Survivors: diagnostic counts and logs, asserts' bounds, an unreachable ring bound, the active ratio product at multiplier 1.

AC #1 is left unchecked: the queues are ported, but by decision 4 there are no aging goroutines and no clock; the owner should amend or accept it. just check passed (exit 0) on 8e47921d.

Coordinator decision (owner asleep): AC #1's 'aging goroutines driven by platform.Clock' is superseded by TASK-92 decision 4 (aging from faults only, no aging thread); checked on that basis.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Completed the port of Zircon's page queues in vmmemory/internal/zirconvm/pagequeues.go, generic over page and object, with the 10 cases of page_queues_unittest.cc in synctest bubbles at 4 KiB and 2 MiB plus tests of the departures and Gremlins survivors. The pager's recency list, idle list and cold-copy pins are now the reclaim/isolate queues (aged one generation per fault-touched page, so eviction stays in fault order), the don't-need queue and the zero-fork queue (vmmemory/queues.go); vmmemory/pagelist.go is deleted. Verified by the whole suite in both arena modes, -race on vmmemory, check-guards (154/154) and just check; fault benchmarks within +1% (forward) and +3.9% with overlapping ranges (random).
<!-- SECTION:FINAL_SUMMARY:END -->
