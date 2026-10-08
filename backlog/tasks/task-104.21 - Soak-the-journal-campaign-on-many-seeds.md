---
id: TASK-104.21
title: Soak the journal campaign on many seeds
status: To Do
assignee: []
created_date: '2026-10-08 02:34'
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
- [ ] #1 One command runs the journal campaign on thousands of seeds across a disposable GCE VM and deletes the VM whatever happens
- [ ] #2 A failing seed is printed so that one go test replays it on the Mac
- [ ] #3 docs/testing.md documents the soak and its last result
<!-- AC:END -->
