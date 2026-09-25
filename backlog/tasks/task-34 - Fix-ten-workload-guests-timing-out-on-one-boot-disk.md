---
id: TASK-34
title: Fix ten workload guests timing out on one boot disk
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - gce
dependencies: []
priority: medium
type: bug
ordinal: 34000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Ten workload guests on one boot disk time out.** With `FORKS_BASE=4 FORKS_PER_REPO=2`, two guests exceeded the orchestrator's ten-minute exec limit, and the host logs showed nothing. Both host pods spill to the node's single pd-balanced disk. Three-and-two completed.
<!-- SECTION:DESCRIPTION:END -->
