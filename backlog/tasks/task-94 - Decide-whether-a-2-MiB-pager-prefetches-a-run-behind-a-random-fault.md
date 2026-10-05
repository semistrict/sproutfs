---
id: TASK-94
title: Decide whether a 2 MiB pager prefetches a run behind a random fault
status: To Do
assignee: []
created_date: '2026-10-05 17:20'
labels:
  - decision
  - vm-memory
dependencies: []
references:
  - docs/measurements/gce-real-app-restore-2026-10-04.md
priority: medium
ordinal: 114000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
de7fc948 made 2 MiB pagers prefetch the rest of a run behind a random fault. Valkey's 20,000 dependent GETs from the store went from 99.8 s to 32.5 s and from the cluster from 22.5 s to 11.7 s, but the synthetic 2 MiB chain went from 7.1 to 10.4 ms a hop. It is on main and needs the owner's decision.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner keeps or reverts the policy, and the decision is recorded in docs/vm-memory.md
<!-- AC:END -->
