---
id: TASK-22
title: Measure fair-share eviction on x86_64 with many guests
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 01:50'
labels:
  - measurement
  - gce
  - deferred
dependencies: []
priority: medium
type: task
ordinal: 22000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Fair-share eviction (0f486bb) was measured once on Lima with two 128 MiB guests. It is unmeasured at production sizes and on x86_64.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A GCE run with many production-size guests records neighbour latency and evictions with the rule on and off
<!-- AC:END -->
