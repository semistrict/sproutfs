---
id: TASK-41
title: Watch a fork's parent host while a child on another host receives
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 16:40'
updated_date: '2026-09-26 17:28'
labels:
  - embedder
dependencies: []
priority: medium
ordinal: 48000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration's receive ends on evidence when its source is cut off while still listed (TASK-15): the source's hold is over. A fork onto another host does not watch its parent's host at all. The orchestrator's fork calls Receive with no watch, so a cut-off parent leaves the child's receive waiting for the HTTP client's 10-minute timeout or the caller's request. The simulated world carries fork receives with no hold to match.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A child's receive on another host ends on the same evidence a migration's does, including the parent's hold
- [ ] #2 A simulation scenario cuts off a listed parent during a remote fork and the child's receive ends at the hold
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. api/host ForkResult reports Hold (hold_seconds) as MigrateResult does; the supervisor fills it from HoldTimeout.
2. Orchestrator fork: count the hold from the fork's answer (handover.Held) and receive every child through o.receive, the same watch and rule (handover.Hold.Gone) a migration's receive uses. A child whose receive ends on that evidence fails the fan-out as any refused child does.
3. Orchestrator tests: a remote fork whose listed parent host goes quiet ends at the parent's hold; the fake host's Fork reports its hold and retires the child's hold at its end.
4. Simulation: the world's fork receives each child under the hold its parent's host keeps (no more Hold{}); scenario TestARemoteForkWhoseParentIsCutOffEndsAtItsHold isolates a listed parent during the child's post-copy and the receive ends at the hold, not the harness patience.
5. Mutation guard: migration-ignore-source-hold also runs the fork scenario (guards.json, testing.md).
6. Docs: migration.md (fork watch), testing.md.
7. Verify: go test ./..., just check, just soak 1 200, the guard fails the new scenario.
<!-- SECTION:PLAN:END -->
