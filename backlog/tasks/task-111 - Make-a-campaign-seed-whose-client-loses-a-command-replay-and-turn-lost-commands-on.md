---
id: TASK-111
title: >-
  Make a campaign seed whose client loses a command replay, and turn lost
  commands on
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 14:08'
updated_date: '2026-10-08 15:51'
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
- [x] #1 Every seed of the prefetch, rules and fork campaigns replays with lost commands on, checked by their replay tests over seeds that lose a command
- [x] #2 A lost guest detaches at once, as a host closes a dead VM, and the run still replays
- [x] #3 simulateLostCommands is gone and the deferred entries in scripts/faults/vmmemory.json are ordinary ones that scripts/check-faults.py fires
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Rebuild the diff harness into the replay tests (requireReplay prints where two runs part).
2. Turn terminal faults on, sweep many seeds of the prefetch, rules and fork campaigns in both arenas, and fix each place a woken or concurrent goroutine decides something outside a turn of the run.
3. Model a lost guest as production runs it: its host learns of the region's end, ends the VMM, waits for its tasks and detaches it at once (machine_test.go).
4. Fix every pager bug the terminal faults expose, each with a regression test and a sim.Bug guard.
5. Delete simulateTerminalFaults, make the manifest's deferred entries live, and run each campaign's replay test over seeds that inject a terminal fault.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Replay leaks found and fixed: (a) an eviction, harvest, rebind or fork-file end commanded several regions in a map's order, so which region met a refusal varied: regions now carry an attach serial and are commanded in that order; (b) waiters an unlock woke raced to take the lock (LazyMutex broadcast, RWMutex readers, a woken writer against its waker's TryLock): every pager lock is now waited for without taking it (ctxsync WaitFree, WaitReadable, WaitWritable) and taken by try when the run admits (lockAdmitted); (c) a detach went on beside the faults and prefetches it waited for: its waits are admitted the same way; (d) the campaigns stopped their flushers at the instant a timer fired, outside any turn: the world now admits that point.

Validation: the replay tests of the prefetch (seeds 5, 7, 10), fork (2, 5, 10) and rules (2, 5) campaigns each inject a terminal fault and replay. Seeds 0 to 199 of each campaign, in both arenas and at both pages, ran twice per check and the check twice, and every one replayed. SPROUTFS_FORK_CAMPAIGN_SEEDS=400 passes in both arenas. check-faults fires all 14 of the client's faults with nothing deferred. Bug found: a fork point's child whose client refused a revocation failed its parent's unseal (guard pager-end-a-step-with-its-sharer).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Lost commands and refused revocations are on in every campaign, and every seed replays. Four places went on outside the run's turns: regions commanded in a map's order, waiters racing for a lock an unlock woke them for, a woken writer racing its waker, and a detach beside the faults it waited for; each now waits without taking anything and goes on when the run admits it. A lost guest is closed at once by its host, which ends its VMM and detaches it. The campaigns found that a child's refused revocation failed its parent's unseal, now fixed with a regression test and a guard. Verified by the replay tests over seeds that inject terminal faults and 200-seed sweeps of all three campaigns in both arenas.
<!-- SECTION:FINAL_SUMMARY:END -->
