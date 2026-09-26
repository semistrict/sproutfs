---
id: TASK-11
title: Qualify Firecracker on x86_64
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 19:18'
labels:
  - embedder
  - gce
dependencies: []
priority: high
type: task
ordinal: 11000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The recorded qualification is aarch64 only. Run it on GCE at the end, with the other cluster checks.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The Firecracker qualification suite passes on x86_64 GCE, and the run is recorded in docs/measurements
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26, recorded in docs/measurements/firecracker-x86_64-2026-09-26.md. SPROUTFS_GCE_QUALIFY=1 scripts/bench-memory-gce.sh on n2-standard-8, kernel 7.0.0-1011-gcp, once per arena mode (the script now passes SPROUTFS_ARENA through). Crate clippy and tests, internal/vmtest (incl. the isolated arena's read-only tests, same mapping counts as Lima), vmmemory and pool exhaustion pass in both modes. The Firecracker suite fails 2 of 63 in both modes, so AC #1 is not met. Fixed on the way: missing dd/sync in the x86 root (80d79f35); the jailed starter test hit EACCES because Ubuntu mounts /tmp nodev, and the pull test's 40 MiB fill did not fit the 39 MiB free on the x86 root (eee20f34). Still failing, not diagnosed: (1) TestAGuestTouchingAllItsRAMLeavesItsNeighbourItsWorkingSet: the hostile guest's RAM hog never finishes its first pass in 2 min (328 evictions/spills/312 refaults after boot, no host error); (2) TestPulledGuestsFaultWithoutTheObjectStore: the child's pull of 53781652 bytes took 52502528 more of the disk, i.e. it did not share the parent's copy of the 32 MiB fill. Both pass in Lima on aarch64. Both logged arena:shared in the isolated run, so those tests ignore SPROUTFS_ARENA.
<!-- SECTION:NOTES:END -->
