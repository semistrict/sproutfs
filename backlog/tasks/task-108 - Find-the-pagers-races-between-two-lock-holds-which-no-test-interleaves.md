---
id: TASK-108
title: 'Find the pager''s races between two lock holds, which no test interleaves'
status: In Progress
assignee: []
created_date: '2026-10-07 19:19'
updated_date: '2026-10-07 20:40'
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
- [x] #1 Every place in vmmemory that releases a lock between a check and the act that depends on it is listed in the task; each is either made one hold or shown safe in its comment
- [x] #2 Each lock release on a fault or prefetch path has a Buggify yield or an admission point, so seeded campaigns interleave there, and the prefetch campaign finds TASK-105's race with that fix reverted
- [ ] #3 An unseeded stress arm runs the prefetch campaign with real parallelism under -race on many cores in a soak, and is documented in docs/testing.md
- [ ] #4 A GCE test starts several forks of one cold template on one host at once and checks every page they read
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Audit of all 67 gaps (scratchpad list) by six agents, merged to main at e9dedf71. Each gap is commented safe or fixed with a seam test and a guard (vmmemory/lockgap_*_test.go). Fixed: detach vs eviction (eviction holds region live), prefetch waiter on a supplied request, unindex into a detaching owner, run read by stale name, run read deadlocking a retire, refault mapping by stale protection, allocateOwn freeing a page a settle handed back (evictIfIdle), cold-copy loss on a failed compare, lookup into a root that dropped its page (Holds; F1, TASK-105 by another path) and into an ended lent root (F2), lent names outliving pages (unlend), seal end destroying a lent root under readers, share after seal end, fork number freed before its drop, protection lost through an abandoned seal, ReadDirty vs detach, five store bugs (rule across windows, rule deadlock, joined page evicted, make-whole onto checkpoint copy, wait for an extent gone elsewhere) and copyOnWrite of a page read into the region's own file. Admission points added on fault, store, prefetch, eviction, give-back and fork paths (listed in docs/testing.md); a seeded fork campaign added. Open: TestPrefetchCampaignReplaysItsSeeds flakes (seed 1, prefetch-cancelled-for-pressure differs), being chased; no campaign reaches the mapping rules or the refault/capture gap.
<!-- SECTION:NOTES:END -->
