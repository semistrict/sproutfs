---
id: TASK-106
title: 'One disk''s flushes never overlap, with durable flush on or off'
status: To Do
assignee: []
created_date: '2026-10-07 18:56'
labels:
  - flush
  - performance
dependencies: []
references:
  - host/flush.go
  - docs/measurements
priority: medium
type: bug
ordinal: 142000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found in the step 9 GCE run. A guest whose 1, 8 or 64 threads each write 4 KiB and fdatasync got the same total rate, about 7,000 flushes a second with durable flush off and about 330 with it on. Latency grew with the thread count. It happens with the journal off too, so the limit is below the journal: in the guest's block device, the VMM or the host's flush path. Where is not known.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Where one disk's flushes serialize is found and written down
- [ ] #2 Either flushes of one disk overlap and the rate grows with threads on GCE, or the docs say why they cannot
<!-- AC:END -->
