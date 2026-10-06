---
id: TASK-98
title: Explain the 4 KiB dependent chain's tail on GCE
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - performance
dependencies: []
priority: low
ordinal: 118000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Random-fault planning did not improve the 4 KiB chain's tail on GCE (p90 4.37 → 5.27 ms, p99 9.82 → 13.41 ms). It was attributed, unverified, to first-touch page-table decoding and the chain's 10–15 prefetches. The slot allocator's run search is bit by bit and refused prefetches give slots back one at a time.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A GCE trace attributes the p90 and p99 to named steps
- [ ] #2 The largest is fixed or a task records why not
<!-- AC:END -->
