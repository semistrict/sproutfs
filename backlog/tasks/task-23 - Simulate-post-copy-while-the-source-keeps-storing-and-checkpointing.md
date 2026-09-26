---
id: TASK-23
title: Simulate post-copy while the source keeps storing and checkpointing
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 17:15'
labels:
  - testing
  - correctness
dependencies: []
priority: medium
type: task
ordinal: 23000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**No simulation reaches the post-copy paths where that defect was.** Two separate things were wrong there, and neither simulated campaign caught either of them. First, the pager's test double stripped the identity permanently, as the product did, so the tests modelled the retire's mistake instead of catching it. Second, with an earlier fix to `readIn` disabled, a hundred soak seeds still passed, because in every campaign the source's unpublished set only shrinks. The missing piece is a scenario in which the source keeps storing and checkpointing while a destination post-copies from it, and the destination then publishes and retires what it received. Until that scenario exists, this class of defect can only be reached on a real kernel.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A simulated scenario has the source storing and checkpointing while a destination post-copies, publishes and retires
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Decide from the code which source keeps storing: a fork's parent stores but serves a frozen fork point and refuses a checkpoint while held; a migration's source stops before its handoff set is taken. What keeps storing, checkpointing and retiring while post-copying is the destination, whose own volume moves past the source's copy.
2. Give simtest.Handover a Meanwhile phase between the receive and the source's release (migrations and forks); draw it on half the schedule's handovers; add zero stores to World.Store so retires hand holes back.
3. Directed scenario TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes (fork and migration) with an arena smaller than one guest.
4. Fix the product defects it finds in vmmigrate.PeerBacking with one rule: a handoff page is the source's until this host takes it; loads ask the source only for such pages and for pre-handoff pages outside the set.
5. Check rather than copy: testbacking records any load whose answer disagrees with Locate; vmmemory's peer double answers from what the pager took; a retire refused with ErrUndroppable fails the run.
6. Guards for the old rules in scripts/mutation/guards.json; prove readIn's case; probe; docs; just check; just soak 1 300.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Decided from the code: no source's served set changes after its handoff. A fork's parent keeps storing, but its page server serves the frozen fork point and a checkpoint of it is refused with volume.ErrSealed while the point is held (the directed scenario asserts this). A migration's source stops and hands its volumes off before Unpublished() is taken. The window that was never simulated is the destination's: it stores, is checkpointed and retires while its peer backing still asks a serving source (production: until the orchestrator's release and the end of the bulk stream; a fork's root is published inside the receive).

Product defects found by the new scenario, both in vmmigrate.PeerBacking, both fixed:
1. LoadUnpublished asked the source for every page. A page the destination had published, once evicted, was read again from the source: the older version, marked as the source's own for a handoff page. Reproduced: fork child reads 2 (fork point) for a page it published as 3.
2. Locate stripped a handoff page while the volume named a checkpoint predating the handoff; a hole names none, so a page of zeros the destination published stayed stripped, its retire dropped it, and the next read asked the source (migration: disk page reads 2, want 0).
Fix: one rule. A handoff page is the source's until this host takes it (unfetched); Locate strips exactly those; loads ask the source only for those and for pages outside the set named by a pre-handoff checkpoint or a hole.

readIn (f9f690fb): with the backing fixed its branch is unreachable (disabling it, the scenario passes); with the old backing (guard migration-ask-for-published-pages) the scenario fails with and without it, because the load path hands the guest the same stale bytes. So it is not a guard; vmmemory's unit test keeps it tested against a disagreeing double.

Validation on 3d67adb9: just check passes (go test ./... included); just soak 1 300 passes (all five soaks, 0 failures). Guard invocations from guards.json each fail the directed scenario; baseline passes. Soak twins seeds 1-60 (120 runs) under each guard: strip-published-pages fails 88, strip-published-holes 49, ask-for-published-pages 3 (buggified seeds whose pager evicts); no guard 0.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added the missing scenario: a destination that stores, is checkpointed and retires what it received while its peer backing still asks a serving source (simtest.Handover.Meanwhile, used by half of the generated migrations and forks, plus TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes for a fork and a migration with an arena smaller than one guest). From the code, no source's served set changes after its handoff: a fork's parent stores but serves the frozen point and refuses a checkpoint while held (asserted); a migration's source stops first. The scenario found two product defects in vmmigrate.PeerBacking (loads asked the source for pages the destination had published since; Locate kept published holes stripped), fixed with one rule: a handoff page is the source's until this host takes it. The simulation now checks the backing's two answers against each other (testbacking), the vmmemory peer double answers from what the pager took instead of copying the rule, and ErrUndroppable fails a run. Guards migration-strip-published-pages / -holes / migration-ask-for-published-pages each fail the scenario; readIn's check is unreachable once the backing is fixed and not observable under the old backing, documented in docs/testing.md. Verified with just check and just soak 1 300.
<!-- SECTION:FINAL_SUMMARY:END -->
