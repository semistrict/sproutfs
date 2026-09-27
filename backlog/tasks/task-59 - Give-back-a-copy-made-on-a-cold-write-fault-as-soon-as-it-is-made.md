---
id: TASK-59
title: Give back a copy made on a cold write fault as soon as it is made
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-27 18:09'
updated_date: '2026-09-27 19:13'
labels:
  - embedder
dependencies: []
priority: high
ordinal: 66000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
On x86_64 KVM finishes a guest's cold read from its async page-fault worker, which asks for the page writable (virt/kvm/async_pf.c, FOLL_WRITE), so the pager copies a shared page the guest only read. TASK-53's give-back returns such copies, but only once per checkpoint interval and only while the copy and its origin are resident. A guest that reads a lot fills the arena with these copies faster than that: the pager spills them, a spilled copy is never given back or settled, and the next checkpoint uploads them. This is behind both TASK-46 failures (a RAM hog's boot made 1056 evictions; a fork child's root re-published about 52 MB of PMEM its parent only read). A copy made on a write fault to a page the guest did not map is the suspect kind (a write-protect fault is a real store); it should be compared with its origin and given back right away, before it can be spilled, for RAM and PMEM alike.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A copy made on a write fault to an unmapped page is compared with its origin and, when unchanged, given back without waiting for an interval or a checkpoint, with the guest running
- [ ] #2 A copy made on a write-protect fault is never queued
- [ ] #3 It works for RAM and PMEM regions, and never for a fixed region
- [ ] #4 A guest store racing the give-back is never lost (pager test with a real userfaultfd)
- [ ] #5 Pager tests count zero copies left after cold write faults of pages that were only read, and Lima and x86 GCE runs show it
- [ ] #6 The code comment says why it exists
- [ ] #7 Every seal (capture, disk checkpoint, fork point) leaves out each cold copy whose bytes are still its origin's, including a spilled one, so no checkpoint uploads or hands a child an unchanged cold copy
- [ ] #8 A cold copy's origin is evicted only when nothing else can go, which ends the copy being cold
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. A copy a store trap makes of a published page is cold (binding.cold), pinning its origin (resident.coldCopies under Host.pinMu, a leaf lock). Every transition that ends a dirty epoch or changes an origin ends it.
2. Eviction takes a pinned page only in a last pass, when nothing else can go; release of a pinned page, and an isolated move, end or move its cold copies.
3. The seal's walk compares every cold copy of the set it took (reading a spilled one back) and leaves out the unchanged ones, which stay cold and writable (their write-protection lifted).
4. The session worker (200 ms) stays the ordinary path.
5. Not done: resolving cold copies in eviction itself (eviction holds page locks only, so it cannot take the region's locks a give-back needs; the seal comparison covers a spilled copy instead), and making rule copies cold (their pins would double a whole range).
6. Tests: pager unit tests for the seal comparison, a spilled copy and a later store (mutation-checked), a native seal test, full Lima pager + Firecracker suites both arenas, x86 GCE run of TASK-46's two tests and the nested test.
<!-- SECTION:PLAN:END -->
