---
id: TASK-64
title: Measure a fork child's first-pass faults again
status: To Do
assignee: []
created_date: '2026-09-28 02:03'
labels:
  - performance
dependencies: []
references:
  - backlog/tasks/task-32 - Stop-copying-pages-a-guest-only-reads.md
priority: low
type: task
ordinal: 71000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The 2026-09-23 fan-out counted 13,226 faults in a fork child's first pass, at a mean of 1.01 ms: about 13 s of stall over its first run (TASK-32). A child starts with only the pages a sibling happens to hold resident mapped, and each fault's read-ahead is capped by the free arena slots it can reserve. The count predates TASK-50 (a fork's unpublished pages shared between its local children) and TASK-62, so it may already be much lower. Measure it again before deciding on either lever: a window larger than the free slots a fault can reserve, or populating the fork point's hot set.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An x86 GCE fan-out records a child's first-pass fault count, mean fault time and total stall on today's code, in both arena modes
- [ ] #2 The result says whether either lever is worth doing, and a follow-up is proposed only if it is
<!-- AC:END -->
