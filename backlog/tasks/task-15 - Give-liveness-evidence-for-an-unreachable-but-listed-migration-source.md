---
id: TASK-15
title: Give liveness evidence for an unreachable but listed migration source
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 16:08'
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
- [ ] #1 A migration whose source is unreachable but listed ends on evidence, not a timeout
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
