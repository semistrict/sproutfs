---
id: TASK-33
title: Re-take the workload measurement with multi-page parts
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - measurement
dependencies: []
priority: low
type: task
ordinal: 33000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**The workload measurement predates multi-page parts and needs to be re-taken.** It was measured with one object per dirty page, before `39bfe37`. So its object counts describe a store layout that no longer exists. Only one fork setting (`FORKS_BASE=2 FORKS_PER_REPO=1`) was run. The commands to re-take it on current `main` are in the document (`docs/measurements-2026-09-14-workload.md`).
<!-- SECTION:DESCRIPTION:END -->
