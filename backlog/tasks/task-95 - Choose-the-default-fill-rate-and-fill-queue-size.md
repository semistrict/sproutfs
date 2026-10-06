---
id: TASK-95
title: Choose the default fill rate and fill queue size
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 17:20'
updated_date: '2026-10-06 19:00'
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
- [x] #1 A GCE run measures publication time and memory at several rates and queue sizes
- [x] #2 The defaults are set from it and the reasoning is in docs/hosting.md
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Bench: node takes -fill-bytes-per-second; a publishing drive publishes a guest from a third node, then each guest alone (time, drops, queue peak, RSS), then paced chains of faults through the pager on the publisher and on a holder with nothing publishing and beside each publication.
2. GCE: six n2-standard-4 Ice Lake hosts; rates 128 MiB/s to unpaced x queues 64 MiB, 256 MiB, 1 GiB; guests of 2 GiB and 8 GiB; then 160, 192 and 256 MiB/s repeated at 64 MiB.
3. Set the defaults from the numbers; docs/hosting.md, deploy/README.md, docs/measurements/gce-fill-defaults-2026-10-06.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 owner decision: measure several fill rates and queue sizes on GCE, then set the defaults from the numbers.

GCE grid 2026-10-06 (docs/measurements/gce-fill-defaults-2026-10-06.md): publisher fault p99 beside a publication 27 ms at 128 MiB/s, 31 ms at 192, 50 ms at 256, 45 ms unpaced (idle 12 ms). 8 GiB commit 84 s, 54 s, 42 s, 30-48 s. Queue does not set the pace once the rate binds; 1 GiB queue costs 2.5 GiB more RSS. Chose 192 MiB/s and kept 64 MiB. just check passed.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
DefaultFillBytesPerSecond is now 192 MiB/s and DefaultFillQueueBytes stays 64 MiB, from a GCE grid of rates x queues with fault chains on the filling hosts. 192 MiB/s keeps the publisher's fault p99 near the paced figure (31 vs 27 ms; unpaced 45) and commits an 8 GiB guest in 54 s instead of 84. Bench gained -fill-bytes-per-second and fault chains beside publications (sim tests). Verified with the GCE runs and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
