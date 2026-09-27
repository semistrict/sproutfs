---
id: TASK-56
title: 'Run the RAM give-back on its own schedule, not the checkpoint interval'
status: To Do
assignee: []
created_date: '2026-09-27 01:51'
labels:
  - performance
dependencies: []
priority: medium
ordinal: 63000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TASK-53's give-back (vmmemory/giveback.go, host/giveback.go) runs once per checkpoint interval because it was hooked onto the host's per-VM checkpoint timer. It has nothing to do with checkpoints: it reads no checkpoint and needs none. A VM that has just read many shared pages should be cleaned up soon, and an idle VM should cost nothing.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The give-back has its own interval setting, independent of SPROUTFS_CHECKPOINT_INTERVAL, documented in deploy/README.md
- [ ] #2 A VM whose count of copies with an origin grows past a threshold is given back without waiting for the interval
- [ ] #3 An idle VM with no such copies costs no give-back pass
- [ ] #4 Tests prove both triggers, and that disabling checkpoints does not stop the give-back
<!-- AC:END -->
