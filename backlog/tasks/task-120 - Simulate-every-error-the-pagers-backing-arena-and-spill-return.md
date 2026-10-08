---
id: TASK-120
title: 'Simulate every error the pager''s backing, arena and spill return'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 15:45'
updated_date: '2026-10-08 16:01'
labels:
  - vmmemory
  - simulation
dependencies: []
priority: high
ordinal: 151000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
scripts/faults/vmmemory.json listed only the pager's client. The pager also calls across a boundary into its backing (a volume, which reads object storage and a peer host), its arena (memfds, whose allocations can fail for want of memory) and its spill file (a local device). No campaign injected their errors at random, so the paths those errors take were never run beside everything else, which is how the mapping refusal bug hid for weeks (docs/testing.md, Every boundary error is simulated).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 scripts/faults/vmmemory.json lists Backing, SparseLoader, UnpublishedLoader, UnpublishedInstaller, PagedBacking, EphemeralBacking, Arena, ArenaFile, ZeroFile, EqualFile, CountedFile, ClosableFile and the spill file, each method with its errors or none
- [x] #2 The campaigns' fixtures fail each listed error at random, and the campaigns handle each as production does: a fault answered with an error ends the guest's session
- [x] #3 check-faults passes for vmmemory with nothing deferred, and every bug a newly fired fault exposed has a regression test and a guard
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Read the production implementations for the errors each method returns (volume.Volume, the Linux arena, the spill file).
2. Give the fixtures a random Buggify site for each error.
3. Extend the campaigns' machine: any injected error a guest's request is answered with ends its session, and its host closes it; the host also verifies the region on a timer, as Connection.verify does.
4. Run the campaigns and sweeps, and fix each pager bug a new fault exposes, each with a regression test and a sim.Bug guard.
5. Write the manifest entries and run check-faults.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Validation: check-faults fires all 27 vmmemory entries with nothing deferred. Sweeps of seeds 0 to 199 of the prefetch and rules campaigns and 0 to 399 of the fork campaign replay in both arenas at both pages; the vmmemory suite passes in both arenas and with SPROUTFS_FORK_CAMPAIGN_SEEDS=400. The new faults exposed no pager bug: every one ends the session it reaches, as production does. The spill file's own failures (truncate, allocate) come one call in a hundred and a campaign makes one pager a seed, so TestAPagerWhoseSpillFileFailsNeverStartsAndSaysWhy runs seeds until each has happened.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The pager's backing, arena and spill are in scripts/faults/vmmemory.json. The campaigns' fixtures fail each listed error at random, the spill's simulated disk fails as a device does, and a guest whose request meets one has its session ended and is closed by its host, which also verifies each region on a timer. No pager bug surfaced. Verified by check-faults, replay sweeps of all three campaigns in both arenas, and the vmmemory suite in both arenas.
<!-- SECTION:FINAL_SUMMARY:END -->
