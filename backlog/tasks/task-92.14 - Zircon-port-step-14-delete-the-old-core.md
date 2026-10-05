---
id: TASK-92.14
title: 'Zircon port step 14: delete the old core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.13
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: medium
type: task
ordinal: 112000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 14 of the plan. Once the new core is the default and measured, the old one and the switch go, so no fix has to land twice. What stays is listed part by part in the plan: the arena, isolation, placement and rules, replacement, pressure and the loss window, the settle, the flush, the fault queue and repeats, the connection, the stats and the probes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The old core files the plan lists under What gets deleted are gone, with vmmemory/internal/pageranges, Config.Core, SPROUTFS_PAGER_CORE and the second pass of just check
- [ ] #2 No test is deleted without the owner's word; any test that only exercised the old core is listed in TASK-92 for the owner first
- [ ] #3 docs/vm-memory.md, docs/testing.md and the site and guard tables name the new files, and just check passes
<!-- AC:END -->
