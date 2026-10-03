---
id: TASK-85
title: >-
  Fix the cache's erasure code per deployment and read each stripe by the code
  it was stored under
status: To Do
assignee: []
created_date: '2026-10-03 22:22'
updated_date: '2026-10-03 23:10'
labels:
  - cluster
  - performance
dependencies: []
references:
  - plans/disk-cache-2026-10-02.md
  - docs/properties/a-page-survives-losing-a-host.md
priority: high
type: feature
ordinal: 92000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Today, with no SPROUTFS_CACHE_CODE set, the orchestrator picks the table's code (1+0, 1+1, 2+1, 2+2, 4+2) for the most caches it has listed since it started, and a reader treats a stripe of another code as a miss. So crossing a size threshold, or an orchestrator restart that picks a smaller code, turns every cached window into a miss at once. Decided 2026-10-03 by the owner: the code is a deployment setting that never follows the number of hosts or shards, and every stripe is read and rebuilt by the code it was stored under, so a deliberate code change leaves earlier windows readable until they age out instead of emptying the cache. Narrowed on 2026-10-03: handing stripes over on drain and reading previous holders after a join were dropped, because TASK-86 moves the cache onto network-disk shards whose placement does not change when compute scales.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The code is a deployment setting that never follows the number of hosts or shards; with none set the deployment uses one fixed default documented in docs/hosting.md
- [ ] #2 A stripe is read and rebuilt by the code it was stored under; a test changes the code and reads every earlier window without a store read
- [ ] #3 Tests follow repo practice: synctest over platform/sim, sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code; spec/diskcache models a code change, every TLC run within a couple of minutes
<!-- AC:END -->
