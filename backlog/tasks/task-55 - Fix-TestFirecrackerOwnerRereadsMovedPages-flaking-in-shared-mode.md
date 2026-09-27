---
id: TASK-55
title: Fix TestFirecrackerOwnerRereadsMovedPages flaking in shared mode
status: To Do
assignee: []
created_date: '2026-09-27 00:09'
labels:
  - bug
dependencies: []
priority: medium
ordinal: 62000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TASK-49's new Lima test fails 2 of 3 runs on main in the shared arena, with 1-3 protect traps on the owner's reread (found by TASK-53, 2026-09-26). It passes in isolated mode.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The cause is found and the test passes 20 runs in a row in both arena modes
<!-- AC:END -->
