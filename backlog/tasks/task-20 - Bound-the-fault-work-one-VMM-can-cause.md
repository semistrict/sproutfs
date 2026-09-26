---
id: TASK-20
title: Bound the fault work one VMM can cause
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 16:41'
labels:
  - security
dependencies: []
priority: medium
type: bug
ordinal: 20000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A hostile VMM can re-fault its own memory without limit, which costs the pager CPU that other VMs need. Found by the hostile-VMM fuzzing of 2026-09-25.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A session that faults beyond its share is slowed or ended, and a neighbour's faults keep their latency
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Measure in Lima: a VMM dropping its own page tables (madvise DONTNEED) and reading them back costs the pager ~40 us of CPU per fault; one such VMM at 64 workers took 3.5 processors at 90k faults/s and raised the neighbour's store latency 2.5x median, 10x max.
2. Mechanism: pace repeated faults. A repeated fault is one on a page the memory region already maps for the access (zero-mapped or mapped for a read, mapped writable for a store): it changes nothing. Every other fault loads, maps or copies a page and is bounded by the resident, dirty and mapping budgets; a guest under memory pressure re-faults through loads, which are never repeated. Each session gets a budget (GCRA: a burst, then a steady rate); a repeated fault past it waits in its own worker, costing no CPU and slowing no other session. A twin of a progress fault in flight (two vCPUs on one page) is free; a twin of a repeat or a twin is not, so twins cannot chain.
3. Code: vmmemory/repeats.go (budget, constants, docs), MemoryRegion.repeated in bindings.go, pacing in connection_linux.go serveFaults, Stats.RepeatedFaults/PacedFaults.
4. Tests: internal unit test of the budget with explicit instants; off-Linux classification test with the fake mapping; Lima test: a refaulting client process (new start-refault/stop-refault commands in the Rust test client) beside the hostile fixture's well-behaved process, asserting the repeated faults stay within budget and the neighbour's store latency stays within a stated bound of its latency alone. Mutation: disable pacing and see the Lima test fail.
5. Docs: vm-memory.md hostile-VMM section.
6. Full pager suite in Lima in shared and isolated modes, go test ./..., just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Mechanism: repeated faults (a fault on a page the region already maps for the access) are paced per session: GCRA budget, burst 1,024, then 1,024/s (vmmemory/repeats.go). Classification is MemoryRegion.repeated (bindings.go); the session's queue moved to vmmemory/faultqueue.go, which also marks twins: an access trapped while a fault that changes something was served is free, a twin of a twin or of a repeat is charged. The wait is in the session's own fault worker (connection_linux.go), never ends the session. Stats.RepeatedFaults, Stats.PacedFaults.
Why not a share of service time or fair queuing: wall time charges I/O-bound restores, and fair admission at the I/O permit never engages for CPU-bound faults. Every non-repeated fault is bounded by the resident, dirty and mapping budgets; only repeated faults are unbounded.
FuzzHostileSession: not extended. Its descriptors are not real userfaultfds, so a session ends at its first resolve and cannot repeat a fault.
Validation: unit tests TestRepeatedFaultsArePacedPastTheirBurst, TestAFaultQueueHoldsAPageOnce, TestOnlyAFaultThatChangesSomethingHasAFreeTwin (kills a twin-chain mutation), TestAFaultIsRepeatedExactlyWhenItsPageIsMappedForItsAccess. Lima: TestAVMMRepeatingItsFaultsIsPacedAndItsNeighbourKeepsItsLatency (10/10 runs: neighbour median within 1.14x of alone) and TestThreadsMeetingOnAPageRepeatNoFaultFree; with pacing disabled both fail the budget (47,018 repeats vs 4,790) and the neighbour's median rose 2.3-3.7x. Full Lima pager suite passes in shared and isolated modes; go test ./... and just check pass.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Repeated faults, which change nothing and are the only fault work a VMM can cause without bound, are paced per session: 1,024 at once, then 1,024 a second, waiting in the session's own fault worker. Twins of a fault that changes something are free, and cannot chain. Proven by unit tests off Linux and by Lima tests where a VMM with 64 workers and 128 threads drops its page tables in a loop beside the well-behaved process: the VMM stays within budget and the neighbour's median store stays within 1.5x of its latency alone plus 100 us (measured at most 1.14x); unpaced it was 2.3-3.7x. Documented in docs/vm-memory.md, Repeated faults.
<!-- SECTION:FINAL_SUMMARY:END -->
