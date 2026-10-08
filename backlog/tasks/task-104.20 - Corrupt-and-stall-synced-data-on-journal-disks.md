---
id: TASK-104.20
title: Corrupt and stall synced data on journal disks
status: To Do
assignee: []
created_date: '2026-10-08 02:34'
labels:
  - durability
  - testing
dependencies: []
references:
  - journal/journal.go
parent_task_id: TASK-104
priority: medium
type: task
ordinal: 148000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The sim disk damages only writes made since the last sync, at a power loss. Nothing flips bits in an entry that was synced, or stalls a journal disk for long, so nothing shows that a recovery reading a damaged entry refuses it rather than writing its bytes into a VM's disk. FoundationDB's DiskFailureInjection stalls disks and flips bits in reads (fdbserver/workloads/DiskFailureInjection.cpp).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A seeded site flips bits in synced journal entries and in the header, and a recovery reading one writes none of its bytes and reports the corruption
- [ ] #2 A seeded site stalls a journal disk for longer than a batch interval, and flushes wait or fail with EIO, never answering early
- [ ] #3 docs/architecture.md says what a corrupted journal entry costs the VM
<!-- AC:END -->
