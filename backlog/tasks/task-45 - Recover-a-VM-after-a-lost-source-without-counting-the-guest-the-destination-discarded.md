---
id: TASK-45
title: >-
  Recover a VM after a lost source without counting the guest the destination
  discarded
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 19:23'
updated_date: '2026-09-27 21:42'
labels:
  - embedder
dependencies: []
priority: high
ordinal: 52000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
In the GCE run of 2026-09-26 (TASK-17, docs/measurements/gce-2026-09-26.md) a migration's source was killed mid-stream. The receive was ended correctly, but the orchestrator's own recovery then failed twice with 'no host has 3221225472 bytes of memory free': the destination's survey still counted the guest it had just discarded. The VM was left stopped until a manual sproutfsctl recover reopened it 3 s later.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The recovery after a lost source reopens the VM on the destination whose discarded guest no longer counts
- [x] #2 An orchestrator test reproduces the stale survey and the recovery succeeds
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Cause: the orchestrator ends its receive call when it sees the source gone and recovers at once, while the destination is still tearing the receive down (it reports the VM in Receiving and its committed guest RAM still counts), so the recovery's placement found no room. Fix: reopen refuses a VM a receive of which is in flight (errReceiving, an errRunning), and recoverLost waits on the watch interval for that receive to end, up to lostRecoveryPatience (2 min). Reproduced and proven by TestARecoveryAfterALostSourceWaitsForTheDestinationToGiveTheGuestUp (fake destination lingers 3 surveys with the VM in Receiving and its RAM committed; fails without the fix).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A recovery after a lost migration source now waits for the destination to give the discarded guest up before reopening the VM, and reopen refuses any VM a host still reports receiving. Verified by an orchestrator test that reproduces the stale survey and fails without the change; go test ./... and just check pass.
<!-- SECTION:FINAL_SUMMARY:END -->
