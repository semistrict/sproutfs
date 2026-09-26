---
id: TASK-41
title: Watch a fork's parent host while a child on another host receives
status: To Do
assignee: []
created_date: '2026-09-26 16:40'
labels:
  - embedder
dependencies: []
priority: medium
ordinal: 48000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration's receive ends on evidence when its source is cut off while still listed (TASK-15): the source's hold is over. A fork onto another host does not watch its parent's host at all. The orchestrator's fork calls Receive with no watch, so a cut-off parent leaves the child's receive waiting for the HTTP client's 10-minute timeout or the caller's request. The simulated world carries fork receives with no hold to match.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A child's receive on another host ends on the same evidence a migration's does, including the parent's hold
- [ ] #2 A simulation scenario cuts off a listed parent during a remote fork and the child's receive ends at the hold
<!-- AC:END -->
