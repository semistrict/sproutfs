---
id: TASK-104.9
title: 'Fsync journal step 9: measure flush latency and its cost on GCE'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
updated_date: '2026-10-07 01:16'
labels:
  - durability
  - measurement
dependencies:
  - TASK-104.8
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 133000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 9 of the plan, and the evidence for TASK-104 criterion 4. The latency estimates in the plan come from fio runs that measured neither a write with a sync at one in flight nor a guest. This run measures them and sets the journal disk default kind and size.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 fio on the raw journal disk measures 4 KiB to 64 KiB writes with a sync at one in flight, on Hyperdisk Balanced and on pd-ssd, p50, p99 and p99.9
- [ ] #2 In a guest, flush latency is measured at 1, 8 and 64 threads that write 4 KiB and flush, with the durable flush mode on and off
- [ ] #3 The throughput cost is reported: guest write rate with and without flushes, protect traps a second, SHA-256 hashing time, journal bytes per guest byte and live bytes over an interval
- [ ] #4 A host killed after a flush is timed until its VM runs again, split into the disk move, the replay and the publication; a scale-up and a scale-down time the create, attach and detach calls and the wait for the journal to empty
- [ ] #5 docs/measurements/gce-fsync-journal-<date>.md reports it; every VM, disk and object is deleted and the script checks none remain
<!-- AC:END -->
