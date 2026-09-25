---
id: TASK-2.2
title: Make the pager arena a set of files
status: To Do
assignee: []
created_date: '2026-09-25 23:02'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: task
ordinal: 38000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 2 of plans/isolated-arena-2026-09-25.md, behind vmmemory.Config.Arena (ArenaShared|ArenaIsolated) and SPROUTFS_ARENA=shared|isolated. A resident page's slot becomes a file and a slot; still one file in both modes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The arena interface makes files; the Linux and simulated arenas follow
- [ ] #2 Every existing suite passes unchanged, in the simulation, the Linux pager suite and the Firecracker suite in Lima
- [ ] #3 The switch exists end to end and shared is the default
<!-- AC:END -->
