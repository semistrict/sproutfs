---
id: TASK-92.2
title: 'Zircon port step 2: model-check a page''s dirty life across a checkpoint'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 05:50'
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
- [x] #1 spec/writeback/Writeback.tla models a few pages of one region through store, seal, walk, settle, upload, land or fail, retire, abandon, spill or drop, refault, a reclaim met by a seal, and a fork hold
- [x] #2 TLC checks SealedBytes, NoLostWrite, Reserved and Budget; every MC configuration passes in seconds and the deep configuration in under two minutes
- [x] #3 Three mutants run in just check-spec and each fails the invariant it names: Zircon in-place AwaitingClean to Dirty fails SealedBytes, a refault that ignores a retire fails Reserved, a seal that skips a page a reclaim holds fails NoLostWrite
- [x] #4 docs/testing.md describes the spec under Model checking, and any defect it finds in the code or the plan is in spec/bugs.md
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Write spec/writeback/Writeback.tla from vmmemory/checkpoint.go, fault.go and bindings.go: per page the guest's binding (clean, Dirty with its own reservation, or sharing the checkpoint's AwaitingClean copy) and the checkpoint's copy, each resident or spilled; reservations as slots of the dirty budget.
2. Actions: store in place, store by copy across its reclaim (D1 split, D5 slot first), spill refault across its reclaim, eviction in two halves (D2 spill of dirty pages and copies, drop of clean pages), pause (D3 protect and take the dirty set), the walk with a reclaim met halfway, settle, reads by the upload and a fork's children, land or fail, retire, abandon (D4), a fork hold that skips the settle.
3. Invariants SealedBytes, NoLostWrite, Reserved, Budget. MC configs of two pages, deep config of three pages with a fork hold, each under two minutes.
4. Mutants via Bugs: zircon-in-place (SealedBytes), refault-ignores-retire (Reserved), seal-skips-reclaim (NoLostWrite).
5. Wire into just check-spec (automatic per directory), describe in docs/testing.md, record any defect in spec/bugs.md.
6. Run just check-spec, check-spec-deep for this spec, and just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
spec/writeback/Writeback.tla written from vmmemory/checkpoint.go, fault.go, bindings.go and resident.go (Host.read). Bytes are a count of the guest's stores to a page, so a stale copy shows. Reservations are named slots, so a reclaim the walk meets halfway writes to the slot the copy took over. Eviction is two steps (page lock), store faults and spill refaults are two steps around the reclaim that gives the region up; the walk holds the region; retire and abandon go a page per batch.
Results on the Mac (15 cores): MCWriteback 75,324 states in 1 s; MCFork 137,631 states in 2 s; deep/Three (three pages, four slots, four stores, two seals, forks) 3,228,944 states in 78 s. Mutants: zircon-in-place fails SealedBytes (10-state trace: store, pause, walk, in-place store, upload read); refault-ignores-retire fails Reserved (14 states: store, spill, refault reads the slot, pause, walk, upload, land, retire, refault binds a private page to the clean page, the 2026-09-22 interleaving); seal-skips-reclaim fails NoLostWrite (6 states: store, reclaim begins, pause, walk skips it).
No defect found in the code. Checked by hand that the model reaches the walk joining a reclaim, a D1 split, a settle, a D4 restore, a full budget and a fork child's read after landing. The spill refault's epoch check has an ABA (seal then abandon during its reclaim returns the same dirty flag and nil checkpoint); the model shows it is harmless, since abandon gives back the same slot and nothing can change the page's bytes while the fault holds its window.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/writeback/Writeback.tla: a page's dirty life across a checkpoint with D1 to D5 (store, seal, walk, settle, upload, land or fail, retire, abandon, spill or drop, refault, a reclaim met by the walk, fork holds). TLC checks SealedBytes, NoLostWrite, Reserved and Budget: MCWriteback 1 s, MCFork 2 s, deep/Three 78 s. Three mutants run in just check-spec and fail the invariant each names (SealedBytes, Reserved, NoLostWrite). docs/testing.md describes it. No defect found, so spec/bugs.md is unchanged. Verified with just check-spec, the deep run, and just check (exit 0).
<!-- SECTION:FINAL_SUMMARY:END -->
