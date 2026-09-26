---
id: TASK-34
title: Fix ten workload guests timing out on one boot disk
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-26 19:18'
labels:
  - gce
dependencies: []
priority: medium
type: bug
ordinal: 34000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Ten workload guests on one boot disk time out.** With `FORKS_BASE=4 FORKS_PER_REPO=2`, two guests exceeded the orchestrator's ten-minute exec limit, and the host logs showed nothing. Both host pods spill to the node's single pd-balanced disk. Three-and-two completed.
<!-- SECTION:DESCRIPTION:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26 (docs/measurements/gce-2026-09-26.md): FORKS_BASE=4 FORKS_PER_REPO=2 scripts/demo-gce.sh workload no longer reaches the disk. The orchestrator refuses the first fork: 'no host is available: sproutfs-host-… has 1879048192 bytes of memory free, and this VM needs 8589934592'. The default 2/1 is refused the same way (needs 4294967296). A fork now admits each child as a guest of its own RAM; a workload guest is 2 GiB and a host's RAM arena 3.75 GiB, so a host holds one. The ten-guest disk saturation cannot be reproduced on this cluster until the demo's sizing or the fork admission changes.
<!-- SECTION:NOTES:END -->
