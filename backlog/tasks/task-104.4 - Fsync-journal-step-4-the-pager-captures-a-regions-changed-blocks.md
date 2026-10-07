---
id: TASK-104.4
title: 'Fsync journal step 4: the pager captures a region''s changed blocks'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:15'
labels:
  - durability
  - pager
dependencies:
  - TASK-104.1
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 128000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of the plan. Guest stores to a PMEM disk are CPU stores into mapped memory, so the pager is the only place that can say which pages changed since the last flush. It keeps an unjournaled set, write-protects it at each capture, and finds the changed 4 KiB blocks of 2 MiB pages by a SHA-256 digest per block, 16 KiB per page (owner decision 2).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every page the guest can store into without a fault is unjournaled; the check runs after every step of the existing pager campaigns
- [ ] #2 MemoryRegion.Capture write-protects the unjournaled runs one command per run, hashes each block once on the settle workers, and returns the blocks whose SHA-256 differs from the digest held; a protect trap on a page no seal holds marks it unjournaled and copies nothing
- [ ] #3 A page with no digests takes them from its resident origin or the zero block, or is captured whole
- [ ] #4 A seal moves the unjournaled pages to the checkpoint and drops their digests; a capture during the seal covers them from the sealed copies; an abandon makes them unjournaled again without digests; other pages keep or drop their digests as step 1 decided
- [ ] #5 A spilled page is captured from the spill; the Linux userfaultfd test covers the new trap on 2 MiB HugeTLB and 4 KiB pages; guards journal-trap-not-marked, journal-seal-keeps-unjournaled and journal-digests-survive-unjournaled-seal are killed by their tests
<!-- AC:END -->
