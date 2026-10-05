---
id: TASK-103
title: Remove finished agent worktrees and stale raw measurement data
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - cleanup
dependencies: []
priority: low
ordinal: 123000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Every finished agent left a worktree under .claude/worktrees; their branches are merged. Gitignored raw measurement data under docs/measurements/*/ is about 6 GB. The Mac's disk ran 95–98% full and broke a soak run. git status in the main checkout once ran over 30 minutes. Deleting needs the owner's OK.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner approves what may go
- [ ] #2 Approved worktrees are removed with git worktree remove and their branches deleted
- [ ] #3 git status in the main checkout finishes in seconds
<!-- AC:END -->
