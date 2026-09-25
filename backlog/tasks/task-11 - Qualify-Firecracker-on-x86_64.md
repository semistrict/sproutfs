---
id: TASK-11
title: Qualify Firecracker on x86_64
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
  - gce
dependencies: []
priority: high
type: task
ordinal: 11000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The recorded qualification is aarch64 only. Run it on GCE at the end, with the other cluster checks.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The Firecracker qualification suite passes on x86_64 GCE, and the run is recorded in docs/measurements
<!-- AC:END -->
