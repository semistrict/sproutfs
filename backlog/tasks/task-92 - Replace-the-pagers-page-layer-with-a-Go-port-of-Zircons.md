---
id: TASK-92
title: Replace the pager's page layer with a Go port of Zircon's
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
dependencies: []
references:
  - plans/zircon-pager-port-2026-10-05.md
documentation:
  - docs/vm-memory.md
priority: high
type: task
ordinal: 98000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The owner decided on 2026-10-05 to replace the page layer of vmmemory with a Go port of the page layer of Zircon's VM (fuchsia zircon/kernel/vm, C++, MIT), copied rather than redesigned. Zircon's dirty states and writeback (Clean, Dirty, AwaitingClean) are our seal; its eviction splits pages a pager backs from anonymous pages as ours splits named pages from the overlay; it brings 261 unit tests; and it is built for a pager outside the kernel, which ours is. The plan maps every part of vmmemory to its Zircon source or says why it stays ours, records five departures from Zircon (D1 to D5), lists which Zircon tests port, and orders the work in fourteen steps, one subtask each. Open decisions for the owner are at the end of the plan.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Every step of plans/zircon-pager-port-2026-10-05.md is done, each landing on main with just check passing
- [ ] #2 The owner has answered the decisions at the end of the plan, and the answers are recorded in this task
- [ ] #3 The pager runs on the ported page layer by default, the old core is deleted, and docs/vm-memory.md and docs/testing.md describe the new layout
<!-- AC:END -->
