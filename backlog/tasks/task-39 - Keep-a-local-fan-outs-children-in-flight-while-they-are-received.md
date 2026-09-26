---
id: TASK-39
title: Keep a local fan-out's children in flight while they are received
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 15:14'
updated_date: '2026-09-26 17:28'
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
- [ ] #1 Each child of a fan-out stays in flight until its receive has finished or failed, however long the fan-out takes
- [ ] #2 A test drives a fan-out whose last child is received after the aging bound and it succeeds
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
