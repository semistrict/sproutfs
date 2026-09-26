---
id: TASK-44
title: Delete a fork child that lands after its fan-out gave it up
status: To Do
assignee: []
created_date: '2026-09-26 18:06'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 51000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork whose child receive fails gives up the remaining holds (TASK-41). A child receive still in flight on another host is then doomed, unless it has already fetched every page it inherited. If it had, the child lands with nobody having asked for it. The survey shows it running, so it is visible, but nothing deletes it. Found while doing TASK-40.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A child that lands after its fan-out failed is deleted, on evidence the orchestrator can prove
- [ ] #2 An orchestrator test and a simulation scenario land such a child and it ends deleted, with its parent untouched
<!-- AC:END -->
