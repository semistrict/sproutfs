---
id: TASK-61
title: Make the tamper test pass under the sproutfsprobe build
status: To Do
assignee: []
created_date: '2026-09-27 19:14'
labels:
  - flaky
dependencies: []
priority: low
ordinal: 68000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
go test -tags sproutfsprobe ./vmmemory/ fails TestAPublishedPageItsVMMChangedEndsThatVMMsSession (vmmemory/isolation_test.go): the probe's stable audit in windowPlan.bindShared (window.go) panics on the published page the test tampers with on purpose, before the pager's own digest check can report ErrTampered, and the cleanup then deadlocks the synctest bubble. It fails on 6cc6ddb4 too, so it predates 2026-09-27's changes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The probe build of vmmemory passes, with the tamper test proving the pager reports ErrTampered
<!-- AC:END -->
