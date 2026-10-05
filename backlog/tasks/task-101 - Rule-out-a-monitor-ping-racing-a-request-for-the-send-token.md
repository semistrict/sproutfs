---
id: TASK-101
title: Rule out a monitor ping racing a request for the send token
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - bug
  - peer
dependencies: []
priority: low
ordinal: 121000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A ping from a connection's monitor ticker could race a request for the send token at the same instant. Never observed; noted on TASK-81.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A scheduled test makes both arrive at once and shows the outcome is correct, or a fix lands with that test
<!-- AC:END -->
