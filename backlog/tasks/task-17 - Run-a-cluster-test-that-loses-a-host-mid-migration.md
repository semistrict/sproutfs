---
id: TASK-17
title: Run a cluster test that loses a host mid-migration
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
  - gce
dependencies: []
priority: high
type: task
ordinal: 17000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**The orchestrator's watch of a migration's source is unproven on a cluster.** The simulated deployment and the orchestrator's own tests over fakes cover it. The one soak run that passed killed a host that ran no VMs. So no GCE run has yet lost a host during a real migration (`cmd/sproutfs-orchestrator/lostsource_test.go`, `internal/simtest/lostmigrationsource_test.go`).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A GCE run kills a host during a real migration and every VM ends running or reopenable
<!-- AC:END -->
