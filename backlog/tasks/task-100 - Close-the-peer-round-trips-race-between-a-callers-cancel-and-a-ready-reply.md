---
id: TASK-100
title: Close the peer round trip's race between a caller's cancel and a ready reply
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - bug
  - peer
dependencies: []
priority: low
ordinal: 120000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
peer/conn.go roundTrip's final select can take a reply that was ready on entry after the caller cancelled, because Go picks either ready case. Callers in vmmigrate now check the backing's end themselves, so nothing is known to break, but the contract is loose.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A test forces both cases ready and shows the chosen outcome
- [ ] #2 roundTrip's contract is stated and kept
<!-- AC:END -->
