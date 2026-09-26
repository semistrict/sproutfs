---
id: TASK-10
title: Rebase the embedder's Firecracker patches onto semistrict/firecracker@sproutfs
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 01:50'
labels:
  - embedder
  - blocked
  - deferred
dependencies: []
priority: medium
type: task
ordinal: 10000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The patches that are still needed move onto the sproutfs branch of the fork. The memory-diff patches can be dropped. Blocked: the patches are not available yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The needed patches are on the sproutfs branch and the Lima suites pass
<!-- AC:END -->
