---
id: TASK-72
title: Keep the simulated disk sparse so go (linux) fits its runner
status: Done
assignee: []
created_date: '2026-09-30 00:50'
updated_date: '2026-09-30 01:20'
labels: []
dependencies: []
priority: high
type: bug
ordinal: 80000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The go (linux) job of .github/workflows/check.yml has failed on every push to main since 2026-09-26: the hosted runner (4 CPUs, 16 GB) is shut down about 40 s into go test ./..., and just reports signal 15. A monitored run showed simtest.test at 13.5 GB resident beside vmmemory.test before the kill. The simulated disk (platform/sim) held each file as one dense byte slice, and every pager truncates its spill file to the size of all the dirty pages it may hold, so each simulated host held its whole spill in memory. A real filesystem makes that truncate a hole.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Truncating a simulated file past its end stores no bytes for the hole
- [x] #2 The disk state-machine test crosses device pages and punches holes
- [x] #3 internal/simtest peaks far below the runner's memory
- [x] #4 The go (linux) job passes on GitHub
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Store a simulated file as its size and a map of 4 KiB pages, missing pages reading as zeroes; pages immutable so Sync and power loss share them.
2. Widen the disk state-machine test across pages and add punch holes; add a test truncating to 1 TiB.
3. Measure simtest peak RSS locally; confirm on a CI run with a memory monitor; then land on main.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Diagnosed on the runner with a memory monitor (branch claude/ci-diag, since deleted): simtest.test reached 13.5 GB RSS and swap filled before the shutdown. A heap profile put 134 GB of allocation in sim.(*file).Truncate from vmmemory.New's spill truncate. After the fix the monitored runner peaked at 6.4 GB used, no swap. With memory fixed, the second go test step exposed a free-port race in the host harness (bind: address already in use in TestHostMigratesAVMToAnotherHost); the harness now uses internal/testnet names, and testnet reports whether a name still listens.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
platform/sim stores a file as its size and a map of immutable 4 KiB pages, so a truncate past the end is a hole and Sync/power loss share pages. simtest peak RSS fell from 22 GB to 1.3 GB on a Mac. The host harness names page addresses through internal/testnet instead of pre-picking ports. Verified by the widened disk state-machine test, a 1 TiB truncate test, mutation checks, local just check-go, and green check runs on GitHub (branch run 36653391336, main run 36653983253 for c99ddcf3).
<!-- SECTION:FINAL_SUMMARY:END -->
