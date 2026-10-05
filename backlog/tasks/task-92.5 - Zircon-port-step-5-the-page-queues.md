---
id: TASK-92.5
title: 'Zircon port step 5: the page queues'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 07:06'
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported the parts of page_queues.cc that VmCowPages calls into vmmemory/internal/zirconvm/pagequeues.go ahead of this step, without the 10 tests, threads, aging timers, LRU actions or loans. This step completes that file and ports its tests.
<!-- SECTION:NOTES:END -->
