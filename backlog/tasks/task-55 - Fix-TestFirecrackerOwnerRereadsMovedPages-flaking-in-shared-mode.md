---
id: TASK-55
title: Fix TestFirecrackerOwnerRereadsMovedPages flaking in shared mode
status: Done
assignee: []
created_date: '2026-09-27 00:09'
updated_date: '2026-09-28 00:17'
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
- [x] #1 The cause is found and the test passes 20 runs in a row in both arena modes
<!-- AC:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Cause: the guest kernel stores into a moved page during the reread, which traps on write protection and copies, correctly. The test now allows faults and copies that are the guest's stores and nothing else. 20 of 20 per arena on GCE x86_64 (SPROUTFS_FIRECRACKER_COUNT added).
<!-- SECTION:FINAL_SUMMARY:END -->
