---
id: TASK-92.10
title: 'Zircon port step 10: one switch for the core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.9
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 108000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 10 of the plan. The fault and checkpoint core is the one part that cannot be swapped in place, so it runs beside the old core until it has been measured. This step adds the switch and nothing else: vmmemory.Config.Core and SPROUTFS_PAGER_CORE, current or zircon, with every exported method of Host and MemoryRegion dispatching on it, as SPROUTFS_ARENA did for the isolated arena.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 vmmemory.Config.Core and SPROUTFS_PAGER_CORE exist, current is the default, and the host logs which core each pager runs
- [ ] #2 Every test passes unchanged under the default, and a configuration naming an unknown core is refused with ErrConfig
- [ ] #3 just check has a pass that runs the tests named in one list under SPROUTFS_PAGER_CORE=zircon; the list starts empty
<!-- AC:END -->
