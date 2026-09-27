---
id: TASK-46
title: Fix the two Firecracker tests that fail on x86_64
status: To Do
assignee: []
created_date: '2026-09-26 19:23'
updated_date: '2026-09-27 14:06'
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
<!-- SECTION:NOTES:END -->
