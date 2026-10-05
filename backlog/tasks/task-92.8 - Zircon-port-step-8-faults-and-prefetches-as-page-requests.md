---
id: TASK-92.8
title: 'Zircon port step 8: faults and prefetches as page requests'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 07:06'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.7
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 106000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 8 of the plan. Zircon asks its pager for missing pages with page requests that batch overlapping ranges, wake every waiter a supply covers, and can be cancelled (zircon/kernel/vm/page_source.cc). A fault's read and a prefetch become READ requests to a provider over the volume, the cluster and a peer, and a fault that meets a page a prefetch is reading waits on that request. PagerProxy's port packets become a goroutine per request. Which faults prefetch and in what order the pages are read stays ours.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Page requests are ported from page_source.cc; every backing read of a fault or a prefetch goes through one
- [ ] #2 The prefetch Buggify sites, the nine prefetch probes and the guards pager-read-in-flight-again, pager-read-alone-at-random, pager-prefetch-every-fault, pager-plan-the-window-at-random, pager-read-the-run-first, pager-plan-the-window-first and pager-fault-waits-for-its-prefetch keep their names and are killed or reached as before
- [ ] #3 TestPrefetchCampaignReplaysItsSeeds and TestPrefetchSurvivesItsFaultsAndReachesItsProbes pass, with the sim.Admit points and the priced planning work under their names
- [ ] #4 BenchmarkARandom4KiBFault and BenchmarkAForward4KiBFault are recorded before and after on one machine, and neither median is slower by more than the spread of two runs before
- [ ] #5 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported PageSource, PageRequest and MultiPageRequest into vmmemory/internal/zirconvm/pagesource.go ahead of this step, with a PageProvider interface and no early wake. This step adds PagerProxy as a goroutine and wires faults and prefetches.
<!-- SECTION:NOTES:END -->
