---
id: TASK-88
title: 'Measure how long a start, a restore and a fork take until the guest runs'
status: To Do
assignee: []
created_date: '2026-10-04 19:21'
labels:
  - measurement
dependencies: []
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
priority: medium
type: task
ordinal: 95000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
No report records the time from a start, a restore or a fork request to the guest's first instruction, or where that time goes. A start and a fork write to the object store on their path (the control record's create and the epoch's advance), which on GCS or S3 is tens of milliseconds; Netlify's microVMs cold start in about 9 ms with no remote write. Before deciding whether a VM that is never recorded or published is worth adding, we need the numbers.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A dated report in docs/measurements gives p50/p99 from request to first guest instruction for a cold start, a restore from a checkpoint, a fork on the same host and a fork to another host, on GCE
- [ ] #2 The report splits each into its steps (control record writes, root read, VMM restore, first faults) and names the largest
- [ ] #3 The report says what a fork that writes nothing to the store would save, and the backlog has a task for it if the saving is worth it
<!-- AC:END -->
