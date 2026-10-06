---
id: TASK-92.14
title: 'Zircon port step 14: delete the old core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
updated_date: '2026-10-06 15:17'
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
- [x] #2 No test is deleted without the owner's word; any test that only exercised the old core is listed in TASK-92 for the owner first
- [ ] #3 docs/vm-memory.md, docs/testing.md and the site and guard tables name the new files, and just check passes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 (coordinator, owner asked to do it directly): every pager call goes to the ported core and the old core's unreachable code is deleted (02f581f8, ~6,700 lines; Linux-only callers restored in 8dbe027f). The switch is gone (0c0f3682): Config.Core, vmmemory.Core, SPROUTFS_PAGER_CORE, internal/testcore, scripts/test-pager-core.py, just check-zircon-core, check-guards' and mutate-gremlins' cores, the GCE script's setting. Test-only leftovers removed next (unused fields, the old prefetch type and host maps). Tests deleted, each because the switch was its subject, and told to the owner in chat: TestAPagerCoreIsNamedAsADeploymentNamesIt, TestEveryPagerRunsTheCoreItsHostNames, TestConfigReadsThePagerCore, TestConfigRefusesAnUnknownPagerCore; TestASessionsMemoryRegionRunsInItsPagersCore became TestASessionsMemoryRegionHasTheRegionLayer. docs/vm-memory.md, docs/testing.md, the plan's status and the talk are updated. Not done: renaming zircon_*.go to plain names and folding the remaining old files the zircon core still calls (fault.go, bindings.go, resident.go, prefetch.go, cold.go, …) into it; internal/pageranges stays for the connection (TASK-92.4); a full just check run and a GCE run of the Linux suites on the single core.
<!-- SECTION:NOTES:END -->
