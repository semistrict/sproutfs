---
id: TASK-122.4
title: >-
  A refused mapping holds a guest's faults until some revocation happens, which
  may be never
status: Done
assignee:
  - '@claude'
created_date: '2026-10-09 00:25'
updated_date: '2026-10-09 09:49'
labels: []
dependencies: []
parent_task_id: TASK-122
priority: high
type: bug
ordinal: 160000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Reproduced the embedder's guest freeze on GCE 2026-10-09 (scripts/demo-gce.sh postgres, pulled VM, during pgbench -i): the guest went 154 s without answering. The pager's refused_mappings rose 0 to 11 while its evictions and revocations stopped (flat at 37,330 and 26,279), and the goroutine dump showed 11 fault workers of the PMEM session parked in Connection.deferFault (vmmemory/connection_linux.go) and every other worker idle; the vCPUs sat in kvm_vcpu_block. deferFault waits for the next revocation the pager makes anywhere, on the assumption that the refusal was the client's VMA budget (ENOSPC) and that eviction's revocations free it. But the client (rust/sproutfs-vm-memory, validate and VmaBudget::admit) answers a flagged ACK for ESTALE (a stale generation) and EINVAL too, which no revocation clears, and with no memory pressure no revocation comes even for ENOSPC. The VMM's maps were far below the 524,288-mapping budget. Each refusal is now logged with its errno and command; the next reproduction says which it was.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The refusal's errno and command in the reproduction are recorded here
- [x] #2 A fault refused for its generation or its range is never parked waiting for a revocation
- [x] #3 A fault refused for the client's mapping budget makes room itself, or ends its session, rather than waiting on other work
- [x] #4 A campaign seed that refuses a mapping with no other work on the host ends with the guest's fault served or its session failed, not waiting
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Only ENOSPC is a refusal the pager answers; any other errno in a flagged ACK (ESTALE, EINVAL) is the client rejecting the command, and the session ends on it (commandFrames).
2. On ENOSPC the fault worker makes room itself: harvestOwn takes back every mapping of the region whose page lock is free (harvestPages), then requeues the fault. Nothing harvested: the session ends on the refusal. Remove the host's revocation signal, which nothing waits for any more.
3. Tests: TestTakingBackARegionsMappingsKeepsItsPages (synctest); hostile: nothing to take back ends the session, ESTALE on READY and on a fault ends it; native: a guest's refused access is served after the take-back without an Unseal.
4. Linux tests on GCE via a new scripts/test-vm-memory-gce.sh (Lima is not used).
5. docs/vm-memory.md: the refusal paragraph.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Reproduced again 2026-10-09 with refusals logged: every refusal is ENOSPC from the client's VmaBudget (map commands id ~114,000, e.g. page 18 at generation 23045), while the jailed VMM held 326 mappings (/proc/PID/maps). The jail has no /proc, so VmaBudget (rust/sproutfs-vm-memory/src/vma_budget.rs) has no /proc/self/maps to refresh from: its estimate grows by 6 for every mapping command and never falls, and after limit/6 = 87,381 map commands it refuses every map for the life of the session. The pager then parks each fault for a revocation that cannot lower that estimate. Root cause is TASK-122.5; this task keeps the pager's side: a refusal must not be waited on blind.

Fixed 2026-10-09 (5505ba37). Only ENOSPC is a refusal; any other errno ends the session with 'the client rejected a mapping command'. On ENOSPC the fault worker harvests every page the region maps whose lock is free and serves the fault again; nothing to take back ends the session on the refusal. The host's revocation signal is gone. Verified on GCE with scripts/test-vm-memory-gce.sh (c3-standard-8, 4096 huge pages): TestNativeRefusedMappingLeavesTheSessionServing (the Rust client at MaxVMAs 128 refuses a guest's store; the session took back 62 mappings and the store was served with no Unseal), TestARefusedFaultWithNothingToTakeBackEndsItsSession, the hostile table with ESTALE on READY and on a fault, TestNativeMappingBudgetRejectsBeforeKernelMutation, TestNativeAbandonedCheckpointCoalescesRevokesAcrossGenerationBoundaries; on the Mac TestTakingBackARegionsMappingsKeepsItsPages and the vmmemory suite. AC 4 is met by the Linux hostile test rather than a campaign: the fault workers that park or make room exist only in the Linux transport (connection_linux.go), which the simulation does not run.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A fault the client refused for its mapping budget no longer waits for a revocation that may never come: the session takes back its region's own mappings and serves it again, or ends on the refusal when there is nothing to take back; a refusal with any other errno ends the session. Verified on GCE with the Rust client and the hostile suite, and on the Mac with a synctest of the take-back.
<!-- SECTION:FINAL_SUMMARY:END -->
