---
id: TASK-56
title: 'Run the RAM give-back on its own schedule, not the checkpoint interval'
status: Done
assignee:
  - '@claude'
created_date: '2026-09-27 01:51'
updated_date: '2026-09-28 00:48'
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
- [x] #1 The give-back has its own interval setting, independent of SPROUTFS_CHECKPOINT_INTERVAL, documented in deploy/README.md
- [x] #2 A VM whose count of copies with an origin grows past a threshold is given back without waiting for the interval
- [x] #3 An idle VM with no such copies costs no give-back pass
- [x] #4 Tests prove both triggers, and that disabling checkpoints does not stop the give-back
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. vmmemory: a memory region counts the copies with an origin it makes since the last give-back pass (takeFromCheckpoint with an origin), notes a backlog when a bounded pass leaves some unreached, reports GiveBackPending, and calls a NotifyCopies callback once the count reaches a pass's worth.
2. host: Config.GiveBackInterval (SPROUTFS_GIVE_BACK_INTERVAL, default independent of the checkpoint interval; documented in deploy/README.md). The give-back loop starts for every VM whatever the checkpoint interval, waits for its interval or the notification, and runs a pass only on regions with something pending.
3. Tests: vmmemory (pending/backlog/notify), host (interval without checkpoints; notification before the interval; an idle VM makes no pass), docs.
<!-- SECTION:PLAN:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The RAM give-back keeps its own schedule, SPROUTFS_GIVE_BACK_INTERVAL (10s default, negative disables; deploy/README.md), independent of the checkpoint interval. A memory region counts copies with an origin and notifies the host at a pass's worth, which runs a pass at once; regions with nothing pending cost no pass. Passes are clock callbacks armed when the machine is added. Verified: host tests on a simulated clock (interval with checkpoints off, pass's worth before the interval, idle VM costs no pass; each mutation-checked), vmmemory tests, config test; just check passes.
<!-- SECTION:FINAL_SUMMARY:END -->
