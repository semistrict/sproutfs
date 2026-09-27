---
id: TASK-47
title: Find why the fork-hold pressure test fails about once in 500 runs
status: Done
assignee: []
created_date: '2026-09-26 19:23'
updated_date: '2026-09-27 23:49'
labels:
  - bug
dependencies: []
priority: medium
ordinal: 54000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TestForkHoldDoesNotAnswerAnotherMemoryRegionsPressure (vmmemory/dirty_budget_test.go) fails rarely under synctest: 1 of 900 runs on main and 2 of 300 on another branch, on 2026-09-26. The store fails with 'managed-memory dirty budget stalled' after the requested checkpoint landed. A synctest failure means the order is not fixed by the code, so this is a real race in the pager or the fixture, not load.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The cause is found and fixed, and 5000 runs pass
<!-- AC:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Cause: takeSpill's take failed, then a checkpoint gave its reservations back and ended before relief looked, so relief and takeBack found nothing and the store failed as stalled with reservations free. Fix: before stopping anybody, the store retries if the budget changed since the attempt began (vmmemory/pressure.go). Verified: 20000 runs of TestForkHoldDoesNotAnswerAnotherMemoryRegionsPressure pass (5 of 10000 failed before, reproduced with a state dump showing 2 free reservations); just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
