---
id: TASK-16
title: Report local fork holds in Status
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: medium
type: feature
ordinal: 16000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The control plane can then release them.

**A child forked onto its parent's own host holds the fork point without the orchestrator seeing the hold.** Such a child is served no pages, so it is not in the parent host's `Status().Serving`. The orchestrator's survey of stale handovers therefore cannot see the hold. Only the host's own four-interval deadline ends a hold whose child never publishes its root (`host/fork.go`, `host/migrate.go`). If hosts reported local holds alongside served ones, the survey could release them the same way.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Status lists local fork holds, and the orchestrator's survey releases stale ones
<!-- AC:END -->
