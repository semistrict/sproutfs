---
id: TASK-14
title: Retry a failed migration receive while the source holds the pages
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 15:32'
labels:
  - embedder
  - correctness
dependencies: []
priority: high
type: feature
ordinal: 14000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**A migration tries the receive once. If the receive fails, the guest loses its writes since its last checkpoint.** The source stops the guest and gives up its volumes before the destination is asked to take the VM. The source keeps the pages no checkpoint holds until it is told that the destination has all of them. When the destination cannot take the VM, the orchestrator records the VM as stopped and reports the failure. Nothing retries the same destination, or another one, while the source still holds those pages (`cmd/sproutfs-orchestrator/orchestrator.go`). The VM reopens from its last checkpoint, and the source's own four-interval hold deadline retires the pages. A drain performs one such migration per VM. So a host that leaves while a destination is briefly unreachable takes the guest's unpublished writes with it. The simulated swizzle campaign shows that a retry is sound, because the source's handoff stays valid for as long as the source holds it. The campaign also shows that the first attempt does fail under a separated link (`internal/simtest/swizzle_test.go`). The nightly seed sweep found this on 2026-09-18. Its seeds 21 and 227 had been passing only because the world reported a VM as on a host that a failed takeover had only attempted.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A failed receive is retried on the same or another destination while the source holds the pages
- [x] #2 The swizzle campaign passes with retries
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Refactor: export the host's hold (Host.HoldTimeout) and report it in MigrateResult.Hold, so the control plane knows how long a handoff stays good.
2. New package internal/handover: the retry policy shared by the orchestrator and the simulated world. Backoff from 1 s doubling to 15 s, stop at the source's hold deadline, two attempts per destination before moving to the host with room that failed least. Probe on every retry; sim.Bug guard that gives up at the first failure.
3. host.Receive admits one receive of a VM at a time, so a retry on the same destination cannot race a receive whose caller hung up.
4. Orchestrator Migrate: after the handoff, carry it through under the policy, detached from the caller's cancellation (a stopped guest is owed a destination). Before each retry, survey: a host running the VM ends it (landed); a source that answered without serving it, or its hold deadline, ends it; a lost source recovers as before; a quiet failed destination blocks the retry until it answers. Fix Drained so a drain report never overwrites a row the migration wrote.
5. Simulated world: MigrateWith follows the same policy by default (drop Handover.Attempts/Pause); invariants: a failed receive leaves no guest on its destination, and only the destination that took the VM runs it. Swizzle campaign uses the deployment's retries.
6. Tests: orchestrator unit tests for each stop rule and destination choice; host test for a concurrent receive; handover package tests.
7. Docs (migration, hosting, testing), mutation guard in scripts/mutation/guards.json, probe registration.
8. Verify: go test ./..., just check, swizzle/crash/topology soaks over many seeds.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Retry policy: internal/handover (shared by the orchestrator and the simulated world). First retry after 1 s, doubling to 15 s; two attempts per destination, then the host with room that failed least; stops when the source's hold (MigrateResult.Hold, 4 intervals) is over. Before each retry the orchestrator surveys: a host running the VM ends it (landed), a lost or no-longer-serving source ends it (recover), a quiet failed destination blocks the retry. After the handoff the handover runs detached from the caller's context, so a drain's 60 s request deadline no longer cuts it short; Drained no longer overwrites a row that names a destination. host.Receive admits one receive of a VM at a time.
Found on the way: the seeded topology soak failed ~20% of seeds on baseline 97e654ed because World.MigrateWith checked the handed-off handle by writing to the guest's first volume, which is an ephemeral disk in some topologies (fixed in the world). TestTheCampaignsReachTheirProbes (soak-only) fails on baseline too: control/reply-reconciled is now reached and is still listed as unreached (not touched).

Validation: go test ./... and just check pass. Swizzle campaign 1000 seeds pass with the deployment's retries (was a campaign-only 64-attempt loop); the migration-give-up-first-receive guard fails all 16 default seeds and the three new retry scenarios. Soaks seeds 1-600: scheduled, host-crash, swizzle pass; seeded and buggified topology pass except seeds 285 (buggified) and 358 (both), which fail identically on baseline 97e654ed at a kept-checkpoint release reported unavailable, with no receive retried. Unproven: the supervisor's ErrReceiving path (Linux-only, no unit harness); zombie receive on a different host is fenced by the epoch but not prevented (hosts do not report receives in flight in Status, left alone for TASK-16).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A failed migration receive is retried while the source holds the pages. internal/handover holds the policy (1 s doubling to 15 s, two attempts per destination then the host with room that failed least, stop at the source's hold, reported as MigrateResult.Hold). The orchestrator carries the handover detached from the caller after the handoff, so drains benefit, and retries only on survey evidence (landed VM ends it, lost or releasing source ends it, quiet failed destination blocks it). Hosts admit one receive of a VM at a time. The simulated world uses the same policy; the swizzle campaign passes with it (1000 seeds), new scenarios and orchestrator tests cover each rule, and a mutation guard proves the retry is load-bearing.
<!-- SECTION:FINAL_SUMMARY:END -->
