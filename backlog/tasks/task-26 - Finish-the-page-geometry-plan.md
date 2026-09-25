---
id: TASK-26
title: Finish the page-geometry plan
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - measurement
dependencies: []
priority: medium
type: task
ordinal: 26000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**RAM runs 2 MiB by default again; 4 KiB is measured and optional.** The
measurements of 2026-09-23 are in docs/measurements. 2 MiB is faster at every
timing, and it costs about the same memory once forks do real work. 4 KiB
holds a tenth of the memory, but only for sparse writers. The rest of this
item is the history of the 4 KiB work. The store, the pager, the wire and the
VMM all carry each memory region's own page size now. The
[page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md) still has
steps 5 and 7 left (handoff and migration geometry, and the qualification).
It also still lacks the realistic workload measurements of retained sharing
and runtime that the whole change is meant to justify. Nothing here has been
run against the recorded workload at 4 KiB. Step 6's request counts are
measured, but only in the simulation: a cold 2 MiB read-ahead run of 512 RAM
pages takes two object-store requests where it took 513. What that saves
against the 169 s that a GCE restore of a 16 GiB guest took on 2026-09-21 is
unmeasured on a real store. The fan-out on 2026-09-22 showed why a restore's
numbers did not apply to forks. A restore's windows are whole, and a fork's
are not: 8,660 loads brought 31,867 pages, 3.7 pages per load, against 16 on
the same run's cold restore. The cause was that the pager split its window at
every page it already held. That is fixed, and a unit test counts it (a
512-page window over three checkpoints with 64 pages resident: 65 loads and
195 GETs, against one load and three GETs). The GCE re-measurement on
2026-09-22 took a cold restore from 7.5 s to 4.6 s, and its loads from 1,323
to 293. The fan-out did not change, because a fork does not fault the way a
restore does: 20,016 of its 21,130 faults were stores, and a store read only
its own page. A store now reads its window ahead too, counted the same way.
The fan-out has not been re-run, so as far as this repository knows, first
output within 2 s is still unmet.
<!-- SECTION:DESCRIPTION:END -->
