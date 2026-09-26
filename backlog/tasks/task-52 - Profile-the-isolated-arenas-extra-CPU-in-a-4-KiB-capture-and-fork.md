---
id: TASK-52
title: Profile the isolated arena's extra CPU in a 4 KiB capture and fork
status: To Do
assignee: []
created_date: '2026-09-26 22:14'
labels:
  - performance
dependencies: []
priority: medium
ordinal: 59000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
In the GCE worst-case run of 2026-09-26, a 4 KiB capture in isolated mode took twice the upload time and host CPU of shared (138 s against 67 s), and a 4 KiB fork 1.7x the time. The BLAKE3 digests do not explain 71 s of extra CPU, and the fork copied only 72 MiB into its fork file. The cause needs a CPU profile of the host during each.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A CPU profile of each names where the extra time goes
- [ ] #2 The cause is fixed or a task is filed for it
<!-- AC:END -->
