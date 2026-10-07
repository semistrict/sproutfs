---
id: TASK-108
title: 'Find the pager''s races between two lock holds, which no test interleaves'
status: In Progress
assignee: []
created_date: '2026-10-07 19:19'
updated_date: '2026-10-07 19:34'
labels:
  - vmmemory
  - testing
dependencies: []
references:
  - vmmemory/prefetch_campaign_test.go
  - docs/testing.md
priority: high
type: bug
ordinal: 144000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder found TASK-105 (a prefetch's READ request met another and the pager panicked), and the flaky forwards-read test this week was the same class of bug. Both were a check made under one hold of a lock and acted on under the next. The prefetch campaign has the exact setup, two forks of one checkpoint racing for the same pages, but it admits each guest at named points, one at a time, so that a seed replays. Nothing ever runs between two of those points, so a race inside that gap is never exercised. -race cannot see it either, because it is not a data race.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every place in vmmemory that releases a lock between a check and the act that depends on it is listed in the task; each is either made one hold or shown safe in its comment
- [ ] #2 Each lock release on a fault or prefetch path has a Buggify yield or an admission point, so seeded campaigns interleave there, and the prefetch campaign finds TASK-105's race with that fix reverted
- [ ] #3 An unseeded stress arm runs the prefetch campaign with real parallelism under -race on many cores in a soak, and is documented in docs/testing.md
- [ ] #4 A GCE test starts several forks of one cold template on one host at once and checks every page they read
<!-- AC:END -->
