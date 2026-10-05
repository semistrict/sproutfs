---
id: TASK-92.3
title: 'Zircon port step 3: the page list'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.1
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 101000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 3 of the plan. Zircon keeps a VMO's pages in a page list whose slots hold a page, a zero marker, a reference to compressed storage, or one end of a zero interval with its own dirty state (zircon/kernel/vm/vm_page_list.cc, vm/include/vm/vm_page_list.h). The pager keeps the same facts in binding blocks, compressed zero runs and sealable runs kept in step by hand. This step ports the page list alone, with its tests; nothing uses it yet.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 vmmemory/internal/zirconvm holds the page list, its slots, zero intervals with their dirty states, cursors and splice lists, ported from vm_page_list.cc and vm_page_list.h with byte offsets and the pager page size in place of kPageSize
- [ ] #2 All 55 cases of vm/unittests/vmpl_unittest.cc run as Go tests in synctest bubbles, each naming its Zircon case in a comment, at a 4 KiB and a 2 MiB page
- [ ] #3 just check passes
<!-- AC:END -->
