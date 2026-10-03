---
id: TASK-81
title: Make the hosts SSDs one erasure-coded disk cache for the cluster
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 03:41'
updated_date: '2026-10-03 04:53'
labels:
  - performance
  - storage
dependencies: []
references:
  - docs/research/lambda-container-loading-2026-10-02.md
  - docs/research/cachelib-navy-2026-10-02.md
  - docs/research/groupcache-2026-10-02.md
  - docs/research/memcache-facebook-2026-10-02.md
documentation:
  - plans/disk-cache-2026-10-02.md
  - docs/properties/README.md
priority: high
type: feature
ordinal: 88000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A cold page fault on GCS costs 40 to 58 ms for a 2 MiB page (GCE, 2026-09-23), and every refault of a page the pager evicted pays it again. The hosts have large, fast local SSDs that are mostly unused: the page cache disk holds only the pages of VMs marked to pull, only while they run, up to a fixed cap that is off by default, and it is truncated at every restart. A page from the cluster SSDs should cost 1.5 to 3 ms. The design is plans/disk-cache-2026-10-02.md: the hosts SSDs form one cache, each window Reed-Solomon coded across the hosts ranked for it by weighted rendezvous hashing (4+2 by default, down to 1+1 on two hosts), filled by store reads and publications, surviving restarts on a local persistent volume, under one disk limiter with space goals and a write budget. The desired properties it serves are in docs/properties/, and the research behind it is in docs/research/*-2026-10-02.md. Decided with the owner on 2026-10-02: no tier of whole local copies; the erasure code over owner-plus-copies; support down to two hosts; the cache persists across pod restarts; a deleted tenant pages are not purged at once; pull becomes a prefetch with no guarantee.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 spec/diskcache checks NoWrongBytes, StripesRanked, SurvivesLosses, PromisesKept, GoalKept and EvictionProgresses in the 2+1, 1+1 and 2+2 configurations, and each mutant the plan names fails with the property it names; just check-spec runs it
- [ ] #2 One disk limiter covers the spill files, the ephemeral spill file, VMM staging and the cache; it takes any combination of minimum free bytes, minimum free percentage and maximum bytes used, the strictest binding and reported; it smooths its readings, shrinks the cache gradually across a band above the floor, counts spill files at their promise, keeps a write budget, and never takes space back from a spill file
- [ ] #3 The cache is one per host, shared by every VM and tenant, laid out as 64 MiB disk regions with stripe headers and region tables, evicted only under pressure by FIFO with a bounded second chance and a free region
- [ ] #4 The cache survives a host process restart and a pod restart: a host reads pages cached before the restart without a request of the object store, a torn region is given back, and a cache file of another deployment is emptied
- [ ] #5 Each window is Reed-Solomon coded (klauspost/reedsolomon) across its top k+m ranked caches, 4+2 by default and set per deployment down to 1+1 on two hosts, wrapping round a shorter list without changing code; a reader decodes from any k, ignores up to m hosts that are lost, slow or marked down, tells a host to drop a wrong stripe, and repairs a missing stripe it finds
- [ ] #6 A cold burst fills a window once (fill rights), reads of the store started by the bound stay within the token bucket, and a sampled HEAD check reports a cache hit whose part is missing
- [ ] #7 In the simulation, each property in docs/properties that the plan lists has a test that states it, including a six-host 4+2 cluster and a two-host 1+1 cluster
- [ ] #8 Concurrent code is tested in testing/synctest bubbles over platform/sim, with sim.Scheduler exploring completion orders by seed; new code has sim.Buggify fault sites whose probes a campaign asserts it reached, sim.Bug guards listed in scripts/mutation/guards.json with the test that kills each, and a Gremlins run with meaningful survivors killed
- [ ] #9 A GCE run of six hosts against a real bucket reports, in docs/measurements, the 4+1 versus 4+2 microbenchmark, the cost of serving by copy, and an 8 GiB restore from memory, from the cluster cache, from the store, and with one host lost
- [ ] #10 The host manifest puts the cache on a local persistent volume and sets goals and a write budget instead of SPROUTFS_CACHE_DISK_BYTES; docs/architecture.md, hosting.md, volumes.md, vm-memory.md, migration.md and context.md describe the cache as built
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Follows plans/disk-cache-2026-10-02.md, step by step; each step is its own commit with tests and docs.
0. spec/diskcache: the TLA+ model and its mutants, wired into just check-spec.
1. The disk limiter: DiskSpace for the simulated disk, space goals, spill promises, the cache's share, the write budget; host wiring, status and metrics.
2. The log: 64 MiB disk regions, stripe headers, region tables, window index, FIFO with a bounded second chance and a free region; pulls stop holding regions.
3. Restarts: header, tables read back in order, scan of a lost table, deployment check; the cache file is no longer truncated.
4. Ranks: cache identity and weight, the orchestrator's GET /caches, weighted rendezvous over windows.
5. The code: Reed-Solomon stripes (klauspost/reedsolomon), decode from any k, wrong-stripe detection, wrap-around for short lists.
6. Filling the cluster: keeps from store reads and publications, fill rights, duplicate drop, bounded rate.
7. Reading from the cluster: stripe reads, bound and token bucket, marking hosts down, drop of wrong stripes, repair, per-peer bounds, sampled HEAD check.
8. Serving without a copy, after measuring the cost of a copy.
9. Pull as a prefetch.
10. Deployment and docs.
Measurements on GCE: the 4+1/4+2 microbenchmark before step 5, the copy cost before step 8, the 8 GiB restore after step 7.
Steps 0, 1 and 2 run in parallel in separate worktrees; the limiter and the log meet at one interface the cache defines.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Decisions with the owner, 2026-10-02 and 2026-10-03:
- No tier of whole local copies; the hosts SSDs are one cache for the cluster (a local hit saves ~0.5 ms; duplicates cost cluster capacity and a second write).
- Erasure code over owner-plus-hot-copies, following Lambda (tail latency over ~4000 windows per restore, no hit lost to a drained host, hot load spread without detection, constant work).
- Reed-Solomon via klauspost/reedsolomon (MIT, checked); 4+2 by default so one host can be drained and another slow; the microbenchmark falls back to 4+1 if 4+2 does not cut that tail.
- Deployments down to two hosts: the code is set per deployment (1+1 for 2 hosts, 2+1 for 3, 2+2 for 4-5, 4+2 for 6+), never derived from the live list; stripes wrap round a shorter list, so a drain never changes the code.
- The cache persists across pod restarts on a local persistent volume; a deleted tenant pages are not purged at once.
- Pull becomes a prefetch into the cluster cache with no guarantee.
- Limiter goals combine as in FoundationDB Ratekeeper: any of minimum free bytes, minimum free percentage, maximum used; the strictest binds; readings are smoothed; the cache shrinks gradually across a band above the floor (the spring).
- Testing: synctest bubbles over platform/sim and sim.Scheduler, Buggify sites with probes, sim.Bug guards in scripts/mutation/guards.json, Gremlins, and the TLA+ model with mutants. From FoundationDB: a drifting simulated free space (sim2 getDiskBytes), read-side bit flips and delays (AsyncFileChaos), and a write checker that tells a lying disk from a cache bug (AsyncFileWriteChecker).
- GCE VMs may be used without asking; every VM is deleted after.
Research behind the plan: docs/research/*-2026-10-02.md.

Step 1 done (fc2f6dd3, merged 0c331740): resource.DiskLimiter with combined goals, smoothing, the band, hysteresis, spill promises counted whole, write budget from platform.DeviceWrites; host wiring, /status and /metrics, settings SPROUTFS_DISK_FREE_BYTES/_FREE_PERCENT/_USED_BYTES/_BAND_BYTES and SPROUTFS_CACHE_WRITE_BYTES_PER_DAY/_BURST_BYTES (default keeps 10 % free). sim.Disk gained filesystem size, drifting outside writers and a device write counter. Six disklimit-* guards; Gremlins on resource: 167 to 198 killed, the rest judged equivalent. Not yet connected to the page cache; SPROUTFS_CACHE_DISK_BYTES still counted. deploy/10-host.yaml's per-concern comment is out of date until step 10.

2026-10-03: the read path asks k+1 of the first k+m ranks, chosen by a hash of the reader and the window, and the rest only after an adaptive delay (about p95 of recent stripe latency) under a FoundationDB-style budget (+1/20 per read within the delay, -1 per second request). Holders return any index of the window they hold and readers decode from any k (B5 from spec/diskcache). Repair sends only an index no rank holds. Why: the stripe benchmark (docs/measurements/gce-stripes-2026-10-03.md) showed asking all k+m holders makes 4+2's tail worse than whole reads at 9,000 reads/s (p99 110 ms, p99.9 466 ms), while 4+2 is the only code that survives a drained and a slow host together. The benchmark gains this read pattern and its full-load pass runs again before step 7. The transport work is TASK-82.
<!-- SECTION:NOTES:END -->
