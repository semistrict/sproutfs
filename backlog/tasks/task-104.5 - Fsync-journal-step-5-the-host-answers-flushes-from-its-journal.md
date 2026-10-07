---
id: TASK-104.5
title: 'Fsync journal step 5: the host answers flushes from its journal'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - host
dependencies:
  - TASK-104.2
  - TASK-104.3
  - TASK-104.4
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 129000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 5 of the plan. Durable flush is an optional mode, SPROUTFS_DURABLE_FLUSH, off by default (owner decision 3). Off, the flush bound works as today. On, a flush waits for its entry on the journal disk, and a flush that cannot be journaled fails with EIO; there is no fallback to a checkpoint. The host opens its journal disk like a shard, captures and batches flushes, and records the covered position at each seal.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 With the mode off, the flush path and its tests are unchanged and no journal disk is opened
- [ ] #2 With the mode on, a flush is answered only after the batch holding its entry has synced; a flush that cannot be journaled (no journal served, a failed write or sync, a detached disk) fails with EIO, and the next batch pads the failed range
- [ ] #3 A full ring is back-pressure: past three quarters the host asks for checkpoints out of turn, a VM over half the ring waits for its own, and a full ring holds captures until trimming frees space
- [ ] #4 The seal records the VM last captured position and the selection writes it as the covered position
- [ ] #5 JOURNAL_READ is served by the disk holder, which first fences the VM at the reader epoch and gives the VM up if it runs it
- [ ] #6 Status and metrics report durable_flush (whether the mode is on), flush latency, hashing time, live bytes and failed flushes; guards journal-covered-after-seal, journal-read-without-fence, journal-full-answers and journal-failed-write-answers are killed by their tests
<!-- AC:END -->
