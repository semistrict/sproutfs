---
id: TASK-34
title: Fix ten workload guests timing out on one boot disk
status: Done
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-28 04:40'
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

Closed by the owner on 2026-09-28 as superseded. The timeouts came from ten 2 GiB guests on hosts whose RAM arenas hold fewer, spilling to the node's one disk. Fork admission (each child admitted as a guest of its own RAM) now refuses that overcommit before any guest starts, so the setup cannot be reached; reproducing it would need an overcommit the deployment no longer allows.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Superseded: fork admission refuses the memory overcommit that made ten guests spill to one disk, so the timeouts can no longer arise as described.
<!-- SECTION:FINAL_SUMMARY:END -->
