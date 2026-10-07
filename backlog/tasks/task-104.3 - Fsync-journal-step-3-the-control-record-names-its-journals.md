---
id: TASK-104.3
title: 'Fsync journal step 3: the control record names its journals'
status: To Do
assignee: []
created_date: '2026-10-07 00:48'
labels:
  - durability
  - metadata
dependencies: []
references:
  - plans/fsync-journal-2026-10-06.md
parent_task_id: TASK-104
priority: high
type: task
ordinal: 127000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of the plan. A recovery must know which journals may hold entries after the selected checkpoint, and from which position. The control record is the authority, and it is written at every selection and open already, so it carries the list at no extra write.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Control record format 6 carries a list of journals, each naming a disk identity, its generation, the epoch and the covered position; format 5 is refused by name
- [ ] #2 Select and SelectKept write the list; an open keeps it; a migration open adds its own journal; a stop and a close write an empty list
- [ ] #3 Record tests and format fixtures cover each change; spec/ownership is updated if its state changes
<!-- AC:END -->
