---
id: TASK-92.9
title: 'Zircon port step 9: the region''s layer and the identity roots'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.2
  - TASK-92.8
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 107000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 9 of the plan. The heart of the port, built and tested alone before anything uses it: VmCowPages and VmObjectPaged (zircon/kernel/vm/vm_cow_pages.cc, vm/vm_object_paged.cc). A region gets one VmCowPages for the pages it owns, dirty-tracked against our pager; each published (checkpoint, volume) a region reads is an identity root of Clean pages that a lookup falls through to by identity instead of a parent chain. It brings the lookup cursor, supply and take, the dirty states and writeback with D1 to D4, zero intervals, the reclaim split with D2, and the snapshot-on-write copy. No hidden parents, no merge, no full or modified snapshot, no slices, no references.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The region layer and identity roots are ported from vm_cow_pages.cc and vm_object_paged.cc, with each departure D1 to D4 marked in the code beside the line it changes
- [ ] #2 The 33 VMO cases the plan lists run as Go tests in synctest bubbles; a case whose expectation a departure changes says which in its comment
- [ ] #3 A test shows a store into an AwaitingClean page leaves the checkpoint the bytes of the pause and gives the store a Dirty copy, and a test shows an abandoned writeback makes every page Dirty again with its own reservation
- [ ] #4 Nothing outside the package uses it yet, and just check passes
<!-- AC:END -->
