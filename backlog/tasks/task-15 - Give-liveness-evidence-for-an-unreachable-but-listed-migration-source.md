---
id: TASK-15
title: Give liveness evidence for an unreachable but listed migration source
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 16:32'
labels:
  - embedder
  - correctness
dependencies: []
priority: medium
type: feature
ordinal: 15000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**A migration whose source host is unreachable but still listed waits for that host.** A destination asks its source for the pages no checkpoint holds until they arrive. The orchestrator ends the migration only on positive evidence that the source is gone: the Kubernetes API no longer lists the pod, or the pod answers and neither runs the VM nor serves its pages (`cmd/sproutfs-orchestrator/orchestrator.go`). If the source host's process is alive, the migration resolves either way. The source's own handover deadline of four checkpoint intervals gives the pages up, and the source's next answer stops the asking. But a host that is unreachable while its pod is still listed does neither. The migration then stays in flight for as long as the request driving it lives. The rule exists to avoid guessing, so the open item is better evidence, not a timeout. The orchestrator has no liveness signal for a pod it cannot reach, other than the Kubernetes API's own.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A migration whose source is unreachable but listed ends on evidence, not a timeout
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Evidence: the source's own hold. A source that reported a hold of H with its handoff gives the pages up H after it armed the deadline, which is before its answer reached the orchestrator. So once H has passed since the handoff arrived, the pages are gone whether the source is alive or not. Kubernetes status beyond listing is not used: NotReady and a deletion timestamp say nothing about the process, and a restart count lags the process it counts, so it cannot be tied to the one that handed the VM over. A hold of zero (a source that promised nothing) proves nothing.
1. internal/handover: Hold (Held, Over) and one rule, Hold.Gone(look), over one look at the source (listed, answered, holding). Policy.Begin takes the Hold. sim.Bug guard that ignores the hold.
2. Orchestrator: handOver counts the hold from the handoff; watchSource and retry both apply Hold.Gone to the survey. errLostSource carries the evidence. The recovery after a lost source excuses the source's silence: it handed the VM over and cannot run it.
3. Tests: orchestrator (quiet listed source ends at its hold and the VM is recovered; quiet source inside its hold waits; a source that promised no hold waits); handover unit tests.
4. Simulation: Config.Hold; an isolated-host fault (process runs, pod listed, links and control plane cut); the world's receive and retry use the same rule; scenario where the source is partitioned while listed ends at the hold, not the harness patience.
5. Docs: migration.md, architecture.md, testing.md, guards.json.
6. Verify: go test ./..., just check, just soak 1 200, mutation guard kills the scenario.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Evidence: the source's own hold (MigrateResult.Hold), counted by the orchestrator from the moment the handoff arrived; the source armed its deadline before answering, so its promise has ended by then. One rule, handover.Hold.Gone(look): unlisted pod, answered without the VM, or hold over. Silence never counts; a zero hold proves nothing. Kubernetes status beyond listing is not used (NotReady and deletion timestamps say nothing about the process; restart counts lag the process they count).
Orchestrator: watchSource (receive in flight) and retry both apply the rule; the retry's last wait ends with the hold, so a handover that no destination took now ends on the rule and the VM is recovered (TASK-14 left it stopped; docs and the simulated world already recovered it). recoverLost excuses only the source's silence (it handed the VM over); any other quiet host still refuses the recovery.
Simulation: Config.Hold, IsolatedHost fault (process runs, listed, links and control plane cut), World.reach for everything the deployment does to a host, world receive and retry use the same rule. Scenario TestAMigrationWhoseSourceIsCutOffEndsAtItsHold; mutation guard migration-ignore-source-hold kills it (ends at the 30 s harness patience instead of the 10 s hold).
Found: a fork onto another host does not watch its parent's host at all in the orchestrator (o.fork calls Receive with no watch), so a cut-off parent leaves the child's receive waiting for the HTTP client's 10-minute timeout. Not in scope; the simulated world carries fork receives under no hold to match.

Validation: go test ./... and just check pass; just soak 1 200 passes (scheduled, seeded topology, host crash, swizzle, buggified). go test -race on the orchestrator, handover and the sim migration scenarios passes; orchestrator suite x10 passes. Guards: migration-ignore-source-hold kills TestAMigrationWhoseSourceIsCutOffEndsAtItsHold; migration-give-up-first-receive still kills the swizzle campaign. Removing the source's excuse from the recovery fails TestAListedSourceNothingCanReachEndsAMigrationAtItsHold.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A migration whose source is listed but unreachable now ends on the source's own hold. internal/handover gains Hold (counted from the handoff's arrival) and one rule, Hold.Gone: unlisted pod, answered without the VM, or hold over; silence never counts, a zero hold proves nothing. The orchestrator's receive watch and retries both apply it (so migrations and drains share it); the retry's last wait ends with the hold, so a handover no destination took is recovered on that evidence. The recovery after a lost source excuses only the source's silence. The simulated world uses the same rule, with Config.Hold and an IsolatedHost fault. Verified by orchestrator tests (quiet listed source ends at its hold and is recovered; quiet source inside its hold waits; another quiet host refuses the recovery), handover unit tests, the sim scenario TestAMigrationWhoseSourceIsCutOffEndsAtItsHold with its mutation guard, go test ./..., just check and just soak 1 200.
<!-- SECTION:FINAL_SUMMARY:END -->
