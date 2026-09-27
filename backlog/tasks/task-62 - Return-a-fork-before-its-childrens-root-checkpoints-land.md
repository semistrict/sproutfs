---
id: TASK-62
title: Return a fork before its children's root checkpoints land
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-27 21:54'
updated_date: '2026-09-27 22:32'
labels:
  - performance
dependencies:
  - TASK-50
priority: high
ordinal: 69000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork waits for each child's root checkpoint (host.rooted in host/migrate.go's Receive) before it returns: until the root lands the child cannot be opened elsewhere, so a host lost meanwhile loses it. The owner wants a fork not to block on object-store publication. With TASK-50 a local child's root no longer uploads what it inherited but still waits for the parent's one upload of the fork point. The root should instead be taken behind the running child, by its interval loop or a background capture, with the child reported as not yet durable (like the parent's own unpublished writes) until it lands.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A fork returns once its children are running, without waiting for any object-store upload
- [x] #2 A child whose root has not landed is reported as such (status and orchestrator), cannot be forked or migrated, and publishes its root in the background with retries
- [ ] #3 Host, orchestrator and simulation tests cover a host lost before a child's root lands
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented: host.Receive no longer waits for a fork child's root; rootBehind publishes it behind the running child with retries from 250 ms doubling to 30 s. hostapi.VM.RootPending and orch.VM.RootPending report it. Tests: host TestAForkReturnsBeforeItsChildsRootLands (receive returns with checkpoint uploads refused; child cannot be migrated; root lands once the store answers), TestNoOtherHostCanOpenAChildBeforeItsRootLands (ErrForkPending), orchestrator TestVMsReportsAForkWhoseRootHasNotLanded; existing host tests wait on VM.Rooted; the simulation's fork step waits for the root. Not covered: a simulation campaign that kills a host with a root pending, and orchestrator recovery of such a child (it reports ErrForkPending and leaves the record).
<!-- SECTION:NOTES:END -->
