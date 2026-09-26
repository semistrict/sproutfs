---
id: TASK-45
title: >-
  Recover a VM after a lost source without counting the guest the destination
  discarded
status: To Do
assignee: []
created_date: '2026-09-26 19:23'
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
- [ ] #1 The recovery after a lost source reopens the VM on the destination whose discarded guest no longer counts
- [ ] #2 An orchestrator test reproduces the stale survey and the recovery succeeds
<!-- AC:END -->
