---
id: TASK-104.19
title: Clog and partition the network in the journal campaign
status: To Do
assignee: []
created_date: '2026-10-08 02:34'
updated_date: '2026-10-08 02:36'
labels:
  - durability
  - testing
dependencies: []
references:
  - internal/simtest/journals_campaign_test.go
  - platform/sim/network.go
parent_task_id: TASK-104
priority: medium
type: task
ordinal: 147000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The simulator has Clog, Partition and CorruptNext, but the journal campaign never uses them; only TestAFlushSurvivesItsHostIsolatedAndGivenUp cuts one host off, at one moment. FoundationDB clogs random links throughout and "swizzles" them, clogging a set and unclogging in another order (fdbserver/workloads/RandomClogging.cpp).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Campaign events clog, partition and swizzle links between hosts, the membership, the orchestrator and the object store, for seeded durations
- [ ] #2 A host cut off from the membership answers no flush it cannot journal under its current assignment, and every answered flush survives
- [ ] #3 Every seed ends with all links healed and every guest reading back what it wrote
- [ ] #4 It runs only in the deterministic simulation (platform/sim and internal/simtest): every fault is a seeded choice, a failing run replays from its seed, and nothing touches a real disk, network or cloud
<!-- AC:END -->
