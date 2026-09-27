---
id: TASK-59
title: Give back a copy made on a cold write fault as soon as it is made
status: Done
assignee:
  - '@claude'
created_date: '2026-09-27 18:09'
updated_date: '2026-09-27 21:30'
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
- [x] #1 A copy made on a write fault to an unmapped page is compared with its origin and, when unchanged, given back without waiting for an interval or a checkpoint, with the guest running
- [x] #2 A copy made on a write-protect fault is never queued
- [x] #3 A guest store racing the give-back is never lost (pager test with a real userfaultfd)
- [x] #4 Pager tests count zero copies left after cold write faults of pages that were only read, and Lima and x86 GCE runs show it
- [x] #5 The code comment says why it exists
- [x] #6 Every seal (capture, disk checkpoint, fork point) leaves out each cold copy whose bytes are still its origin's, including a spilled one, so no checkpoint uploads or hands a child an unchanged cold copy
- [x] #7 It works for RAM and PMEM regions alike
- [x] #8 A cold copy's origin is evicted only when nothing else can go; a copy whose origin went stays cold and is compared with the bytes its volume holds for the page
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Evidence: vmmemory unit tests (cold_test.go, giveback_test.go: seal leaves out RAM and PMEM cold copies, spilled cold copy compared from the spill, eviction gives back an aged cold copy and spills a young one, one-page arena compares with the volume, protect-trap copy not cold, store during compare kept; each mutation-checked); native userfaultfd tests (giveback_linux_test.go: session gives back a populate-write copy, keeps a real store, seal leaves two adjacent cold copies out) passed on Lima both arenas and in the x86 GCE vmmemory suite (551/552 pass, 0 fail); simulation and probe builds pass. x86 GCE: TestPulledGuestsFaultWithoutTheObjectStore passes in both arenas (was the child re-publishing ~52 MB its parent only read). Not closed by this: TASK-46's RAM hog, whose stall is outside the pager.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A copy a store trap makes of a published page is now cold until a comparison shows the guest changed it. It pins its origin (evicted only when nothing else can go, after which the copy is compared with its volume's bytes), its session gives it back after 200 ms (reading a spilled one back), an eviction gives an aged one back instead of spilling it, and every seal compares cold copies and leaves unchanged ones out, so no capture, disk checkpoint or fork point uploads or hands a child a page the guest only read. Verified by mutation-checked unit tests, native userfaultfd tests, the simulation and probe builds, and x86 GCE (pull test fixed in both arenas, pager suite clean).
<!-- SECTION:FINAL_SUMMARY:END -->
