---
id: TASK-92.14
title: 'Zircon port step 14: delete the old core'
status: Done
assignee: []
created_date: '2026-10-05 05:10'
updated_date: '2026-10-06 16:42'
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
- [x] #1 The old core files the plan lists under What gets deleted are gone, with Config.Core, SPROUTFS_PAGER_CORE and the second pass of just check; vmmemory/internal/pageranges stays, for the connection (TASK-92.4)
- [x] #2 No test is deleted without the owner's word; any test that only exercised the old core is listed in TASK-92 for the owner first
- [x] #3 docs/vm-memory.md, docs/testing.md and the site and guard tables name the new files, and just check passes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 (coordinator, owner asked to do it directly): every pager call goes to the ported core and the old core's unreachable code is deleted (02f581f8, ~6,700 lines; Linux-only callers restored in 8dbe027f). The switch is gone (0c0f3682): Config.Core, vmmemory.Core, SPROUTFS_PAGER_CORE, internal/testcore, scripts/test-pager-core.py, just check-zircon-core, check-guards' and mutate-gremlins' cores, the GCE script's setting. Test-only leftovers removed next (unused fields, the old prefetch type and host maps). Tests deleted, each because the switch was its subject, and told to the owner in chat: TestAPagerCoreIsNamedAsADeploymentNamesIt, TestEveryPagerRunsTheCoreItsHostNames, TestConfigReadsThePagerCore, TestConfigRefusesAnUnknownPagerCore; TestASessionsMemoryRegionRunsInItsPagersCore became TestASessionsMemoryRegionHasTheRegionLayer. docs/vm-memory.md, docs/testing.md, the plan's status and the talk are updated. Not done: renaming zircon_*.go to plain names and folding the remaining old files the zircon core still calls (fault.go, bindings.go, resident.go, prefetch.go, cold.go, …) into it; internal/pageranges stays for the connection (TASK-92.4); a full just check run and a GCE run of the Linux suites on the single core.

2026-10-06 (one layout, branch worktree-agent-a79d6eaa8f05a2eba): the old core's last statically reachable code went (binding, resident, the old queues, sharing index and checkpoint set, the nil-core fallbacks; staticcheck U1000 on darwin and Linux, with and without sproutfsprobe). zirconHost and zirconRegion merged into Host and MemoryRegion (the region's binding lock is bindingsMu, the window-request lock windowMu); every forwarder took its core's body. Each zircon_X.go joined X.go; frames.go, store.go, peer.go, evict.go (was evictor.go, with the page queues' notes), replacement.go (the store's replacement), settle.go (the settle's comparison), giveback.go; resident.go and queues.go are gone. z prefixes dropped (zframe to frame, zbinding to binding, zplan to plan, zrequest to readRequest, zlent to lentFrame, and the rest); internal/zirconvm unchanged. Comments and docs/vm-memory.md describe one pager. The probe's five tests over the old page now run over a frame and a binding; no test deleted. Two tests renamed: TestThePagersPerPageValuesKeepTheirSizes, TestTheProbeRefusesAnOlderFrameUntilItsBindingIsRetired. AC 1 amended: pageranges stays for the connection, as these notes recorded. Verified: go test ./vmmemory/... ./host/... ./vmmigrate/... ./internal/simtest/... and go test -race ./vmmemory/... pass; just check exits 0, 157 of 157 guard runs killed. Not done here: a GCE rerun of the Linux suites on the one core.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The old core is gone and vmmemory has one layout: Host and MemoryRegion hold the ported core's state, one file per subject, no z prefixes. Verified by the pager, host, migration and simulation suites, the race suite and just check (all guards killed). A GCE rerun of the Linux suites is still to do.
<!-- SECTION:FINAL_SUMMARY:END -->
