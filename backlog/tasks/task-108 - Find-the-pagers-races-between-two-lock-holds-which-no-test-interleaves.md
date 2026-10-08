---
id: TASK-108
title: 'Find the pager''s races between two lock holds, which no test interleaves'
status: Done
assignee: []
created_date: '2026-10-07 19:19'
updated_date: '2026-10-08 09:40'
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
- [x] #3 An unseeded stress arm runs the prefetch campaign with real parallelism under -race on many cores in a soak, and is documented in docs/testing.md
- [x] #4 A GCE test starts several forks of one cold template on one host at once and checks every page they read
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Audit of all 67 gaps (scratchpad list) by six agents, merged to main at e9dedf71. Each gap is commented safe or fixed with a seam test and a guard (vmmemory/lockgap_*_test.go). Fixed: detach vs eviction (eviction holds region live), prefetch waiter on a supplied request, unindex into a detaching owner, run read by stale name, run read deadlocking a retire, refault mapping by stale protection, allocateOwn freeing a page a settle handed back (evictIfIdle), cold-copy loss on a failed compare, lookup into a root that dropped its page (Holds; F1, TASK-105 by another path) and into an ended lent root (F2), lent names outliving pages (unlend), seal end destroying a lent root under readers, share after seal end, fork number freed before its drop, protection lost through an abandoned seal, ReadDirty vs detach, five store bugs (rule across windows, rule deadlock, joined page evicted, make-whole onto checkpoint copy, wait for an extent gone elsewhere) and copyOnWrite of a page read into the region's own file. Admission points added on fault, store, prefetch, eviction, give-back and fork paths (listed in docs/testing.md); a seeded fork campaign added. Open: TestPrefetchCampaignReplaysItsSeeds flakes (seed 1, prefetch-cancelled-for-pressure differs), being chased; no campaign reaches the mapping rules or the refault/capture gap.

Unscheduled soak fixes since: capture protection read before the protection, lost reads counted as decisions (ErrContended), prefetch request read after send, replaced page let go before the store's command, orchestrator recovery past a serving source, and (3cb697d8) allocateOwn counting a page an eviction is taking as mapped (ErrCapacity 'both places'). Still open, each about 1 in 70 Mac soak runs: a writable resolve of a page mapped read-only after an unseal (the parent's store is lost), and keepFork giving a child a fork file whose seal ended (nil map). Being soaked on GCE with map, revoke and fork-file histories.

18c8fe93 fixes the keepFork nil map: dropLentRoot now takes the lent pages out under their locks before the copies, since a retire that publishes a page under another name leaves its lent page (TestTheEndOfASealWaitsForAChildsCopyOfAPageTheRetirePublishedElsewhere, guard pager-drop-lent-copies-before-their-lent-pages).

Mapping audit (7b8680ab, 86114c0e): every vmmemory test checks the pager's resolves and bindings against what each mapping command installed; it turned the lost store (about 1 in 70 soak runs, as an invalid resolution) into a finding in five runs and then into its cause, fixed in d17581af: a fault let its own unmapped page's lock go before its lookup, an eviction spilled the page, and the lookup bound a root's page or the volume's bytes over the guest's own (TestAFaultRefaultsItsOwnPageAnEvictionSpilledBeforeItsLookup, guard pager-look-a-spilled-page-up-past-its-layer). AC4: the cold-forks test (branch cold-forks-gce) ran on GCE and every fork stalled on a dirty budget sized to its stamps; budgets resized, rerunning.

AC4 verified on GCE (n2-standard-8, nested KVM, 2026-10-08): TestForksOfOneColdTemplateStartAtOnceAndReadEveryPage passed with 2 forks (7.4 s; 1,045 evictions, 784 refaults, 249 identity hits) and with 4 forks (27 s phase; 15,380 evictions, 13,451 refaults, 3,671 identity hits), every page of every fork correct. Eight forks thrash that host until the guests stall, so four is the default. Its first runs found two test faults (a dirty budget sized to the stamps, and the console's carriage return), fixed in 93b133bf's series; the memory benchmark now stages source with stage-source.py (1effedf4).

AC3 verified: the unscheduled soak (TestThePagersCampaignsSoakWithoutAScheduler, prefetch, fork and rules worlds, real goroutines under -race, mapping audit on) ran on a 22-core GCE VM via scripts/soak-pager-gce.sh on d09c46e3: 24,109 worlds at 4 KiB and 367 at 2 MiB in the isolated arena, 24,499 and 368 in the shared one, all clean; documented in docs/testing.md. Earlier soaks found and drove the fixes for the lost store (d17581af), the eviction place (3cb697d8), the lent root's copies (18c8fe93) and the capture and settle deadlock (d09c46e3); the soak now ends itself with every stack when a world hangs (88938001).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Audited all 67 lock gaps in vmmemory: each is one hold or commented safe, with seam tests and sim.Bug guards; admission points let the seeded prefetch, fork and rules campaigns interleave at them, and Buggify draws are keyed per task. Added an unscheduled soak (real goroutines, -race, a GCE script) and a mapping audit in every vmmemory test, which together found and drove fixes for a lost store after an unseal (d17581af), an eviction's place counted taken (3cb697d8), a lent root's copies outliving their file (18c8fe93) and a capture/settle deadlock (d09c46e3); the writeback TLA+ spec models the lookup (733638ba). Verified: the 22-core GCE soak on d09c46e3 ran about 49,000 worlds in both arenas clean; the cold-forks Firecracker test passes on GCE with 2 and 4 forks; just check passes, every new guard is killed.
<!-- SECTION:FINAL_SUMMARY:END -->
