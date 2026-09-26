---
id: TASK-30
title: Make a 4 KiB read-ahead run more than one page under pressure
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
updated_date: '2026-09-26 01:50'
labels:
  - performance
  - deferred
dependencies: []
priority: low
type: enhancement
ordinal: 30000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**A 4 KiB read-ahead run is one page under pressure.** Read-ahead takes only
free arena slots and never evicts. So a guest that scans more memory than the
arena holds takes one fault per page instead of one per run. At 2 MiB that
was one fault per 2 MiB. At 4 KiB it is 512 times as many faults. This is why
the fan-out suite's read phase went from seconds to minutes, and why its
bound had to be rescaled. `forkFanOutRead` is six minutes. That is three
times the slowest reading of the phase on an idle qualification instance
under the probe build, and a third above the slowest reading behind the whole
suite. Letting read-ahead evict, or reserving a run's worth of slots before a
scan, is performance work that this plan did not do
(`vmmemory/window.go`, `reserveRuns`).
<!-- SECTION:DESCRIPTION:END -->
