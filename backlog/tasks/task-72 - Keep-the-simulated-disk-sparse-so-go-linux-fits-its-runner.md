---
id: TASK-72
title: Keep the simulated disk sparse so go (linux) fits its runner
status: In Progress
assignee: []
created_date: '2026-09-30 00:50'
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
- [ ] #1 Truncating a simulated file past its end stores no bytes for the hole
- [ ] #2 The disk state-machine test crosses device pages and punches holes
- [ ] #3 internal/simtest peaks far below the runner's memory
- [ ] #4 The go (linux) job passes on GitHub
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Store a simulated file as its size and a map of 4 KiB pages, missing pages reading as zeroes; pages immutable so Sync and power loss share them.
2. Widen the disk state-machine test across pages and add punch holes; add a test truncating to 1 TiB.
3. Measure simtest peak RSS locally; confirm on a CI run with a memory monitor; then land on main.
<!-- SECTION:PLAN:END -->
