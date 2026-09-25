---
id: TASK-36
title: Prove the same-host fan-out rollback and the termination budget on a cluster
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
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
