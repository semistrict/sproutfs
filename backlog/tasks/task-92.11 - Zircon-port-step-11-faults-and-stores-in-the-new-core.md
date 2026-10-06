---
id: TASK-92.11
title: 'Zircon port step 11: faults and stores in the new core'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 16:47'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.10
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 109000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 11 of the plan. Read faults, prefetch, population, stores, write-ahead, placement and the two rules of the old core (vmmemory/fault.go, faultfirst.go, window.go, population.go, placement.go, rules.go) run over the region layer and identity roots. The policy is ours and moves as it is: the page first, a fault at random alone, the populate budget, the private page at its own offset, the gap and half-private rules. Mapping commands are collected under the object lock and issued after it, as DeferredOps does in Zircon.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Under SPROUTFS_PAGER_CORE=zircon the fault, prefetch, population, store, write-ahead, placement and rules tests of vmmemory pass, and they are on the list just check runs
- [x] #2 vm_mapping_page_fault_optimisation_test and vm_mapping_page_fault_range_test run as Go tests of mapping a fault's resident neighbours
- [x] #3 The guard pager-zero-new-page and the fault-policy guards are killed under both cores
- [ ] #4 No mapping command is issued with an object lock held, shown by a test that holds the client's answer; go test -race of vmmemory passes on the Mac, and the hostile Linux suites pass under the new core on GCE
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Split into green commits, as the step's size asks: reads; stores and write-ahead; population, placement and the rules.
1. zirconvm: a VmPage may name a Frame, Zircon's paddr, in place of bytes in this process; the package's copies and zeroings assert they have bytes here. The alias set is generic so both cores keep one per page.
2. The zircon core's objects: a Pmm over the arena whose pages are frames (a slot of an arena file), an identity root per (checkpoint, volume) with its page source, a region layer per memory region whose resolver names the root of each offset the fault has located, and a binding beside the layer for what Zircon has no place for (mapped, zero runs).
3. Reads: the fault's lookup is the layer's lookup cursor (RequireReadPage), a missing page a READ request on its root's source that the fault answers by reading the page into a frame and supplying it to the root; the fault maps the page with its resident neighbours (IfExistPages, the two aspace tests). The policy moves as it is: page first, a fault at random alone, a stream's run at once, prefetch behind the page as READ requests on the roots, a fault meeting a prefetch waits on its request. Every Buggify site, probe, guard and sim.Admit point on these paths keeps its name. Mapping commands are collected under the object lock and issued after it.
4. Population over the roots' resident pages, the budget as it is.
5. Stores and write-ahead over the region layer: a store copies a root's page into a frame of the region's private file at its placed offset and supplies it Dirty; fresh zeros become Dirty frames in one command; the gap and half-private rules.
6. Attach, detach, statistics and idle pages under the zircon core; eviction stays step 12, so the core refuses what would evict.
7. The tests of fault, prefetch, population, store, write-ahead, placement and the rules that pass under the zircon core go on scripts/pager-core-zircon.json; guards on these paths get cores current and zircon; a test holding the client's answer shows no command is issued under an object lock; go test -race; benchmarks under both cores (alternate binaries, 10 runs, medians); Gremlins on the new files.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Reads (076dc97a) and stores (029cb96e) are in, as two green commits.
Design: a page is a zirconvm.VmPage whose Frame is an arena slot (zirconvm gained VmPage.Frame/NewFramePage; its byte copies, zeroings and compressions assert the bytes are in this process). The arena is the node's Pmm but makes no page: the pager fills frames where placement and isolation put them and supplies them. A published (checkpoint, volume) is an identity root, made when a region first locates one of its pages and kept until the pager closes. A region's layer is CreateRegionLayer over the region's own source, which traps dirty transitions; its resolver names the root of each located page a root holds. A zbinding beside the layer keeps mapped, zero runs (Untracked zero intervals), the page mapped, and of a stored page its reservation, origin and ahead mark.
Reads: the faulting page's lookup is the layer's lookup cursor (RequireReadPage, no DeferredOps needed). A page no root holds is a READ request on the region's own source, answered by reading into a frame and supplying it to its root (SupplyPages), then OnPagesSupplied on the region's source. Decision kept from step 8: a fault's own read is invisible to prefetches; a fault meeting a prefetch waits on the prefetch's request in the root first (inFlight). Pages are bound to the region under their object's lock, so an idle drop cannot take a page between lookup and binding. Policy ported as it is: planFault, readAlone/readFirst/readRun, survey with one root-lock hold per run of one root (the fault-around of present pages), provisional runs, prefetch as root READ requests that land idle in the don't-need queue and are then mapped into the asking region, population over the roots with the shared budget (groupResidentRuns/affordRuns factored out of population.go). Idle pages are given up with ReclaimRangeForEviction (DropIdle, makeRoom, the budget's cache, allocations). Stores: needsPrivatePage/takeSpill as in Fault; storeFresh/storeZeros (write-ahead into fresh zero frames, supplied and made Dirty with DirtyPages, mapped by one command); copyOnWrite (readIn of the faulting window binding the origin unmapped, copy into the placed slot, supply + DirtyPages, the origin held until the command lands, closeAround with the gap and half-private rules, the refusal backstop makeWhole); a page read with no identity is dirtied in place. Pressure, the loss window and the dirty budget are shared (dirtyCount and OldestUnpublished dispatch).
Mapping commands are never issued under an object lock: TestAFaultIsServedWhileAnotherFaultsCommandIsUnanswered (read and store variants) holds a fault's Map unanswered while a read and a store in other windows of the same region over the same checkpoint are served; with the layer lock held across install it hangs (checked by hand: the run timed out with the second fault blocked in lookup).
Aspace tests ported as Go tests under both cores: TestAFaultMapsTheResidentPagesOfItsWindowAndNoOthers (vm_mapping_page_fault_optimisation_test) and TestAFaultReadsNothingForTheResidentPagesItMapsAndStopsAtItsWindow (vm_mapping_page_fault_range_test).
Guards: the eight fault-policy guards and pager-zero-new-page carry cores [current, zircon] in guards.json; check-guards kills all 18 runs. pager-zero-new-page moved from internal/simtest TestScheduledWorldReproduces (which needs checkpoints under the zircon core) to vmmemory TestEveryPageAFaultReadsOrAStoreCopiesHoldsItsBytes. The zircon allocation keeps pager-prefetch-ignores-pressure and vmmemory/evict-past-a-free-slot (here: give an idle page up past a free slot). Every prefetch site, probe and sim.Admit point of the fault path exists in the zircon core under its name; ProbePrefetchHeld is unreachable while peer backings are refused.
List: 105 vmmemory tests in scripts/pager-core-zircon.json pass under the zircon core in both arena modes, three times each and under -race. go test -race ./vmmemory/... passes (current core).
Benchmarks (Mac M5 Pro, one test binary, cores alternated by SPROUTFS_PAGER_CORE, 10 runs each, medians, load average 5.3 to 5.7): BenchmarkARandom4KiBFault current 3061 ns/op (2976-3110, 38 allocs) vs zircon 3034 ns/op (3006-3132, 36 allocs); BenchmarkAForward4KiBFault current 359.3 us (349.2-370.6) vs zircon 355.2 us (348.6-361.9). The first zircon version cost the random fault +10.8% (3363 ns, 51 allocs): a splice list per supply and a page request per lookup. Supplies now reuse their splice lists (zirconvm PageSpliceList.Reuse, a marked Go departure) and lookups their MultiPageRequests.
Not served yet, refused with ErrCoreUnsupported or ErrCapacity, all step 12's by the plan: seals and everything a checkpoint does, eviction of a mapped page, cold copies and give-back, serving and handoff, peer (UnpublishedLoader) backings. So the category tests that seal or evict to check what they stored stay off the list: TestStoreIntoFreshZeroPageIsOneMappingCommand, TestSequentialStoresIntoFreshMemoryTakeOneFaultPerRun, TestWriteAheadTakesOnlyFreeArenaSlots, TestWriteAheadTakesOnlySpareDirtyReservations, TestACheckpointPublishesEveryWriteAheadPage, TestWriteAheadAgainstIndependentByteModel, both zero_ahead tests, TestARangeThatIsHalfPrivateBecomesWhole, TestASettleHandsBackTheGapPagesTheGuestNeverWrote, TestAWholeRangeStaysWholeThroughASettle, TestARuleNeverMapsAPageOntoAnOffsetHoldingAnotherPage, TestEveryPageAStoreMakesPrivateIsInTheRunItMaps, TestPopulationTakesAForkPointsPagesWhateverTheirRun, TestAForkPointsPagesTakeTheBudgetFirstAndAreBoundedByIt, TestAPrefetchTakesOnlyFreeSlots, TestAPrefetchLeavesAPageTheSourceHoldsToItsFault, both prefetch campaigns (peer backing, eviction), TestSparseSiblingsSurviveArenaPressureWithoutRevocation.

Gremlins (scripts/mutate-gremlins.py gained --pager-core, since the wrapper strips SPROUTFS_ variables) on zircon.go, zircon_frames.go, zircon_bindings.go, zircon_fault.go, zircon_window.go, zircon_prefetch.go, zircon_population.go, zircon_stats.go, zircon_store.go and core.go, each mutation running the listed tests under the zircon core: first run 536 killed, 201 lived, 119 not covered, 72 timed out. Tests of the survivors that mattered, run under both cores (store_bounds_test.go: the write-ahead run's growth and reservations, the half-private rule at exactly half, the gap rule at its bound from either side and from a range's first page; window_reads_test.go: a fault over pages of two checkpoints, which caught a stale resolver cache in mutation, a prefetch leaving pages another prefetch reads and the prefetch bound, an attach after the region of zeros detached reading no metadata; content_test.go's counts of copies, store traps, idle and dirty pages), and two zirconvm cases for the departures (a page at a Frame has no bytes here; a reused splice list and a supplied page's backlink): second run 569 killed, 174 lived, 112 not covered, 73 timed out. What lives: slice sizes and resets after a plan is unlocked (equivalent), error branches no fault reaches without injected failures, counters only eviction reads (MemoryRegion.resident), the stream's reserveAround and the isolated arena's reserveOwn boundaries, and the paths step 12's tests reach (refusal backstop, allocation under pressure).
Not done, for step 12 by the plan: AC 1 holds for every listed test, but the category tests that seal, evict a mapped page, give cold copies back or attach a peer backing to check what they did are not on the list (named above). AC 4's GCE run of the hostile Linux suites under the zircon core is not run: their neighbour publishes through Seal and Retire (hostile_linux_test.go:428, hostile_client_linux_test.go:250), which this core refuses until step 12.

Step 12 (TASK-92.12) finished what this step left: the zircon core serves seals, eviction, cold copies, isolation, serving and peer backings, and just check runs the whole vmmemory, host, vmmigrate, simtest and vmmachine suites under it in both arena modes, so AC 1 holds for every test (the named list is gone). AC 4: go test -race of vmmemory passes on the Mac under the zircon core. On GCE the hostile suites pass under the zircon core in both arenas (the dev VM runs of the whole Linux suite, both page sizes), but one hostile test, TestARefusedFaultWaitsForARevocation, fails under either core on main: its command count is 4 or 5 against the 3 it wants. So AC 4 stays open on that test, which is not the zircon core's.
<!-- SECTION:NOTES:END -->
