---
id: TASK-92.10
title: 'Zircon port step 10: one switch for the core'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 10:56'
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
- [x] #1 vmmemory.Config.Core and SPROUTFS_PAGER_CORE exist, current is the default, and the host logs which core each pager runs
- [x] #2 Every test passes unchanged under the default, and a configuration naming an unknown core is refused with ErrConfig
- [x] #3 just check has a pass that runs the tests named in one list under SPROUTFS_PAGER_CORE=zircon; the list starts empty
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. vmmemory.Core (current, the zero value and default; zircon) with String and ParseCore, Config.Core, and New refusing an unknown core with ErrConfig.
2. Host.zircon and MemoryRegion.zircon: every exported method of Host and MemoryRegion that touches the page layer dispatches on the core; the zircon core refuses each with ErrCoreUnsupported naming the operation. Accessors that read only configuration stay shared.
3. SPROUTFS_PAGER_CORE in cmd/sproutfs-host (SupervisorConfig.PagerCore, refused when unknown), passed to every pager, and the pager's core in the line the supervisor logs when it assembles a pager.
4. internal/testcore, as internal/testarena: the core a suite builds its pagers in, from SPROUTFS_PAGER_CORE. Fixtures of vmmemory, host, vmmigrate, internal/simtest and vmmachine set it.
5. scripts/pager-core-zircon.json (starts empty) and scripts/test-pager-core.py, run by just check: each listed test under SPROUTFS_PAGER_CORE=zircon in both arena modes, failing on a listed test that does not exist or does not pass.
6. scripts/check-guards.py: an entry's optional cores list runs its guard under each core.
7. Tests: the switch (default, zircon refusal, unknown core refused, ParseCore), host config parse and refusal; docs (vm-memory.md, testing.md, deploy/README.md).
8. just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Built: vmmemory.Core (core.go; current is the zero value, zircon), Config.Core refused by New with ErrConfig when unknown, Host.zircon and MemoryRegion.zircon (zircon.go). Every exported method of Host and MemoryRegion that reaches the page layer asks the core first: Attach, DropIdle, Stats, Sharing, SettlePrefetches, Fault, Populate, Seal, Unseal, Verify, Detach, GiveBackColdCopies, SetUnpublishedAge, OldestUnpublished, ReadResident, Resident, Handoff, Unpublished, MemoryRegion.Stats and SettlePrefetches. The zircon core refuses each with ErrCoreUnsupported naming the operation. Accessors of configuration (PageSize, Kind, Resources, LogicalHeadroom, OnInterval, Ephemeral, HoldToNoWindow, Populated, GuestFaults), the flush (stays ours), SetPressure/SetFlushed and Close are shared.
SPROUTFS_PAGER_CORE: cmd/sproutfs-host reads it into SupervisorConfig.PagerCore (refused when unknown), host/pager.go gives it to every pager, and the line the supervisor logs for each pager names its core (assembled, host/pager.go).
internal/testcore is the core a suite builds its pagers in; the fixtures of vmmemory, host, vmmigrate, internal/simtest and vmmachine set it.
scripts/pager-core-zircon.json (empty) and scripts/test-pager-core.py run by just check-zircon-core (in just check and CI's go jobs): every listed test under SPROUTFS_PAGER_CORE=zircon in both arena modes; a listed test that fails or does not exist fails it (checked by hand with a missing name).
scripts/check-guards.py: an entry's cores (default current) runs its guard under each core.
Tests: TestAPagerCoreIsNamedAsADeploymentNamesIt, TestAPagerRefusesACoreItDoesNotHave, TestTheZirconCoreRefusesWhatItDoesNotServeYet (vmmemory), TestEveryPagerRunsTheCoreItsHostNames (host), TestConfigReadsThePagerCore, TestConfigRefusesAnUnknownPagerCore (cmd/sproutfs-host).
Validation: just check exit 0 (155 of 155 guard runs killed, no test listed for the zircon core).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added the pager core switch: vmmemory.Config.Core (current by default, or zircon) and the host's SPROUTFS_PAGER_CORE, with every page-layer method of Host and MemoryRegion asking the core first and the zircon core refusing each operation by name with ErrCoreUnsupported. The supervisor logs each pager's core. internal/testcore sets the suites' core, scripts/pager-core-zircon.json (empty) is run under the zircon core in both arena modes by just check-zircon-core, and check-guards can check a guard under both cores. Verified by the switch, host and config tests and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
