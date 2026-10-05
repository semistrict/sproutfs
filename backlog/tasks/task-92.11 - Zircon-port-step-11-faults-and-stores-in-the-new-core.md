---
id: TASK-92.11
title: 'Zircon port step 11: faults and stores in the new core'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 10:57'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.10
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 109000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 11 of the plan. Read faults, prefetch, population, stores, write-ahead, placement and the two rules of the old core (vmmemory/fault.go, faultfirst.go, window.go, population.go, placement.go, rules.go) run over the region layer and identity roots. The policy is ours and moves as it is: the page first, a fault at random alone, the populate budget, the private page at its own offset, the gap and half-private rules. Mapping commands are collected under the object lock and issued after it, as DeferredOps does in Zircon.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Under SPROUTFS_PAGER_CORE=zircon the fault, prefetch, population, store, write-ahead, placement and rules tests of vmmemory pass, and they are on the list just check runs
- [ ] #2 vm_mapping_page_fault_optimisation_test and vm_mapping_page_fault_range_test run as Go tests of mapping a fault's resident neighbours
- [ ] #3 The guard pager-zero-new-page and the fault-policy guards are killed under both cores
- [ ] #4 No mapping command is issued with an object lock held, shown by a test that holds the client's answer; go test -race of vmmemory passes on the Mac, and the hostile Linux suites pass under the new core on GCE
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Split into green commits, as the step's size asks: reads; stores and write-ahead; population, placement and the rules.
1. zirconvm: a VmPage may name a Frame, Zircon's paddr, in place of bytes in this process; the package's copies and zeroings assert they have bytes here. The alias set is generic so both cores keep one per page.
2. The zircon core's objects: a Pmm over the arena whose pages are frames (a slot of an arena file), an identity root per (checkpoint, volume) with its page source, a region layer per memory region whose resolver names the root of each offset the fault has located, and a binding beside the layer for what Zircon has no place for (mapped, zero runs).
3. Reads: the fault's lookup is the layer's lookup cursor (RequireReadPage), a missing page a READ request on its root's source that the fault answers by reading the page into a frame and supplying it to the root; the fault maps the page with its resident neighbours (IfExistPages, the two aspace tests). The policy moves as it is: page first, a fault at random alone, a stream's run at once, prefetch behind the page as READ requests on the roots, a fault meeting a prefetch waits on its request. Every Buggify site, probe, guard and sim.Admit point on these paths keeps its name. Mapping commands are collected under the object lock and issued after it.
4. Population over the roots' resident pages, the budget as it is.
5. Stores and write-ahead over the region layer: a store copies a root's page into a frame of the region's private file at its placed offset and supplies it Dirty; fresh zeros become Dirty frames in one command; the gap and half-private rules.
6. Attach, detach, statistics and idle pages under the zircon core; eviction stays step 12, so the core refuses what would evict.
7. The tests of fault, prefetch, population, store, write-ahead, placement and the rules that pass under the zircon core go on scripts/pager-core-zircon.json; guards on these paths get cores current and zircon; a test holding the client's answer shows no command is issued under an object lock; go test -race; benchmarks under both cores (alternate binaries, 10 runs, medians); Gremlins on the new files.
<!-- SECTION:PLAN:END -->
