---
id: TASK-62
title: Return a fork before its children's root checkpoints land
status: To Do
assignee: []
created_date: '2026-09-27 21:54'
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
- [ ] #1 A fork returns once its children are running, without waiting for any object-store upload
- [ ] #2 A child whose root has not landed is reported as such (status and orchestrator), cannot be forked or migrated, and publishes its root in the background with retries
- [ ] #3 Host, orchestrator and simulation tests cover a host lost before a child's root lands
<!-- AC:END -->
