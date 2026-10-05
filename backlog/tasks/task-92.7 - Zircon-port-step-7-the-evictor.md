---
id: TASK-92.7
title: 'Zircon port step 7: the evictor'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 10:30'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.6
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 105000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 7 of the plan. The victim loop of Host.allocate (vmmemory/allocation.go) becomes the synchronous path of Zircon's evictor (zircon/kernel/vm/evictor.cc) over the page queues, with ReclaimPage's split: an identity root's clean page is dropped, a region's own page is spilled. The fair share stays ours as a filter on the candidates: Zircon protects VMOs by priority and hints, not by a share of the arena. Idle pages go first, then prefetches still reading are cancelled, then a mapped page is evicted, as today.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The evictor is ported from evictor.cc with its targets; an allocation short of a slot uses its synchronous path
- [x] #2 Six cases of vm/unittests/evictor_unittest.cc run as Go tests in synctest bubbles; evictor_discardable_test is left out
- [x] #3 The Buggify site vmmemory/evict-past-a-free-slot, the guard pager-prefetch-ignores-pressure and the probe vmmemory/eviction-during-publication sit in the evictor under the same names; every campaign that requires them to fire or be reached still passes
- [x] #4 The fair-share and eviction-ordering tests pass unchanged, and every test in vmmemory, host, vmmigrate and internal/simtest passes in both arena modes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Port vm/evictor.cc and vm/include/vm/evictor.h to vmmemory/internal/zirconvm/evictor.go: targets, combine, preloaded and external targets, EvictSynchronous, EvictAsynchronous with its goroutine, EvictUntilTargetsMet, EvictPageQueues, the failure diagnosis, and ReclaimFromGlobalPageQueues over a Node's queues. The reclaim and free-page functions are Zircon's test hooks, and the pager supplies its own through them. Departures, each commented: a synchronous eviction names whom it evicts for (the pager's fair share needs it); the eviction lock guards the counts, not a reclaim (a reclaim here waits on other VMMs); a reclaim can fail with an error; the goroutine starts at the first asynchronous request (a pager never makes one, and an idle goroutine outlives a synctest bubble); counters per evictor.
2. Port the six cases of vm/unittests/evictor_unittest.cc (not evictor_discardable_test) in synctest bubbles, plus a case of the node path.
3. Replace the queue walks Reclaimable, AnonymousZeroFork and DontNeed with filtered peeks (PeekIsolateWhere, PeekAnonymousZeroForkWhere, PeekDontNeedWhere): a filter passes over a page without moving it, so the fair share does not change recency.
4. vmmemory/evictor.go: the pager's reclaim step (idle page, then cancel prefetches, then fair, unfair and pinned candidates; give back, drop or spill), the Buggify site, the guard and the probe. Host.allocate calls EvictSynchronous for one page. No asynchronous trigger: idle pages are kept for VMs that inherit them, so nothing evicts ahead of a shortage.
5. Whole suite both arena modes, -race on vmmemory and zirconvm, check-guards, benchmarks old/new, Gremlins on changed zirconvm files, docs, just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Ported vm/evictor.cc and evictor.h to vmmemory/internal/zirconvm/evictor.go, generic over what a synchronous eviction is for. Departures, each commented: a request is handed to each reclaim (the fair share needs the region); the eviction lock guards the counts only, not a round of reclaims (a reclaim here revokes mappings from other VMMs, and one that stops answering would stop every eviction on the host); a reclaim can fail with an error that ends the eviction; the eviction goroutine starts at the first EvictAsynchronous (an idle goroutine outlives a synctest bubble, and the pager makes no asynchronous request); counters per evictor (Stats for GetGlobalStats). Not ported: discardable VMOs, loaned pages, the queues' Dump. Pmm gains CountFreePages (pmm_count_free_pages).
Queue walks Reclaimable, AnonymousZeroFork and DontNeed are replaced by filtered peeks (PeekIsolateWhere, PeekAnonymousZeroForkWhere, PeekDontNeedWhere). A page the filter refuses stays where it is: Zircon's way, MarkAccessed, would make a protected page look recently faulted. A lowest queue below the active ones ages the queues until the active pages are inactive, since aging is by faults only (decision 4). Pages stays for the stats. The walk tests in pagequeues_test.go are rewritten against the peeks; one expectation changes: a page marked accessed in a generation comes after a page set there earlier, because Zircon's LRU processing puts it at the head of that generation's list.
vmmemory/evictor.go: the pager's reclaim step (idle page; cancel prefetches; fair, unfair, then zero-fork candidates; give back, drop or spill), evictPage (was evictBatch, one victim), the Buggify site (evictPastAFreeSlot, called where the four sites were), the guard and the probe. Host.allocate calls EvictSynchronous for one page; its sim.Admit points stay in allocate. No asynchronous trigger: an idle page is kept for the next VM that inherits it, so nothing evicts ahead of a shortage.

Validation: go test of vmmemory, host, vmmigrate, internal/simtest (Buggify and probe campaigns), vmmachine pass in both arena modes; go test -race ./vmmemory ./vmmemory/internal/zirconvm passes; check-guards 155 of 155 killed (pager-prefetch-ignores-pressure among them); just check exit 0 after merging main (fbe90cd4).
Benchmarks (Mac, old/new binaries alternated, 10 runs each, medians; load average 5 to 10 from other agents): BenchmarkARandom4KiBFault 3006 vs 3031 ns (+0.8%, ranges 2957-3044 vs 2979-3060); BenchmarkAForward4KiBFault 357.2 vs 362.9 us (+1.6%, ranges 352-378 vs 358-372); fourteen eviction, fair-share, cold-copy and dirty-budget tests together 4.048 vs 4.058 s (+0.2%); TestAMemoryRegionKeepsItsShareWhileItFaults x20 1.061 vs 1.068 s (+0.7%).
Gremlins on evictor.go and pagequeues.go: first run 226 killed, 42 alive, 33 not covered, 9 timed out; after tests 244 killed, 24 alive. Evictor survivors: equal-bound boundaries (minimum met exactly, carried minimum of zero), async path log lines; pagequeues survivors are step 5's plus a peek bound that only does more work.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Ported Zircon's evictor (vm/evictor.cc, evictor.h) to vmmemory/internal/zirconvm/evictor.go with six of its seven unit tests in synctest bubbles, plus tests of the node path, the async goroutine, errors, rounds and the diagnosis. Host.allocate's victim loop is now the evictor's synchronous path: vmmemory/evictor.go supplies the reclaim step (idle page, prefetch cancel, fair-share filtered candidate, any page, zero-fork last; give back, drop or spill) and holds the Buggify site, guard and probe. The temporary queue walks are replaced by filtered peeks. Verified by the whole suite in both arena modes, -race, check-guards and just check; fault benchmarks within 1.6%.
<!-- SECTION:FINAL_SUMMARY:END -->
