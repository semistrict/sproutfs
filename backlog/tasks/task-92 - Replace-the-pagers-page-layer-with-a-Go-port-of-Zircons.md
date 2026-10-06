---
id: TASK-92
title: Replace the pager's page layer with a Go port of Zircon's
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
updated_date: '2026-10-06 14:22'
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Owner decisions, taken by the coordinator overnight 2026-10-05 on the plan's recommendations (owner asleep, asked to proceed independently): D1 split on store — accepted; D2 spill dirty pages of named volumes — accepted; byte offsets with the pager's page size — accepted; aging driven by faults only — accepted; switch-over gate: the runs in step 13, no median fault slower than the spread of two runs of the old core — accepted; test names in repo sentence style with the Zircon name in a comment — accepted. Owner to review in the morning.

2026-10-06: the owner decided to delete the old core and keep only the zircon core now, waiving the step 13 speed gate (zircon random 4 KiB fault ~3.6–5% slower than before step 12 after 251a94f8; forward faults and the walk faster). Steps 13 and 14 proceed without the GCE switch-over measurements.
<!-- SECTION:NOTES:END -->
