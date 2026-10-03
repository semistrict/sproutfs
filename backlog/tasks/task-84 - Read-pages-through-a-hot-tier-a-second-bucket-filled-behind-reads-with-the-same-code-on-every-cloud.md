---
id: TASK-84
title: >-
  Read pages through a hot tier, a second bucket filled behind reads, as an
  alternative to the hosts' SSD cache
status: To Do
assignee: []
created_date: '2026-10-03 18:46'
updated_date: '2026-10-03 18:49'
labels:
  - storage
  - performance
dependencies: []
references:
  - docs/hosting.md
  - plans/disk-cache-2026-10-02.md
priority: high
type: feature
ordinal: 91000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Add a read-through hot tier to the checkpoint store, as an ALTERNATIVE to the hosts' SSD cluster cache (TASK-81). A deployment uses one or the other, never both: a host configured with both a hot tier and the cluster cache share (SPROUTFS_CACHE_CLUSTER_PERCENT above 0) refuses to start with a clear error naming both settings. It follows the pattern of Mountpoint for S3's shared cache, but built into sproutfs and free of cloud-specific features. The hot tier is a second bucket, configured by URL alone (for example a zonal bucket in the hosts' zone), reached through the same store interface and adapters as the regional bucket. A page read goes memory, then the hot tier, then the regional bucket. A miss in the hot tier is filled behind the read with a create-if-absent PUT of the same object under the same name; page objects are immutable and named by their checkpoint, so there is no version check, two hosts filling at once write identical bytes, and a stale copy cannot exist. A fault, a publication and a pull never wait for a fill; fills are bounded and dropped when over budget. A publication may also write hot checkpoints to the hot tier once the regional write is durable. The regional bucket stays the only durable copy: a hot tier that is down, lost with its zone, or emptied only costs misses. Decided 2026-10-03 by the owner: the same code must serve every cloud with no reliance on cloud-specific features (no Rapid Cache, no Mountpoint, no lifecycle rules); the hot tier and the SSD cluster cache are alternatives and may not be enabled together. Expiry of the hot tier is wanted but deferred to a later task. The measurement below compares the two alternatives for dependent faults.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The checkpoint store can be given a hot-tier bucket by URL through the existing store adapters; with none configured nothing changes
- [ ] #2 A page read tries memory, then the hot tier, then the regional bucket; a hot-tier miss or failure falls through to the regional bucket and never fails a read
- [ ] #3 A miss is filled behind the read with a create-if-absent PUT of the same object name; fills are bounded by a queue and a rate and are dropped, never waited on, by faults, publications and pulls
- [ ] #4 Publications can write each part to the hot tier once its regional PUT has succeeded, never before
- [ ] #5 No code path depends on a cloud-specific API or feature; the same build serves GCS and S3-compatible stores, shown by tests over the simulated store and a run against a real bucket on each cloud we deploy to
- [ ] #6 /status and /metrics report hot-tier hits, misses, fills sent and fills dropped by reason
- [ ] #7 Tests follow repo practice: synctest over platform/sim with a simulated hot store, Buggify sites with probes a campaign asserts (hot tier down, slow, refusing, lost replies), sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, and the fingerprint test stays stable
- [ ] #8 A GCE measurement compares dependent single reads of 2 MiB and 4 KiB pages from the regional bucket, a zonal hot-tier bucket and the hosts' cluster cache, at least 3 rounds, written up under docs/measurements
- [ ] #9 A host configured with both a hot tier and the SSD cluster cache (share above 0) refuses to start with an error naming both settings; a test covers it, and docs/hosting.md and deploy/README.md say the two are alternatives
<!-- AC:END -->
