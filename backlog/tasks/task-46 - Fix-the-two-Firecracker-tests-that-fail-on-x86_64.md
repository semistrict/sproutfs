---
id: TASK-46
title: Fix the two Firecracker tests that fail on x86_64
status: To Do
assignee: []
created_date: '2026-09-26 19:23'
updated_date: '2026-09-27 23:13'
labels:
  - embedder
dependencies:
  - TASK-11
priority: high
ordinal: 53000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The GCE run of 2026-09-26 (docs/measurements/firecracker-x86_64-2026-09-26.md) ran the Firecracker qualification suite on x86_64. 61 of 63 pass in both arena modes. Two fail on x86_64 and pass on Lima aarch64, undiagnosed: TestAGuestTouchingAllItsRAMLeavesItsNeighbourItsWorkingSet (the RAM hog never finishes its first pass within 2 min, no host error), and TestPulledGuestsFaultWithoutTheObjectStore (the child's pull of 53781652 bytes took 52502528 more disk, so it did not share the parent's copy). Both logged arena:shared in the isolated run, so they ignore SPROUTFS_ARENA. TASK-11 closes when the suite passes on x86_64.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Both tests pass on x86_64 GCE in both arena modes, or a product defect they found is fixed
- [ ] #2 Both tests honour SPROUTFS_ARENA
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE x86 rerun 2026-09-27: both still fail. Both look like the x86 async page fault behaviour (a cold read reaches the pager as a write). Pull test: the parent's reads of its DAX file after its reopen made private copies (checkpoint shows 23 dirty pages, 48 MB), so the fork child's root published ~52 MB of them again as its own instead of inheriting the parent's copy, and its pull took them again. The child's root settled nothing (unchanged_pages 0), although the fork design says the child's first checkpoint settles what it inherited unchanged. RAM hog test: the hostile guest's boot alone made 1056 evictions and spills on a 3/4-size arena, and the hog never finished its first pass in 2 minutes: reads turned into dirty copies that must spill. Next: confirm with the store-trap counters from TASK-49, and make the fork child's root settle inherited unchanged pages.

GCE x86 2026-09-27, after TASK-59's cold copies (cold state, seal leaves unchanged cold copies out, eviction give-back after 200 ms, volume comparison when a pinned origin must go): TestPulledGuestsFaultWithoutTheObjectStore PASSES in both arenas. TestAGuestTouchingAllItsRAMLeavesItsNeighbourItsWorkingSet still fails, and it is not reads-as-writes: after boot both guests' RAM is wholly dirty (dirty 64 of 64), and the hog's RAM pager made 100 copies, none cold. In its 120 s the RAM pager served 4228 faults (4131 evictions/spills, 4079 refaults) in 9.7 s of fault time, max 4.5 ms per fault, fault-queue max 3 ms: the guest spent ~110 s of the 120 s elsewhere. Next: where a guest waits outside the pager on x86 (async page fault completion, KVM EPT at 4 KiB against 2 MiB pages, the VMM), e.g. kvm tracepoints kvm_async_pf_* and kvm_page_fault during the hog.

2026-09-27 GCE: the hog of 80 MiB exceeded the x86 guest's MemAvailable (~74 MiB of 128) and stalled in guest reclaim after 64 MiB (it wrote 64 MiB in 139 ms). Fixed by sizing it from MemAvailable (hog ram 0, less 8 MiB). Now the first pass completes in both arenas and the calm working set faults back in in 281/381 ms, but calm's agent exec does not answer within 30 s: 34903 RAM faults, 34549 evictions, fault time 89.6 s (max 5 ms). A booted x86 guest has 32 pages (64 MiB) resident, above the 24-page share the test assumes. Rerunning with per-guest residency and the KVM trace.
<!-- SECTION:NOTES:END -->
