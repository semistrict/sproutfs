---
id: TASK-92.5
title: 'Zircon port step 5: the page queues'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 07:32'
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
- [ ] #1 The page queues are ported from page_queues.cc, with their aging goroutines driven by platform.Clock
- [ ] #2 All 10 cases of vm/unittests/page_queues_unittest.cc run as Go tests in synctest bubbles
- [ ] #3 The host recency list, idle list and cold-copy pins are the reclaim, don't-need and zero-fork queues; vmmemory/pagelist.go is deleted
- [ ] #4 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes, including the idle-page and cold-copy tests
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Port vm/page_queues.cc and vm/include/vm/page_queues.h into vmmemory/internal/zirconvm/page_queues.go: queues, generations, MarkAccessed, ProcessLruQueue, PeekIsolate, the isolate and zero-fork queues, counts. Not ported, each commented: the MRU and LRU threads and the timeout (owner decision 4: aging driven by faults only), the accessed-bit scan, loaned pages, LRU actions, the debug compressor, kernel counters, Dump.
2. Port the 10 cases of page_queues_unittest.cc into page_queues_test.go, in synctest bubbles at both page sizes.
3. Replace in place: resident gets the queue node; Host.lru becomes the reclaim and isolate queues (aged one generation per fault, so the order is fault order as before), Host.idle becomes the don't-need queue, the pins of cold copies move the page they pin into the zero-fork queue, outside reclaim, taken last. Delete vmmemory/pagelist.go and the links.
4. Run the whole suite in both arena modes, check-guards, just check.
5. Benchmarks old vs new (10 alternating runs), Gremlins on page_queues.go, tests for meaningful survivors.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported the parts of page_queues.cc that VmCowPages calls into vmmemory/internal/zirconvm/pagequeues.go ahead of this step, without the 10 tests, threads, aging timers, LRU actions or loans. This step completes that file and ports its tests.
<!-- SECTION:NOTES:END -->
