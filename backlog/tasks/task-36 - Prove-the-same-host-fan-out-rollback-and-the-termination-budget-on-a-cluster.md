---
id: TASK-36
title: Prove the same-host fan-out rollback and the termination budget on a cluster
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-26 19:18'
labels:
  - gce
dependencies: []
priority: medium
type: task
ordinal: 36000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Two control-plane changes are unproven on a cluster.** (Templates named by their image's bytes are proven: the soak's redeploys replaced every pod, and the bucket kept one template per image.) The two unproven changes are:
- A same-host fan-out that fails part way takes back the children it started.
- The termination budget is exactly 31 min of preStop plus 60 s of shutdown within the 1920 s grace period, with no slack for the kubelet's own round trips.

Both are in `lifecycle_linux.go`, which needs Linux, hugetlbfs and Firecracker. `scripts/demo-gce.sh redeploy` proves them. Across restarts, the bucket should hold exactly one template per distinct guest image, regardless of pod names. `scripts/demo-gce.sh fixes` runs the rollout end to end.
<!-- SECTION:DESCRIPTION:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26 (docs/measurements/gce-2026-09-26.md). Fan-out rollback: new fix fanout-rollback in scripts/lib/demo-fixes.sh forks a parent with a 16 GiB root 3 times on its host; the orchestrator admits on RAM, the host's PMEM logical cap refuses the second child at receive; the child already started was deleted, the other holds given up, the host served no hold, and the parent checkpointed again. Held in shared and isolated. Termination budget: SPROUTFS_DEMO_ARENA=isolated redeploy with a VM holding a marker written after its last checkpoint; first host pod drained nothing and was gone within 6 s, second drained the VM in 1.04 s and was gone within 2 s, no FailedPreStopHook, the VM kept its marker. The 1920 s grace was not approached; a drain using its full 30 min was not produced. Templates: 7 in the bucket for 7 distinct images (4 alpine builds, 2 runtime imports, workload) across ~15 host pods. Also found: a redeploy rotated the token, which would 401 the second host's drain (fixed 824be482); the first run of this redeploy used the fix.
<!-- SECTION:NOTES:END -->
