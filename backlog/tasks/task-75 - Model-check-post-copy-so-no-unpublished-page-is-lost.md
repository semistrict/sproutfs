---
id: TASK-75
title: Model-check post-copy so no unpublished page is lost
status: Done
assignee:
  - '@claude'
created_date: '2026-10-01 05:37'
updated_date: '2026-10-01 05:53'
labels:
  - formal
  - migration
dependencies: []
priority: high
type: feature
ordinal: 82000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration publishes nothing: the pages no checkpoint holds live only in the source host until the destination fetches them and its next checkpoint publishes them. The source refuses a release while any remain unfetched, gives pages up when a hold outlives its deadline, and the orchestrator retries a failed receive while the source holds the pages (docs/migration.md). spec/ownership leaves the pager and post-copy out. This is the remaining place where a VM can lose writes silently.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A TLA+ spec models the source pages, the handoff, destination fetches with lost replies, Done, ReleaseMigrated, the hold deadline, a lost source and a retried receive
- [x] #2 TLC checks that every page no checkpoint holds is on a live host until a checkpoint publishes it, except when the host holding it is lost
- [x] #3 Small configurations run in just check-spec within a few minutes, with mutants that each invariant catches
- [x] #4 Real defects found are recorded in spec/bugs.md and fixed or filed
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TLC found B1 (fixed) and B2 (open, TASK-80); see spec/bugs.md. B1 is reproduced by TestAnAgedRowDoesNotReleaseASourceUnderAReceive and TestAReceiveKeepsItsRowInFlight, each failing before the fix. TestAReconcileLeavesAForkHoldWhoseChildIsBeingReceived no longer expects refused release requests while the child is received, because the survey now asks for none. AC2 holds except B2, which MCPostCopy tolerates explicitly; mutants/b2-open shows it is still there.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added spec/postcopy/PostCopy.tla: the source's book, release and hold; receive attempts with lost replies and discards; the orchestrator's retries, row ageing and survey release; host loss and orchestrator crashes. NoSilentLoss holds over 14,751 states. TLC found two real defects. B1, fixed: a survey released a source under a receive still in flight, after the row aged during a long receive. The survey now leaves a handover alone while any host reports its receive, and a migration refreshes its row on each source look. B2, open as TASK-80: an orchestrator crash loses the handoff. Verified with new orchestrator tests, three mutants, and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
