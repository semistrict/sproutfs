---
id: TASK-41
title: Watch a fork's parent host while a child on another host receives
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 16:40'
updated_date: '2026-09-26 17:48'
labels:
  - embedder
dependencies: []
priority: medium
ordinal: 48000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration's receive ends on evidence when its source is cut off while still listed (TASK-15): the source's hold is over. A fork onto another host does not watch its parent's host at all. The orchestrator's fork calls Receive with no watch, so a cut-off parent leaves the child's receive waiting for the HTTP client's 10-minute timeout or the caller's request. The simulated world carries fork receives with no hold to match.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A child's receive on another host ends on the same evidence a migration's does, including the parent's hold
- [x] #2 A simulation scenario cuts off a listed parent during a remote fork and the child's receive ends at the hold
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. api/host ForkResult reports Hold (hold_seconds) as MigrateResult does; the supervisor fills it from HoldTimeout.
2. Orchestrator fork: count the hold from the fork's answer (handover.Held) and receive every child through o.receive, the same watch and rule (handover.Hold.Gone) a migration's receive uses. A child whose receive ends on that evidence fails the fan-out as any refused child does.
3. Orchestrator tests: a remote fork whose listed parent host goes quiet ends at the parent's hold; the fake host's Fork reports its hold and retires the child's hold at its end.
4. Simulation: the world's fork receives each child under the hold its parent's host keeps (no more Hold{}); scenario TestARemoteForkWhoseParentIsCutOffEndsAtItsHold isolates a listed parent during the child's post-copy and the receive ends at the hold, not the harness patience.
5. Mutation guard: migration-ignore-source-hold also runs the fork scenario (guards.json, testing.md).
6. Docs: migration.md (fork watch), testing.md.
7. Verify: go test ./..., just check, just soak 1 200, the guard fails the new scenario.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: host ForkResult reports Hold (hold_seconds) as MigrateResult does; the supervisor fills it from HoldTimeout, the deadline every fork hold is armed with before Fork answers. The orchestrator counts it from the answer (handover.Held) and receives every child through o.receive, the watch a migration's receive uses, so one rule (handover.Hold.Gone over look) ends both. The parent's host lists every child it holds the point for in Serving, local ones included, so the look finds it holding until release. A child whose receive ends on that evidence fails the fan-out like a refused child (rest given up, started ones deleted).
Tests: orchestrator TestAForkWhoseListedParentHostNothingCanReachEndsAtItsHold (remote fork, parent host quiet and listed, ends with errLostSource+ErrHoldOver after the 0.2 s hold; exact host log; child rows forgotten; parent still running). Without the watch the test hangs (checked with a 5 s timeout). Fake hosts now retire a fork hold at its reported hold.
Simulation: FanOutWith counts the hold from Fork's return and forked receives under it (was Hold{}). Scenario TestARemoteForkWhoseParentIsCutOffEndsAtItsHold (lostforkparent_test.go): StalledStream on the destination, IsolatedHost on the parent's host during the child's post-copy; receive ends at the 10 s hold (was the 30 s patience), fork does not happen, parent stays on host-0, the host retires the point at its own deadline, parent checkpoints and verifies.
Guard: migration-ignore-source-hold (site Hold.Gone) now names both scenarios in guards.json; each fails alone with it (fork: ended 29.999 s after cut-off).
Also: lostsource_test.go set a quiet host's down flag from inside a receive without the fake's lock, a data race go test -race hit once; the fake's cutOff() takes the lock.

Validation: go test ./... and just check pass; just soak 1 200 passes; the new orchestrator test and sim scenario pass under -race; migration-ignore-source-hold kills both scenarios.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A fork's child is received under the same watch and rule as a migration's receive (handover.Hold.Gone: unlisted pod, an answer that it holds nothing, or the hold over). The host's ForkResult reports its hold (hold_seconds); the orchestrator counts it from the answer and receives every child through o.receive. The simulated world receives fork children under that hold instead of none. Verified by the orchestrator test TestAForkWhoseListedParentHostNothingCanReachEndsAtItsHold, the scenario TestARemoteForkWhoseParentIsCutOffEndsAtItsHold (ends at the 10 s hold, not the 30 s patience), the migration-ignore-source-hold guard now naming both scenarios, go test ./..., just check and just soak 1 200.
<!-- SECTION:FINAL_SUMMARY:END -->
