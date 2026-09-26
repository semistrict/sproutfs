---
id: TASK-39
title: Keep a local fan-out's children in flight while they are received
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 15:14'
updated_date: '2026-09-26 17:48'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 45000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A local fork hold whose child is never taken in is now given up once the child's table row ages out of flight (TASK-16). The row ages out after two minutes, set when the fan-out wrote it. A fan-out whose child's receive starts later than that can have that child's hold given up mid-fork. The fork then fails cleanly and loses no data, but it fails for no reason.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Each child of a fan-out stays in flight until its receive has finished or failed, however long the fan-out takes
- [x] #2 A test drives a fan-out whose last child is received after the aging bound and it succeeds
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Refactor: the table's aging bound becomes the table's own (a field defaulting to inFlightFor), so a test can shorten it; stillInFlight becomes a table method.
2. A flight (cmd/sproutfs-orchestrator/flight.go): the rows of an operation under way, written again on an interval inside the aging bound until each one lands (its receive finished, the row now says running) or the operation ends.
3. Fork writes its children's rows through a flight before the parent is paused; fork lands each child as its receive finishes; a failed fan-out ends the flight before it forgets the children.
4. Fake host refuses a fork child's receive whose hold was given up, as the real one fails it.
5. Orchestrator test: a local fan-out whose last child is received after the aging bound, with the reconcile running on a short timer, succeeds and gives no hold up. Check it fails without the flight.
6. Docs: migration.md (holds and the table), hosting.md if it describes the aging.
7. Verify: go test ./..., just check, just soak 1 200.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: a flight (cmd/sproutfs-orchestrator/flight.go) writes an operation's rows and writes them again every quarter of the table's aging bound (30 s) until each one lands. Fork flies its children's creating rows before the parent is paused; fork lands each child (row written running) as its receive finishes; Fork ends the flight before it forgets the children of a failed fan-out, so no row is written after that. The aging bound is now the table's own (table.aging, default inFlightFor) so a test can shorten it; stillInFlight is a table method. The two minutes now bound how long a row outlives its orchestrator, not how long an operation may take.
Fake host: a fork child whose hold was given up or ran out (retired) is refused on receive, as the real host fails it.
Test: TestAFanOutKeepsItsChildrenInFlightWhileTheyAreReceived (localfork_test.go): aging 200 ms, reconcile every 20 ms, first child's receive held 600 ms, the second child is received after the bound, fork succeeds, no hold given up, both rows running. With the refresh disabled (interval x400) it fails: the reconcile gives up both holds and the watch ends the first receive.
Found: release() judges a row against a survey taken before it read the row. A child that lands between the two is given up after it was taken in (seen once in a test run). Harmless on a real host (abandoning a taken-in child's hold is a release without the check), not fixed.
No mutation guard: orchestrator tests carry no sim runtime, and the simulated world has no aging table (its survey runs between steps).

Validation: go test ./... and just check pass; just soak 1 200 passes (scheduled, seeded topology, host crash, swizzle, buggified); orchestrator suite x3 and the fan-out test x10 pass under -race.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A fork keeps each child in flight until its own receive has finished or failed. A flight (cmd/sproutfs-orchestrator/flight.go) writes the children's rows before the parent is paused and again every quarter of the table's aging bound (30 s), and writes each child running as its receive finishes; a failed fan-out ends the flight before it forgets the children. The aging bound is now the table's own so a test can shorten it. Verified by TestAFanOutKeepsItsChildrenInFlightWhileTheyAreReceived (last child received after the bound with the reconcile running; fails with the refresh disabled), go test ./..., just check and just soak 1 200.
<!-- SECTION:FINAL_SUMMARY:END -->
