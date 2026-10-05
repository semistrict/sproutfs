---
id: TASK-92.7
title: 'Zircon port step 7: the evictor'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.6
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 105000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 7 of the plan. The victim loop of Host.allocate (vmmemory/allocation.go) becomes the synchronous path of Zircon's evictor (zircon/kernel/vm/evictor.cc) over the page queues, with ReclaimPage's split: an identity root's clean page is dropped, a region's own page is spilled. The fair share stays ours as a filter on the candidates: Zircon protects VMOs by priority and hints, not by a share of the arena. Idle pages go first, then prefetches still reading are cancelled, then a mapped page is evicted, as today.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The evictor is ported from evictor.cc with its targets; an allocation short of a slot uses its synchronous path
- [ ] #2 Six cases of vm/unittests/evictor_unittest.cc run as Go tests in synctest bubbles; evictor_discardable_test is left out
- [ ] #3 The Buggify site vmmemory/evict-past-a-free-slot, the guard pager-prefetch-ignores-pressure and the probe vmmemory/eviction-during-publication sit in the evictor under the same names; every campaign that requires them to fire or be reached still passes
- [ ] #4 The fair-share and eviction-ordering tests pass unchanged, and every test in vmmemory, host, vmmigrate and internal/simtest passes in both arena modes
<!-- AC:END -->
