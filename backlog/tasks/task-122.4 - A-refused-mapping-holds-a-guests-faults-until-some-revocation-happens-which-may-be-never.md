---
id: TASK-122.4
title: >-
  A refused mapping holds a guest's faults until some revocation happens, which
  may be never
status: To Do
assignee: []
created_date: '2026-10-09 00:25'
updated_date: '2026-10-09 00:48'
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
- [ ] #1 The refusal's errno and command in the reproduction are recorded here
- [ ] #2 A fault refused for its generation or its range is never parked waiting for a revocation
- [ ] #3 A fault refused for the client's mapping budget makes room itself, or ends its session, rather than waiting on other work
- [ ] #4 A campaign seed that refuses a mapping with no other work on the host ends with the guest's fault served or its session failed, not waiting
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Reproduced again 2026-10-09 with refusals logged: every refusal is ENOSPC from the client's VmaBudget (map commands id ~114,000, e.g. page 18 at generation 23045), while the jailed VMM held 326 mappings (/proc/PID/maps). The jail has no /proc, so VmaBudget (rust/sproutfs-vm-memory/src/vma_budget.rs) has no /proc/self/maps to refresh from: its estimate grows by 6 for every mapping command and never falls, and after limit/6 = 87,381 map commands it refuses every map for the life of the session. The pager then parks each fault for a revocation that cannot lower that estimate. Root cause is TASK-122.5; this task keeps the pager's side: a refusal must not be waited on blind.
<!-- SECTION:NOTES:END -->
