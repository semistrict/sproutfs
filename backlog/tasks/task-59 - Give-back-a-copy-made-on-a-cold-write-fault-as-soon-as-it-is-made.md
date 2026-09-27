---
id: TASK-59
title: Give back a copy made on a cold write fault as soon as it is made
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-27 18:09'
updated_date: '2026-09-27 18:09'
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
<!-- AC:END -->
