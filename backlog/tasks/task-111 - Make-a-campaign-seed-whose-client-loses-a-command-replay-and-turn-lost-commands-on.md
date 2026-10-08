---
id: TASK-111
title: >-
  Make a campaign seed whose client loses a command replay, and turn lost
  commands on
status: To Do
assignee: []
created_date: '2026-10-08 14:08'
labels:
  - vmmemory
  - simulation
dependencies: []
priority: high
ordinal: 150000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The vmmemory test client can lose a command's answer at random (commandLost in vmmemory/memory_test.go), which ends the region as an unanswered command does in production. It is switched off (simulateLostCommands) because a seed that loses one does not replay: on 2026-10-08 prefetch-campaign seed 10 diverged after a region failed, at the disk's eviction (evict-remove) followed by either the disk's reserve-runs or the migrated guest's reclaim-step, in Go-scheduler order. Without the lost command the seed replays every run. A host also closes a dead VM promptly, so the faithful harness detaches a lost guest at once, and a Detach beside running guests does not replay either; deferring the detach to the end starves the others of slots (fork campaign seed 10 deadlocked). With lost commands on, the campaigns found a real deadlock already (a failed capture's unprotect under its own protection), so the paths behind them are worth simulating. A diff harness that prints the first divergence of two runs of a seed is in the session scratchpad (zz_replay_test.go.txt) and is easy to rebuild from Scheduler.Recording(runtime.Trace()).WriteText.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every seed of the prefetch, rules and fork campaigns replays with lost commands on, checked by their replay tests over seeds that lose a command
- [ ] #2 A lost guest detaches at once, as a host closes a dead VM, and the run still replays
- [ ] #3 simulateLostCommands is gone and the deferred entries in scripts/faults/vmmemory.json are ordinary ones that scripts/check-faults.py fires
<!-- AC:END -->
