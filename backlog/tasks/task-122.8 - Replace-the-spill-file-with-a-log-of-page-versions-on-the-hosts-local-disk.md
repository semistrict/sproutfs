---
id: TASK-122.8
title: 'Keep a published page''s version in the spill file, and load it from there'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-09 09:25'
updated_date: '2026-10-09 10:55'
labels:
  - performance
dependencies: []
parent_task_id: TASK-122
priority: high
type: task
ordinal: 164000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 1 of plans/local-writeback-2026-10-09.md, after the decision there that the log is the spill file's fixed slots rather than an append log. A checkpoint's page whose bytes are in its reservation when the checkpoint retires keeps that slot under its identity instead of freeing it, and a load reads a version the spill holds before the store. Published versions are dropped oldest first whenever a reservation wants a slot. A slot's bytes count as a version only while no store can have changed the page since they were written, so a refault that hands the guest its page writable drops them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A test publishes a spilled page, evicts it and refaults it from the spill file, with no backing read
- [x] #2 A reservation takes the slot of the oldest published version when no slot is free, and the dirty budget is unchanged
- [x] #3 A refault that hands the guest its page writable drops the slot's bytes, and a test proves a stale version is never loaded
- [x] #4 Every vmmemory suite and campaign passes; docs/vm-memory.md describes the published versions
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Landed fd850e81. Decision recorded in the plan: the log is the spill file's fixed slots, not an append log (every version is one page, so nothing is compacted). Verified: TestAPublishedSpilledPageLoadsFromTheSpillFile (published spilled page refaults from the spill file, no backing read, at 4 KiB and 2 MiB); zirconvm TestAReservationTakesTheOldestUnreadVersionsAllocation, TestVersionsPastTheirBoundDropTheOldest, TestAVersionDroppedWhileItIsReadIsAMiss, TestAVersionTheDeviceChangedIsDropped; TestARefaultedPageKeepsNoStaleVersion, which kills guard pager-keep-a-refaulted-page-s-spill; just check green before push. The simulation knob spill-versions draws none, one or the pager's bound; TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes runs with none so its read still reaches the peer backing.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A checkpoint's page whose bytes are in its reservation at retire keeps them in the spill file under its identity, and loads read such versions before the backing; versions take only slots reservations leave and drop oldest-first with a second chance; a writable refault drops the stale bytes. Verified by unit and synctest tests, a guard, and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
