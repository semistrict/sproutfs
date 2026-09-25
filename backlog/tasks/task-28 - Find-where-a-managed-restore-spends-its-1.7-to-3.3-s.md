---
id: TASK-28
title: Find where a managed restore spends its 1.7 to 3.3 s
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - measurement
dependencies: []
priority: medium
type: task
ordinal: 28000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**The breakdown of a managed restore's 1.7–3.3 s is now recorded, but still
untimed on a cluster.** A fork's restore used to be recorded as one duration.
So the fan-out's `restore_each_ns` (1.68 s and 3.28 s, against plain
Firecracker's 0.22 s cold restore) did not show which part was the VMM's own
start, the snapshot load, or the pager attaching and populating each memory region.
`vmmachine.StartPhases` records those four phases. `vmmemory.AttachStats`
records what one session's `Connect` cost, including the populate's
commands, runs, pages and duration. The fan-out records all of this per fork
as `restore_phases` (`vmmachine/process_linux.go`,
`vmmemory/connection_linux.go`,
`vmmemory/population.go`). Nothing has run with them yet. The
2026-09-23 record already rules out the mapping commands. 482 of them carried
12,000 runs over 4,089,383 pages for the whole scenario, at about a fifth of a
millisecond each. So the round trips total a tenth of a second, not seconds.
With the populate bounded at 128 runs per memory region, those 12,000 runs are the
faults' windows, not the populate. The pages are the populate's holes, and
one command covers any number of holes.
<!-- SECTION:DESCRIPTION:END -->
