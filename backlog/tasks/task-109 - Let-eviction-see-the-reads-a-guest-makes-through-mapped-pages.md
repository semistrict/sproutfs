---
id: TASK-109
title: Let eviction see the reads a guest makes through mapped pages
status: To Do
assignee: []
created_date: '2026-10-08 12:34'
labels:
  - vmmemory
  - eviction
dependencies: []
priority: high
ordinal: 145000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder measured a DAX root disk (ext4 dax=always on a PMEM region) under heavy guest writes: random 4 KiB O_DIRECT reads of a 512 MB file went from 0.01 ms idle to a 5.3 ms p50 while several GB were written. The page queues age only on faults (decision 4 of plans/zircon-pager-port-2026-10-05.md; vmmemory/evict.go), and a DAX read of a mapped page never faults, so pages written new push out the pages the guest reads most. The VMM is ours: a Revoke takes a page out of its mappings, and the next touch faults back to the pager. Revoking the oldest pages of the reclaim queues while keeping their frames, and moving any page that faults back before eviction reaches it to the front with MarkAccessed, gives the queues the accessed signal the port left out, at the cost of a fault per hot page per pass and with no I/O.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A page the guest keeps reading through its mapping is not evicted ahead of pages written once while it was read
- [ ] #2 A revoked page that is touched again is mapped back from its frame with no read from the spill, the cache or the object store
- [ ] #3 A deterministic simulation test reproduces the DAX read-plus-write-churn shape and fails without the change
- [ ] #4 A GCE run of the same shape (8 GiB guest, 80 GiB DAX root, 6.4 GiB PMEM arena, several GB written while a 512 MB file is read at random) reports read p50 and p90 during the writes, before and after
- [ ] #5 docs/vm-memory.md says what the queues' recency is made of
<!-- AC:END -->
