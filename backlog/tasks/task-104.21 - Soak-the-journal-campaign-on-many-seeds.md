---
id: TASK-104.21
title: Soak the journal campaign on many seeds
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
  - scripts/soak-pager-gce.sh
parent_task_id: TASK-104
priority: medium
type: task
ordinal: 149000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The journal campaign runs 16 seeds in just check. Its rare interleavings show up only over many seeds, as the pager's soak showed this week: three bugs at about one in seventy runs each. FoundationDB runs a very large number of simulation seeds nightly on a cluster.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A failing seed is printed so that one go test replays it on the Mac
- [ ] #2 docs/testing.md documents the soak and its last result
- [ ] #3 One command runs the simulated journal campaign over thousands of seeds in parallel on one machine
- [ ] #4 It runs only in the deterministic simulation (platform/sim and internal/simtest): every fault is a seeded choice, a failing run replays from its seed, and nothing touches a real disk, network or cloud
<!-- AC:END -->
