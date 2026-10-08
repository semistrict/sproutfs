---
id: TASK-104.17
title: Fault the journal campaign while it recovers
status: To Do
assignee: []
created_date: '2026-10-08 02:34'
labels:
  - durability
  - testing
dependencies: []
references:
  - internal/simtest/journals_campaign_test.go
parent_task_id: TASK-104
priority: high
type: task
ordinal: 145000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The journal campaign settles after every fault before the next one, so no fault lands while a survivor reads a lost host's journal, while the membership moves its disk, or while a VM opens with replayed blocks. Those are the paths that run only after something already failed, and the ones most likely to hold a second bug. FoundationDB's MachineAttrition and RandomClogging run for the whole workload, so its kills often land mid-recovery (fdbserver/workloads/MachineAttrition.cpp in ~/src/foundationdb).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A campaign event kills the host that is replaying a lost host's journal partway through, and the VM later opens elsewhere with every answered flush present
- [ ] #2 A campaign event partitions the replaying host from the membership, and detaches or moves the journal disk while it is being read
- [ ] #3 Each new event reaches a probe the campaign asserts, and every seed still verifies every answered flush
<!-- AC:END -->
