---
id: TASK-85
title: >-
  Keep the SSD cache through autoscaling: a fixed code, stripes read by their
  own code, and stripes handed over on drain
status: To Do
assignee: []
created_date: '2026-10-03 22:22'
labels:
  - cluster
  - performance
dependencies:
  - TASK-83
references:
  - plans/disk-cache-2026-10-02.md
  - docs/properties/a-page-survives-losing-a-host.md
priority: high
type: feature
ordinal: 92000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An autoscaler adds and removes hosts through the day. Today three things make that cost the hosts' SSD cache (TASK-81) much of what it holds. (1) The code follows the host count: with no SPROUTFS_CACHE_CODE set, the orchestrator picks the table's code for the most caches it has listed since it started, and a reader treats a stripe of another code as a miss, so crossing a threshold (for example 5 to 6 hosts, 2+2 to 4+2) turns every window into a miss at once, and an orchestrator restart can pick a smaller code. (2) A join pushes one holder out of the first k+m for about (k+m)/(N+1) of windows, and that stripe is no longer asked for; repair restores it only when the window is read, so several joins without reads push cold windows below k reachable stripes. (3) A host the autoscaler removes takes its stripes with it; under 4+2 on 10 hosts, three removals before repair lose about 17 % of windows. Decided 2026-10-03 by the owner: fix all three in a new task. The code is fixed per deployment and never follows the host count; while the membership holds fewer hosts than k+m, stripes go round the hosts it has. Every stripe is read by the code it was stored under, so a deliberate code change leaves old windows readable until they age out instead of emptying the cache. A reader that misses after a membership change also asks the previous generation's holders (TASK-83 gives each change a generation), so a join does not hide stripes, and repair moves them to their new holders behind reads. A draining host hands its stripes to their new holders before it leaves, within the background budget, and the membership is changed one host at a time, each removal waiting for its handoff, within the drain's preStop bound.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The code is a deployment setting that never follows the number of hosts; with none set the deployment refuses to start or uses one fixed default documented in docs/hosting.md, never the table's code for the hosts seen
- [ ] #2 A stripe is read and rebuilt by the code it was stored under; a reader can rebuild windows stored under an earlier code, and a test changes the code and reads every earlier window without a store read
- [ ] #3 After a join, a reader that misses asks the previous generation's holders before the store; a test adds hosts one by one without reads in between and reads every window from the cluster
- [ ] #4 A draining host hands every stripe it holds to the window's new holder before it leaves, within the background budget and the drain's preStop bound, and reports how many it handed over and how many it could not
- [ ] #5 Removals happen one host at a time, each waiting for its handoff; a test removes three of ten hosts under 4+2 and loses no window
- [ ] #6 A new property docs/properties/autoscaling-keeps-the-cache.md states it, and a simulation replays a day of scale-ups and scale-downs and reports the hit rate before and after each change
- [ ] #7 Tests follow repo practice: synctest over platform/sim, Buggify sites with probes a campaign asserts, sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, the fingerprint test stable under shake; spec/diskcache models code changes, joins with previous-generation reads and drain handoff, every TLC run within a couple of minutes
<!-- AC:END -->
