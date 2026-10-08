---
id: TASK-104.18
title: Take several hosts down in one step of the journal campaign
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
parent_task_id: TASK-104
priority: medium
type: task
ordinal: 146000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The campaign kills one host per step, and only while more than two are up, so it never sees two journals to read back at once, nor a host that loses its own disk. FoundationDB kills up to its fault tolerance at once and also reboots machines with their data deleted (KillType RebootAndDelete, fdbrpc/include/fdbrpc/SimulatorKillType.h).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A step can kill two hosts at once, crash or power loss each, and every VM they ran opens on the survivor with every answered flush present
- [ ] #2 A host can come back with its local disk wiped, and opens no VM from what it lost
- [ ] #3 The campaign runs with at least four hosts so a double kill leaves a host to read both journals
- [ ] #4 It runs only in the deterministic simulation (platform/sim and internal/simtest): every fault is a seeded choice, a failing run replays from its seed, and nothing touches a real disk, network or cloud
<!-- AC:END -->
