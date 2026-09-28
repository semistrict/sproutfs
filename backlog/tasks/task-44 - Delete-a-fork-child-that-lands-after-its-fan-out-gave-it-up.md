---
id: TASK-44
title: Delete a fork child that lands after its fan-out gave it up
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 18:06'
updated_date: '2026-09-28 03:17'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 51000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork whose child receive fails gives up the remaining holds (TASK-41). A child receive still in flight on another host is then doomed, unless it has already fetched every page it inherited. If it had, the child lands with nobody having asked for it. The survey shows it running, so it is visible, but nothing deletes it. Found while doing TASK-40.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A child that lands after its fan-out failed is deleted, on evidence the orchestrator can prove
- [x] #2 An orchestrator test and a simulation scenario land such a child and it ends deleted, with its parent untouched
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Option B (the owner's choice after review found the table approach unsound: the table is not authority).
1. Page-server protocol: ClaimRequest/ClaimResponse. A fork child's destination, once Done (every inherited page here), claims its hold from the parent's host; the page source marks it claimed if still served, else answers UNKNOWN_VM. Claim and Discard share the page source's lock, so exactly one wins.
2. Local child: Host.took claims the hold under the machines lock, refused if the hold is gone.
3. Host.Receive discards a child whose claim is refused (ErrGivenUp), bounded by the hold timeout.
4. Host.GiveUp reports whether the child had claimed; POST /vms/{id}/abandoned answers {claimed}. The orchestrator deletes a claimed child with the children that started.
5. The orchestrator's fork rollback runs on context.WithoutCancel, so a caller that hangs up still gives up holds and deletes children.
6. Simulation: fork receives can outlive their caller; new LostReceiveAnswer fault; campaigns draw both; Settle and Close require a given-up child's receive to end without it.
7. Tests: host claim tests (given up before claim, claimed before give-up, local); orchestrator tests (lost answer after claim, outlived receive, caller hang-up); sim scenarios for outlived and lost-answer; mutation of each.
8. Known limits, documented: an orchestrator that dies mid-fork, and a give-up the parent's host cannot be reached for.
9. Verify: go test ./..., just check, just soak 1 200.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented option B. Validation: host claim tests (host/claim_test.go), orchestrator tests TestAForkChildWhoseAnswerWasLostIsDeleted, TestAChildWhoseReceiveOutlivesItsFanOutNeverRuns, TestAForkWhoseCallerHangsUpIsStillRolledBack, sim scenarios TestAForkChildWhoseReceiveOutlivesItsFanOutNeverRuns, TestALocalForkChildWhoseReceiveOutlivesItsFanOutNeverRuns, TestAForkChildWhoseAnswerWasLostIsDeleted; each fails with its fix removed. The campaigns now draw outlived-receive and lost-receive-answer; the first soak found 9 seeds of world bookkeeping (lost migration answers, a given-up child's record left by a store outage, a seal held by a given-up receive still in flight), all fixed. just soak 1 200 and just check pass. Known limits, documented in migration.md: an orchestrator that dies mid-fork gives up no hold, and a give-up the parent's host cannot be reached for is not retried.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A fork child runs only after claiming its hold from its parent's host, over the page-server connection (or locally), and that host decides a claim and a give-up one at a time. A child whose fan-out gave its hold up first is discarded by its destination; one that claimed first is reported claimed by the give-up (POST /vms/{id}/abandoned answers {claimed}) and the orchestrator deletes it. The orchestrator's rollback runs uncancelled. Verified by host, orchestrator and simulation tests (each fails without its fix), the campaigns drawing the two new faults, just soak 1 200 and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
