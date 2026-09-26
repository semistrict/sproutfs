---
id: TASK-39
title: Keep a local fan-out's children in flight while they are received
status: To Do
assignee: []
created_date: '2026-09-26 15:14'
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
