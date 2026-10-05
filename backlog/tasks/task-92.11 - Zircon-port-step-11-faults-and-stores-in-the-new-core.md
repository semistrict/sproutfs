---
id: TASK-92.11
title: 'Zircon port step 11: faults and stores in the new core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 05:10'
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
