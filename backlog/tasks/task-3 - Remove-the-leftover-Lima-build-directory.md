---
id: TASK-3
title: Remove the leftover Lima build directory
status: Done
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 17:21'
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
- [x] #1 The owner has allowed it, and the directory is gone
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The directory was already gone by 2026-09-26 (Lima restarted after the Mac upgrade; /tmp does not survive).
<!-- SECTION:NOTES:END -->
