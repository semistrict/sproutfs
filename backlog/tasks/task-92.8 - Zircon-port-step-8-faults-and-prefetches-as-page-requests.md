---
id: TASK-92.8
title: 'Zircon port step 8: faults and prefetches as page requests'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 09:24'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.7
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 106000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 8 of the plan. Zircon asks its pager for missing pages with page requests that batch overlapping ranges, wake every waiter a supply covers, and can be cancelled (zircon/kernel/vm/page_source.cc). A fault's read and a prefetch become READ requests to a provider over the volume, the cluster and a peer, and a fault that meets a page a prefetch is reading waits on that request. PagerProxy's port packets become a goroutine per request. Which faults prefetch and in what order the pages are read stays ours.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Page requests are ported from page_source.cc; every backing read of a fault or a prefetch goes through one
- [x] #2 The prefetch Buggify sites, the nine prefetch probes and the guards pager-read-in-flight-again, pager-read-alone-at-random, pager-prefetch-every-fault, pager-plan-the-window-at-random, pager-read-the-run-first, pager-plan-the-window-first and pager-fault-waits-for-its-prefetch keep their names and are killed or reached as before
- [x] #3 TestPrefetchCampaignReplaysItsSeeds and TestPrefetchSurvivesItsFaultsAndReachesItsProbes pass, with the sim.Admit points and the priced planning work under their names
- [x] #4 BenchmarkARandom4KiBFault and BenchmarkAForward4KiBFault are recorded before and after on one machine, and neither median is slower by more than the spread of two runs before
- [x] #5 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Complete zirconvm/pagesource.go in place: a query of the outstanding requests a range meets (the pager here is in the caller's process and asks before it sends), with the departure marked.
2. Port object/pager_proxy.cc and object/include/object/pager_proxy.h as zirconvm/pagerproxy.go: the provider of a source whose pager is in this process. No port: the fault or prefetch that sends a request answers it on a goroutine of its own. Tests for both.
3. vmmemory: a page source per identity root (checkpoint, volume) while a prefetch reads it, keyed under the host lock, and one per memory region. A prefetch sends READ requests on the roots of its pages; overlapping ones batch (the later prefetch leaves those pages to the earlier). A fault meeting a prefetch makes a READ request that waits on the prefetch's; the prefetch's supply or failure wakes every waiter it covers. A fault's own reads are READ requests on its region's source, so a prefetch does not see them, as today (TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped).
4. Delete Host.inflight, prefetch.done and the per-identity in-flight map; keep the bound, pressure cancel, policy, probes, sites, guards and Admit points in place.
5. Docs: vm-memory.md (package layout, prefetch, reading forwards), testing.md if it names the mechanism.
6. Verify: whole suite both arena modes, host, vmmigrate, internal/simtest, vmmachine; check-guards; campaign replay; race on vmmemory and zirconvm; benchmarks before/after 10 runs alternating; Gremlins on changed zirconvm files; just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported PageSource, PageRequest and MultiPageRequest into vmmemory/internal/zirconvm/pagesource.go ahead of this step, with a PageProvider interface and no early wake. This step adds PagerProxy as a goroutine and wires faults and prefetches.

Design. zirconvm/pagesource.go is completed in place: AppendOutstanding is the one addition, marked as a departure (the pager is in its callers' process and asks which pages a request already sent reads). zirconvm/pagerproxy.go ports object/pager_proxy.cc and its header: the provider keeps the requests it holds (Holds) and Zircon's wait counters; no port or packet queue, because the fault or prefetch that sends a request answers it on a goroutine of its own (the faulting page's read goroutine, the prefetch's goroutine; a page read alone, a run and a store's copy read are answered on the fault's goroutine, which waits on nothing else). vmmemory/pagerequests.go: a page source per identity root (checkpoint, volume) while a prefetch reads it (Host.roots, under Host.mu, which serializes every send and answer of a root's requests), and one per memory region (MemoryRegion.reads) for a fault's own reads.
Decision: a fault's own reads go to its memory region's source, not to the root's, so prefetches do not see them. That keeps TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped and the duplicate probe as they are (AC 5 and AC 2): the test says a fault's own read is not in flight for prefetches to see. Batching on the roots is prefetch with prefetch (the later leaves those pages to the earlier) and fault waiting on prefetch (the fault's READ request waits on the prefetch's; the prefetch's supply, or failure when its read fails, wakes every waiter at once).
Deleted: Host.inflight (the per-identity map of prefetches), prefetch.done, and the select on the faulting read's done channel; the fault now waits on its page's request. The prefetch's cancel for pressure stays its context; its read then fails and it fails its requests' range, which is Zircon's failure of a range rather than CancelRequest, because waiters must wake. The bug pager-fault-waits-for-its-prefetch now waits on the prefetch's requests.
Validation so far: vmmemory, host, vmmigrate, internal/simtest, vmmachine pass in both arena modes; the eight prefetch guards killed in each of 2 runs; campaign tests pass three times.

Benchmarks on the Mac (M5 Pro), before (b0d5af86) and after binaries alternated, ten runs each, three rounds; rounds 2 and 3 ran beside other agents' load. Medians, ns/op, before -> after: BenchmarkARandom4KiBFault 2963 -> 3026, 3404 -> 3697, 4264 -> 4288; BenchmarkAForward4KiBFault 365771 -> 358524, 409924 -> 405486, 504686 -> 469751. Allocations unchanged (38 and about 1950 a fault). The spread of two runs before is 441 ns for the random fault (rounds 1 and 2); the quiet round's +63 ns (2%) is within it, and the forward fault is faster in every round (each root is asked once a window for its prefetches' requests, where the in-flight map was asked once a page).
Gremlins on pagesource.go and pagerproxy.go: first run 68 killed, 25 lived, 4 not covered; with tests of an out-of-order supply, a failure covering a waiter's start or ending where the next request begins, and a request spanning one already sent: 77 killed, 17 lived, 3 not covered. The 3 not covered are the cancel switch's case conditions; each fails the package's tests when applied by hand. The 17 survivors are asserts' bounds reached only by empty or overflowing ranges, the length of a waiting request (only its start is read), the insert position among requests that never overlap, and boundaries both sides of which agree.
Race: go test -race ./vmmemory ./vmmemory/internal/zirconvm passes. Guards: 154 of 154 killed in just check; the eight prefetch guards killed in each of two runs. TestPrefetchCampaignReplaysItsSeeds and TestPrefetchSurvivesItsFaultsAndReachesItsProbes pass (run three times).
Seen while checking, not caused here: cmd/sproutfs-restorebench TestEveryCaseReadsTheGuestBack is flaky on b0d5af86 too (2 of 40 runs fail; the 2MiB/sequential/page/16 cluster case sometimes hedges). just check passed on its second run.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Ported Zircon's page requests in place: zirconvm/pagesource.go completed (AppendOutstanding added as a marked departure) and object/pager_proxy.cc ported as zirconvm/pagerproxy.go, with cases of the port's own. Every backing read of a fault or a prefetch now answers a READ request: a prefetch's go to the identity roots of its pages, so prefetches batch and a fault on a page a prefetch reads waits on its request, which the prefetch's supply or failure wakes; a fault's own reads go to its memory region's source, which prefetches do not see, as before. Deleted Host.inflight and prefetch.done. Verified: the suites of vmmemory, host, vmmigrate, internal/simtest and vmmachine in both arena modes, race on vmmemory and zirconvm, every guard, the campaign replay, Gremlins (77 killed, 17 equivalent survivors), and alternated benchmarks within the spread of two runs before.
<!-- SECTION:FINAL_SUMMARY:END -->
