---
id: TASK-104.2
title: 'Fsync journal step 2: the journal format and its writer'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
labels:
  - durability
  - storage
dependencies: []
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 126000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 2 of the plan. A host needs a write-ahead journal on a network disk that it can append flushed blocks to, read back after a crash, serve to other hosts, and trim. This step builds it as a new package over platform.File, with no caller yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Package journal writes and reads the format in the plan: two header slots with lease and tail hint, entries with XXH3-128, a ring with pads, positions that only grow
- [ ] #2 A group-commit writer keeps one batch in flight and answers its flushes only after the batch syncs, in position order
- [ ] #3 Over platform/sim disks with PowerLossFaults, every entry answered before a power loss reads back, a torn batch ends the read, and a failed write range is padded so nothing after it is lost
- [ ] #4 A lease of a newer assignment refuses the disk; trimming by covered positions frees the ring
- [ ] #5 Format fixtures under journal/testdata, a fuzz test of the parser, and a sim.Bug guard journal-answer-before-sync killed by its tests
<!-- AC:END -->
