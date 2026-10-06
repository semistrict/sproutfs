---
id: TASK-93
title: Find and cut the populate's per-page cost before a guest runs
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - performance
  - vm-memory
dependencies: []
references:
  - docs/measurements/gce-start-latency-2026-10-04.md
priority: high
ordinal: 113000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Every start maps the pages a host already holds before the guest runs. On GCE that took 115–122 ms for a 2 GiB root disk and 55–70 ms for RAM, one after the other: about 0.12 ms per 2 MiB page over only 12 mapping commands, so the cost is per page. 2 MiB in 0.12 ms is about 17 GB/s, memory-copy speed, which suggests something touches every byte (the VMM, the kernel's populate, or a check of ours). TASK-27 measures the populate bound on a cluster; this task finds the cause.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A profile on GCE names where the 0.12 ms per 2 MiB page goes
- [ ] #2 The cause is removed, or RAM and the root populate at once, or the root populates on first fault; the choice is justified
- [ ] #3 A GCE run of cmd/sproutfs-startbench shows time to running before and after
<!-- AC:END -->
