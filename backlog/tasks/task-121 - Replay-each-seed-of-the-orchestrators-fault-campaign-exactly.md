---
id: TASK-121
title: Replay each seed of the orchestrator's fault campaign exactly
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 19:25'
updated_date: '2026-10-08 19:35'
labels:
  - orchestrator
  - simulation
dependencies: []
priority: medium
ordinal: 155000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TestTheOrchestratorUnderBoundaryFaults (cmd/sproutfs-orchestrator/campaign_test.go) runs without a scheduler, so a failing seed cannot be replayed. sim.Buggify numbers a site's draws per task only in a controlled run (platform/sim/inject.go drawingTask); outside one the host surveys, the reconcile timer, a migration's source watch, a flight's rewrites and a resumed handover draw in Go-scheduler order and go on beside each other at one instant.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The campaign runs under a sim.Scheduler, with every concurrent caller in the orchestrator named by sim.WithTask and every boundary call and timer wake admitted
- [x] #2 A replay test runs the 128 campaign seeds twice each and requires identical recordings, printing where they first differ; a 500-seed sweep replays too
- [x] #3 check-faults on scripts/faults/orchestrator.json passes and the campaign comment no longer says a seed is not replayed
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Run the campaign under sim.NewScheduler with a Wait that passes through once the campaign ends.
2. Name every concurrent caller with sim.WithTask: campaign steps, each orchestrator process's reconcile ticks, survey requests by host, a receive's source watch, a flight's rewrites, a resumed handover, membership steps.
3. Admit (sim.Admit) every fake boundary call and every timer wake: campaign waits, hold ends, reconcile, flight, source watch, retry and recover-lost pauses.
4. Add TestTheOrchestratorCampaignReplaysItsSeeds with requireReplay; check 128 and 500 seeds; show it fails without the scheduler.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Without a scheduler (Wait nil) 19 of the first 32 seeds diverge on a second run; with it 128 seeds replay twice. fanOut already writes each reply into its host's slot in name order, so no reply is acted on in arrival order.

Validation: SPROUTFS_ORCHESTRATOR_SEEDS=128 and =500 replay tests pass (42s, 163s); the 128-seed campaign passes; check-faults on orchestrator.json fires 57 of 57.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The orchestrator's fault campaign now runs under a sim.Scheduler: the fakes admit every request, a host's hold ends and the campaign's waits admit after waking, and the orchestrator names each concurrent caller (steps, reconcile ticks, survey requests by host, source watches, flights, resumed handovers, membership steps) and admits after every timer it sleeps on. TestTheOrchestratorCampaignReplaysItsSeeds runs 16 seeds twice by default; 128 and 500 replay. Without the scheduler 19 of 32 seeds diverged.
<!-- SECTION:FINAL_SUMMARY:END -->
