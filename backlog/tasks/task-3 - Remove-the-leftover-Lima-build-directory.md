---
id: TASK-3
title: Remove the leftover Lima build directory
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 01:46'
labels:
  - needs-owner
  - chore
dependencies: []
priority: low
type: chore
ordinal: 3000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A killed test run left /tmp/sproutfs-firecracker.XkXxbi in the Lima instance "default". Removing it needs rm -rf, which needs the owner's permission.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner has allowed it, and the directory is gone
<!-- AC:END -->
