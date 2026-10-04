---
id: TASK-84
title: >-
  Read pages through a hot tier, a second bucket filled behind reads, as an
  alternative to the hosts' SSD cache
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 18:46'
updated_date: '2026-10-04 13:51'
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
- [x] #1 The checkpoint store can be given a hot-tier bucket by URL through the existing store adapters; with none configured nothing changes
- [x] #2 A page read tries memory, then the hot tier, then the regional bucket; a hot-tier miss or failure falls through to the regional bucket and never fails a read
- [x] #3 A miss is filled behind the read with a create-if-absent PUT of the same object name; fills are bounded by a queue and a rate and are dropped, never waited on, by faults, publications and pulls
- [x] #4 Publications can write each part to the hot tier once its regional PUT has succeeded, never before
- [x] #5 No code path depends on a cloud-specific API or feature; the same build serves GCS and S3-compatible stores, shown by tests over the simulated store and a run against a real bucket on each cloud we deploy to
- [x] #6 /status and /metrics report hot-tier hits, misses, fills sent and fills dropped by reason
- [x] #7 Tests follow repo practice: synctest over platform/sim with a simulated hot store, Buggify sites with probes a campaign asserts (hot tier down, slow, refusing, lost replies), sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, and the fingerprint test stays stable
- [ ] #8 A GCE measurement compares dependent single reads of 2 MiB and 4 KiB pages from the regional bucket, a zonal hot-tier bucket and the hosts' cluster cache, at least 3 rounds, written up under docs/measurements
- [x] #9 A host configured with both a hot tier and the SSD cluster cache (share above 0) refuses to start with an error naming both settings; a test covers it, and docs/hosting.md and deploy/README.md say the two are alternatives
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Refactor: every read of a checkpoint object (index tail, root, segment, member, extent, part table) is one operation over one object key, run against a tier (an object store plus what the read learned of the object). readRange/readSuffix take the tier. No behaviour change.
2. checkpoint/hottier.go: HotTierConfig {Store, Bound, QueueBytes, BytesPerSecond, SkipPublications, HeadCheckEvery, Clock}, NewHotTier, Stats, Settle, Close. Store.Config.HotTier. A read runs against the hot tier under a bound; a hit returns; a miss (NotFound) or any failure (error, past the bound, bytes that do not decode) runs the same read against the regional bucket. A miss is handed to one fill worker behind the read: GET the whole object from the regional bucket, create-if-absent PUT to the hot tier (precondition = already there). Queue bounded in bytes, rate in bytes/s, both drop. Three failures in a row mark the hot tier down for a while; a sampled HEAD of the regional object checks a hit's object still exists.
3. Publications hand each part to the hot tier once its regional PUT succeeded, in part order (handOver), and the index object after its PUT.
4. Mutual exclusion: NewStore refuses a hot tier beside a cache that fills the cluster; host.StartHost and sproutfs-host config refuse with both setting names. SPROUTFS_HOT_TIER is a URL (gs://bucket/prefix, s3://bucket/prefix, ?endpoint= for emulators) through adapters.
5. /status and /metrics: hot-tier hits, misses, failures, fills sent, present, dropped by reason.
6. Tests: synctest over platform/sim with a second sim object store; exact-expectation tests per AC; Buggify sites (hot down, slow, refusing PUT, lost replies, partial objects) with probes asserted by a scheduler campaign; sim.Bug guards (fill before regional PUT, read fails when hot fails, both caches allowed, ...) in guards.json and docs/testing.md; simtest world option with hot tier, fingerprint stable under shake; Gremlins on hottier.go.
7. GCE: Rapid bucket investigation; restorebench hot tier mode with dependent single reads (2 MiB, 4 KiB); measure regional, hot warm, cluster cache; report in docs/measurements/gce-hot-tier-2026-10-03.md.
8. Docs: hosting, volumes, context, deploy/README, properties.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Rapid bucket: docs (cloud.google.com/storage/docs/rapid/rapid-bucket) say reads work over JSON, XML and gRPC but writes only over gRPC BidiWriteObject (appendable objects); JSON and XML writes are not supported. Creating one in us-east4-a and us-central1-a failed: HTTP 400, project lacks rapid_zonal_bytes quota. Our adapter's create-if-absent PUT is a JSON upload, so a Rapid bucket cannot be a hot tier without a cloud-specific API.

Design: every read of a checkpoint object is one operation over one object run against a tier (checkpoint/tier.go); Store.readObject runs it against the hot tier under a bound, then the regional bucket on a miss or any failure, so corrupt or partial hot bytes fall through too. checkpoint.HotTier (hottier.go): one fill worker, queue bounded in bytes (256 MiB), rate 128 MiB/s, both drop; a miss GETs the whole regional object (or uses the bytes the read already holds) and PUTs it create-if-absent; publications hand each part over after its regional PUT, in part order, and the index object after its PUT (SkipPublications turns this off). Three failures in a row mark the hot tier down for 10 s. One hit in HeadCheckEvery (10,000) HEADs the regional object. Mutual exclusion in checkpoint.NewStore (ErrHotTierBesideClusterCache), host.StartHost and sproutfs-host config (SPROUTFS_HOT_TIER and SPROUTFS_CACHE_CLUSTER_PERCENT). SPROUTFS_HOT_TIER is gs://bucket/prefix or s3://bucket/prefix with ?endpoint=.

Evidence: checkpoint hot tier tests (exact counts), TestHotTierSurvivesItsFaultsAndReachesItsProbes (12 seeds, all 5 sites fire, all 16 probes reached), TestAHotTierReadsAndFillsThroughEveryProvidersAdapter (GCS and S3 emulators), host tests, config tests, metrics tests; guards hot-tier-fill-before-durable, hot-tier-read-fails, hot-tier-beside-cluster, hot-tier-fill-waits, hot-tier-unbounded-queue each fail their named test; fingerprint test has a hot tier arm, seeds 1-25 of it stable under shake; Gremlins hottier.go 49->51 killed of 77 (7 alive justified), tier.go 22/24; just check exit 0 (TestHostForksEveryChildFromOnePause flakes ~3/400 on main too).

AC8 not met as written: no zonal bucket on GCS takes standard-API writes (Rapid Bucket writes only via gRPC BidiWriteObject; creating one also failed on rapid_zonal_bytes quota). Measured a second regional bucket in us-east4 as the hot tier on two n2-standard-4 hosts (1+1, cluster read from own SSD), quota allowed no more: warm hot 54.7/28.6 ms p50 (2 MiB/4 KiB) vs regional 46.1/27.3 vs cluster 9.4/0.20; docs/measurements/gce-hot-tier-2026-10-03.md. A six-host 4+2 run was not possible while another agent held 24 of 32 vCPUs.

Clarified 2026-10-03 by the owner: 'not cloud specific' means not depending on a cloud's managed features (Rapid Cache, Mountpoint, lifecycle rules); differences between cloud APIs are trivial and fine. So the hot tier writes a GCS Rapid zonal bucket through GCS's gRPC append API (BidiWriteObject, appendable objects) in the GCS adapter, and the same hot tier would use S3 Express One Zone directory buckets through the S3 adapter. The GCP measurement against a real Rapid bucket needs the project's rapid_zonal_bytes quota raised (Cloud Quotas API), which the owner does.

AWS run 2026-10-04 (docs/measurements/aws-hot-tier-2026-10-04.md, scripts/bench-hot-tier-aws.sh). Decisions: S3 Express One Zone directory buckets need nothing in the S3 adapter but the name (SDK does CreateSession and the zonal endpoint); conformance against a real directory bucket passed create-if-absent (If-None-Match gives 412), If-Match, ranges, deletes; ETags are opaque (same bytes, different ETag); listings come back unordered and only under a prefix ending in /, so S3ObjectStore.List refuses a directory bucket (ErrUnorderedListing, errors.ErrUnsupported) and a directory bucket is a hot tier only. restorebench node takes -cloud aws. Hosts m7i.xlarge (Sapphire Rapids with SHA, 16 GiB so the fill queue holds a publication; same core/network/EBS limits as c7i.xlarge) in use1-az4; gp3 64 GiB, 16000 IOPS, 500 MiB/s per host. Results, median of 3 rounds, p50 2 MiB/4 KiB: regional S3 Standard 102/26.3 ms, warm hot tier on Express 16.9/4.20 ms, cluster on gp3 4+2 6.23/1.24 ms; cold hot tier all hits by the third walk. Cost for 10 TiB at 1000 page reads/s: hot tier $4,285/month at 2 MiB (retrieval $3,080) and $1,211 at 4 KiB; shards $1,270 and $1,229. AC8 (GCE zonal) still open: the zonal comparison is now done on AWS instead.
<!-- SECTION:NOTES:END -->
