---
id: TASK-92.2
title: 'Zircon port step 2: model-check a page''s dirty life across a checkpoint'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
  - spec
dependencies:
  - TASK-92.1
references:
  - plans/zircon-pager-port-2026-10-05.md
  - docs/testing.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 100000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 2 of the plan. The port departs from Zircon where a store meets a page a checkpoint holds (D1), where a failed checkpoint gives its pages back (D4), and where dirty pages spill (D2, D5). These are where a checkpoint could hold bytes from after its pause or lose a write, and where the pager had its worst defect, a refault that bound a private page after a checkpoint retired it (docs/vm-memory.md, A refault decides again after its reclaim). A spec checks the departures before any code depends on them. See The TLA+ question in the plan.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 spec/writeback/Writeback.tla models a few pages of one region through store, seal, walk, settle, upload, land or fail, retire, abandon, spill or drop, refault, a reclaim met by a seal, and a fork hold
- [ ] #2 TLC checks SealedBytes, NoLostWrite, Reserved and Budget; every MC configuration passes in seconds and the deep configuration in under two minutes
- [ ] #3 Three mutants run in just check-spec and each fails the invariant it names: Zircon in-place AwaitingClean to Dirty fails SealedBytes, a refault that ignores a retire fails Reserved, a seal that skips a page a reclaim holds fails NoLostWrite
- [ ] #4 docs/testing.md describes the spec under Model checking, and any defect it finds in the code or the plan is in spec/bugs.md
<!-- AC:END -->
