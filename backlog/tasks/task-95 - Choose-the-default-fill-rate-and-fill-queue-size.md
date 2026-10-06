---
id: TASK-95
title: Choose the default fill rate and fill queue size
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
updated_date: '2026-10-06 15:42'
labels:
  - decision
  - disk-cache
dependencies: []
priority: medium
ordinal: 115000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
CacheConfig.FillBytesPerSecond defaults to 128 MiB/s, which caps a paced publication near the old one-at-a-time pace; the GCE benches ran at 4 GiB/s. The fill queue default is 64 MiB against 1 GiB (38 s against 28 s for 8 GiB, with more memory). Sizes in between are not measured.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A GCE run measures publication time and memory at several rates and queue sizes
- [ ] #2 The defaults are set from it and the reasoning is in docs/hosting.md
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 owner decision: measure several fill rates and queue sizes on GCE, then set the defaults from the numbers.
<!-- SECTION:NOTES:END -->
