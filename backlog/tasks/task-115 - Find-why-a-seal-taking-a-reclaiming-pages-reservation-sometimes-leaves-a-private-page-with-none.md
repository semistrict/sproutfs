---
id: TASK-115
title: >-
  Find why a seal taking a reclaiming page's reservation sometimes leaves a
  private page with none
status: To Do
assignee: []
created_date: '2026-10-08 15:22'
labels:
  - pager
  - flaky
dependencies: []
priority: medium
type: bug
ordinal: 154000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
vmmemory TestSealTakingAReclaimingPagesReservationKeepsItsBytes fails now and then under load with "private page has no spill reservation" when reading or storing into a guest page (seal_test.go:322 and 331). It fails on main at e24104c1 too: 3 of 320 runs with eight runs in parallel, none of 200 run one at a time. It failed one just check of the platform manifest work, whose change does not reach the test (its fixture runs without Buggify). The flake makes just check fail at random.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The cause is found and fixed, and the test passes 1000 runs with eight in parallel
- [ ] #2 A regression test or a sim.Bug guard covers the race
<!-- AC:END -->
