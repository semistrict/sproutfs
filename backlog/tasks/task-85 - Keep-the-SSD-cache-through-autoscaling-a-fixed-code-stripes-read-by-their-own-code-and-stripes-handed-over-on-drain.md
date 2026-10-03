---
id: TASK-85
title: >-
  Fix the cache's erasure code per deployment and read each stripe by the code
  it was stored under
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 22:22'
updated_date: '2026-10-03 23:40'
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

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. rank: List carries the current code and the codes the deployment used before it, newest first (NewList(code, caches, earlier...), Codes, Earlier, Under). DefaultCode = 4+2, the code of a deployment that sets none. At most three earlier codes.
2. Wire: GET /caches carries earlier codes (api/host Caches.Earlier as k+m strings).
3. Orchestrator: SPROUTFS_CACHE_CODE unset is DefaultCode, never the table's code for the hosts seen; SPROUTFS_CACHE_EARLIER_CODES lists the codes it replaced. Recorded in the deployment's settings: stateless, survives an orchestrator restart, and TASK-83's membership object takes the same field. Drop mostCaches.
4. Disk: a read of the own disk tries the list's code, then each earlier code, and rebuilds only from stripes of one code. The index is already keyed by code. Writes and has() stay under the current code.
5. Cluster read: a window not rebuilt under the current code is read again under each earlier code, ranks taken from the same list under that code's width. Counted once per window. Repair runs under the code the window was read under; a holder takes a repair keep under an earlier code its list names, ranked under that code; fills and fill rights stay under the current code only.
6. Tests (synctest over platform/sim): a code change on six hosts reads every earlier window with no store read, also with a host lost; a store read after the change fills under the new code; repair of an earlier window stays under its code; orchestrator default is fixed whatever the hosts. sim.Bug guards: reader treats earlier codes as a miss, disk reads current code only, repair under the current code, orchestrator code follows host count. Each in guards.json and docs/testing.md, shown to fail its test.
7. Gremlins on rank, orchestrator caches.go, diskstripes.go, clusterread.go, fill.go with --file/--run, before/after.
8. spec/diskcache: codes as a sequence, a deliberate ChangeCode, stripes carry their code, reads try current then earlier codes, repair under the read's code; SurvivesLosses per code; mutant current-code-only; every TLC run within a couple of minutes.
9. Docs: hosting.md (The list of caches, The code), context.md (Code, Stripe, Keep), plan Small clusters, testing.md.
10. Merge main, just check, commit.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design decisions (2026-10-03):
- Default code is a fixed 4+2 (rank.DefaultCode), set by loadConfig when SPROUTFS_CACHE_CODE is unset; mostCaches and the table lookup are gone from the orchestrator. CodeFor stays only as the operator's table.
- Earlier codes are recorded in the deployment's settings (SPROUTFS_CACHE_EARLIER_CODES, newest first, at most 3) and carried in GET /caches as 'earlier'. Stateless, survives an orchestrator restart, and maps onto a field of TASK-83's membership object. Rejected: orchestrator state (lost on restart, and TASK-83 replaces it), each disk's own record (a joining host would not know peers' codes).
- The disk index was already keyed by code (entries carry k, m; lookup by diskCode); no change needed.
- A read tries the list's code, then each earlier code, each a separate windowRead with ranks from list.Under(code) (rendezvous order does not depend on the code). Join takes one code only.
- Repair only under the list's code. A window rebuilt under an earlier code is refilled under the current code as a read of the store is (needs the fill right), instead of being repaired under the old code. So holders never take keeps of an earlier code, ranked() is unchanged, and hot windows migrate to the new code while cold ones age out.
- New counter earlier_hits in cache_read and sproutfs_cache_read_earlier_hits_total: tells the operator when an earlier code can be dropped.
<!-- SECTION:NOTES:END -->
