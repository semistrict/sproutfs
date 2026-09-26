---
id: TASK-43
title: Judge a handover against a survey taken after its row was read
status: To Do
assignee: []
created_date: '2026-09-26 17:48'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 50000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The orchestrator's release() judges a table row against a survey of the hosts taken before it read that row. A child whose receive lands in between is given up after it was already taken in. On a real host that is harmless, because giving up a taken-in child's hold works like releasing it, but the rule reasons from evidence older than what it judges. Found while doing TASK-39.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 release() never judges a row written after the survey it uses began
- [ ] #2 An orchestrator test lands a receive between the survey and the row read, and the child's hold is released rather than given up
<!-- AC:END -->
