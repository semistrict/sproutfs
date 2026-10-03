---
id: TASK-81
title: Make the hosts SSDs one erasure-coded disk cache for the cluster
status: To Do
assignee: []
created_date: '2026-10-03 03:41'
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
- [ ] #2 One disk limiter covers the spill files, the ephemeral spill file, VMM staging and the cache, keeps a minimum-free-percentage, minimum-free-bytes or maximum-used goal, counts spill files at their promise, keeps a write budget, and never takes space back from a spill file
- [ ] #3 The cache is one per host, shared by every VM and tenant, laid out as 64 MiB disk regions with stripe headers and region tables, evicted only under pressure by FIFO with a bounded second chance and a free region
- [ ] #4 The cache survives a host process restart and a pod restart: a host reads pages cached before the restart without a request of the object store, a torn region is given back, and a cache file of another deployment is emptied
- [ ] #5 Each window is Reed-Solomon coded across its top k+m ranked caches; a reader decodes from any k, ignores up to m hosts that are lost, slow or marked down, tells a host to drop a wrong stripe, and repairs a missing stripe it finds
- [ ] #6 A cold burst fills a window once (fill rights), reads of the store started by the bound stay within the token bucket, and a sampled HEAD check reports a cache hit whose part is missing
- [ ] #7 In the simulation, each property in docs/properties that the plan lists has a test that states it, including a six-host 4+2 cluster and a two-host 1+1 cluster
- [ ] #8 A GCE run of six hosts against a real bucket reports, in docs/measurements, the 4+1 versus 4+2 microbenchmark, the cost of serving by copy, and an 8 GiB restore from memory, from the cluster cache, from the store, and with one host lost
- [ ] #9 The host manifest puts the cache on a local persistent volume and sets a goal and a write budget instead of SPROUTFS_CACHE_DISK_BYTES; docs/architecture.md, hosting.md, volumes.md, vm-memory.md, migration.md and context.md describe the cache as built
<!-- AC:END -->
