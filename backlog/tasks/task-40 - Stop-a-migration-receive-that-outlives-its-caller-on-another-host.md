---
id: TASK-40
title: Stop a migration receive that outlives its caller on another host
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 15:52'
updated_date: '2026-09-26 18:06'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 47000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration now retries a failed receive (TASK-14). A receive whose caller gave up can still be running on the destination it was sent to while the retry goes to another host. The control record's epoch fences whichever of the two loses, so no data mixes, but the loser has started a guest for nothing and holds its pages until it is fenced. Preventing it needs each host to report the receives it has in flight, so that the orchestrator's evidence rules can wait for them or end them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A host's Status lists the receives it has in flight
- [x] #2 The orchestrator never retries a receive on another host while an earlier destination reports that receive in flight
- [x] #3 A simulation scenario where a receive outlives its caller starts no second guest
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Host: Status reports Receiving, the VMs a receive is in flight for, from the admission TASK-14 added; the host API, the supervisor and orch.Host carry it.
2. internal/handover: Attempts.Next takes the hosts that report a receive of the handoff in flight and chooses no destination while any does (sim guard migration-retry-beside-a-receive).
3. Orchestrator retry passes the hosts reporting the receive, so no receive goes anywhere while an earlier one is in flight; a receive that lands is found by the existing running check.
4. Simulated world: fault OutlivedReceive(host, start): the caller of a receive there hangs up once the host has admitted it, and the host takes start to start the guest. The world keeps that receive; its retry passes the hosts reporting it, and a receive that outlived its caller and took the VM ends the handover there.
5. Tests: host Status lists a receive in flight; orchestrator fake where the failed destination still reports the receive and then lands; handover unit test; sim scenario asserting one guest started; guards.json entry.
6. Docs: migration.md, hosting.md, testing.md, context.md if needed.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: host.Status().Receiving lists the VMs a receive is in flight for (the admission set TASK-14 added), carried by hostapi.Status.Receiving, the supervisor and orch.Host. internal/handover Attempts.Next(ctx, candidates, receiving) chooses no destination while any host reports the receive in flight (guard migration-retry-beside-a-receive), so the orchestrator's retry and the simulated world share the rule; the same host is held back too, since it would refuse a second receive. A receive that lands is found by the existing running check. Once the source's pages are gone the retry does not wait: a receive still in flight then waits for pages nobody has, and the recovery's open fences it (documented). Simulation: simtest.OutlivedReceive(host, start) makes the caller of the next migration receive hang up as the host begins to start the guest and makes the start slow; the world keeps the receive, its retry waits while reachable hosts report it, a receive that took the VM in ends the handover there, and every handover end waits for such receives and requires none took the VM in afterwards. World.ReceivedGuests counts guests started by receives. Not in any campaign (scenario only), like isolated-host.

Validation: go test ./... and just check pass; just soak 1 200 passes (1000 seeded runs). host TestOneReceiveOfAVMAtATime asserts Status().Receiving while a receive is held in its start and empty after. Orchestrator TestNoReceiveIsSentWhileAnEarlierOneIsInFlight and TestTheRetriesGoOnOnceAReceiveInFlightGivesTheVMUp fail with the receivers argument removed (VM goes to host-2). internal/simtest TestAReceiveThatOutlivesItsCallerStartsNoSecondGuest passes and fails under SPROUTFS_SIM_BUG=migration-retry-beside-a-receive (a second host takes the VM; the outlived receive takes it in after the handover). Unproven: the Linux supervisor's Receiving wiring runs only on Linux (compiled and vetted, not exercised); the lost-source case still recovers beside a receive in flight and relies on the epoch.

Rebased onto main after TASK-39/41. forsaken now also refuses to give up a hold while any host reports the child's receive in flight: that receive is creating the child even if the fork that sent it died (TestAReconcileLeavesAForkHoldWhoseChildIsBeingReceived; fails with the check removed). A fork's own child receives do not consult Receiving: a fork sends one receive per child and never retries, so there is no second receive to hold back. Revalidated: go test ./..., just check, just soak 1 200, go test -race ./cmd/sproutfs-orchestrator ./internal/handover.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Hosts report the receives they have in flight in Status (receiving), and the handover policy's Attempts.Next chooses no destination while any host reports one, so the orchestrator never sends a receive anywhere while an earlier one is in flight; a receive that outlived its caller either lands (found as running) or ends and the retries go on. The simulated world shares the rule and gains simtest.OutlivedReceive; its scenario starts one guest and a mutation guard proves the rule is load-bearing. Verified by host, orchestrator, handover and simulation tests, go test ./..., just check and just soak 1 200.
<!-- SECTION:FINAL_SUMMARY:END -->
