---
id: TASK-31
title: Measure keeping RAM mappings whole on a cluster
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - measurement
  - gce
dependencies: []
priority: low
type: task
ordinal: 31000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**RAM's mappings are kept whole, and the benefit is unmeasured on a
cluster.** The step of the
[page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md) for this
is done:
- The arena's offsets are not its pages, and its memfd is sized to the
  offsets.
- Every 2 MiB-aligned range that holds a private page owns an extent, and a
  private page sits at its own offset within its range.
- A store closes a gap of at most sixteen pages.
- A range that reaches half its pages is filled.
- The mapping budget is the backstop, and `Stats.MappingMerges` reports when
  it acted.

The mapping protocol went to version 8 for this change, because ATTACH's
length is now the offset space instead of the capacity. What remains open is
the benefit on a real workload. The counts are proved in `vmmemory`
and in the simulation. One Lima reading of the fork fan-out at 4 KiB shows a
child's VMM holding 3,299 and 3,288 mappings, down from 4,485 and 4,631. It
also shows 28 private extents, 2,723 pages copied by the rules, and the
backstop never acting. But that is one run of each, on an instance whose load
differed between the two runs. Nothing has been run against the recorded
workload, or on GCE with the rules on and off.
<!-- SECTION:DESCRIPTION:END -->
