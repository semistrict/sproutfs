---
id: TASK-92.4
title: 'Zircon port step 4: the bindings on the page list'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.3
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 102000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of the plan. The first in-place swap: the binding blocks and the compressed zero runs (vmmemory/bindings.go, vmmemory/internal/pageranges) become the ported page list, and the runs a seal protects become its dirty runs. The fields Zircon has no place for (mapped, the checkpoint copy a page shares, its origin, cold, written ahead) stay in a binding beside the slot. The existing pager suite is the check, unchanged.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A memory region keeps its per-page state in the ported page list; vmmemory/internal/pageranges is no longer used by vmmemory
- [ ] #2 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged, in both arena modes, and every guard in scripts/mutation/guards.json is still killed
- [ ] #3 vmmemory/sparse_metadata_test.go still holds the metadata bound, and a region that touches one page in 512 holds no more metadata than before, measured by a test
- [ ] #4 A seal still issues one protect command per run of dirty pages, shown by the existing seal tests
<!-- AC:END -->
