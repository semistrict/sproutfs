---
id: TASK-92.12
title: 'Zircon port step 12: checkpoints and the rest in the new core'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-05 05:10'
updated_date: '2026-10-05 15:02'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.11
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 110000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 12 of the plan. Everything else of the old core runs on the new one: the seal as WritebackBegin with D3 (protect in the pause, AwaitingClean in the walk behind it), the settle, the retire as WritebackEnd and a move into the identity root of the checkpoint that published the page, the abandon (D4), a fork point sharing its sealed pages as a temporary identity root, read dirty, the loss window and pressure, eviction and spill, cold copies and the give-back as the zero-fork scan widened to the origin, the isolated arena with tenants, moves and fork files, serving a peer and handoff. If it does not fit one reviewable change, it is divided between the seal and eviction.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The whole vmmemory suite, and the host, vmmigrate and internal/simtest suites, pass under SPROUTFS_PAGER_CORE=zircon in both arena modes; the named list is gone and just check runs them all under both cores
- [ ] #2 Every guard in scripts/mutation/guards.json is killed under both cores, every Buggify site fires and every probe is reached in the campaigns that require them, under both cores
- [ ] #3 The probe build audit (stable, bind, granted, retired, reshared) runs on the new core, and probe_internal_test.go passes against it
- [ ] #4 The seal pause issues only range protections, shown by the seal pause tests, and the walk runs after the vCPUs resume
- [ ] #5 The hostile, race and isolation Linux suites pass under the new core on GCE
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Split into green commits along the plan's lines, merging main between them.
1. Checkpoints: seal (D3: the pause protects the runs of the dirty set; the walk behind it, with the region held, makes the resident pages of the set AwaitingClean in the region's layer), the checkpoint's copies as detached bindings beside the layer that own the reservations, D1 split of a store into an AwaitingClean page with the pager's own copy (zirconvm SplitAwaitingClean), settle, retire (WritebackEnd, then each page taken out of the layer and supplied to the identity root of the checkpoint that published it; a page the volume holds no object for is dropped), abandon (D4, WritebackAbandon), discard, read dirty, Share as a temporary identity root of lent pages, the loss window and pressure over the region's dirty set. zirconvm gains RemovePage and SplitAwaitingClean (marked departures: a frame's bytes are the pager's to move).
2. Eviction of mapped pages: a lock per frame as the current core's per resident, the evictor's synchronous path over the node's queues with the fair share, revocation across the alias set, D2 spill into the binding's reservation and refault, cold copies pinned in the zero-fork queue, give-back.
3. Isolation over frames: reach, moves into the shared file with the digest check, fork files for lent pages, endFork.
4. Serving and handoff (ReadResident, Resident, Unpublished, Handoff), peer (UnpublishedLoader) backings, the probe build's audit over both cores.
5. The list grows to the whole suite; check-zircon-core runs the vmmemory, host, vmmigrate and simtest suites under the zircon core in both arena modes; guards gain cores [current, zircon] where they live on these paths; GCE hostile/race/isolation suites under the zircon core; benchmarks both cores; Gremlins on the new files; go test -race.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Commit 1, checkpoints and eviction of mapped pages (they did not split cleanly: the walk joins a page an eviction holds, and the checkpoint tests need both). The zircon core keeps the current core's binding semantics beside the layer: a guest binding and a detached checkpoint copy per sealed page, a dirty set and sealable runs per region, and a lock per frame (zframe.mu) as the current core keeps one per resident page, taken outside every object lock. Seal = pause protects dirtyRuns (D3), walk aliases each copy to the page and WritebackBegin on each run the layer holds. D1 store = the pager copies the bytes and zirconvm SplitAwaitingClean installs its copy. Retire = WritebackEnd and the page moved (RemovePage + supply) into the identity root of its published identity; a hole is given back once checked zeros. Abandon = WritebackAbandon (D4). Settle, Share (a temporary identity root of zlent pages naming the parent's frames, wired so no eviction peeks them), read dirty, the loss window and pressure over the dirty set. Eviction: Host.reclaimStep dispatches to the zircon core, which peeks the node's queues with the fair share; the node ages Dirty and AwaitingClean pages in the reclaim queues (zirconvm Node.AgeDirtyPages, D2's queue); evictPage revokes every alias, writes the page to its bindings' reservations, takes it out of its object (RemovePage) and frees its slot; a spilled page refaults from its reservation, AwaitingClean again where it shares the copy. zirconvm gains RemovePage, HeldPageLocked, SplitAwaitingClean and AgeDirtyPages, each with a departures test. Plans lock the frames they map until their commands land; a population waits for them in identity order. Replacement is the current core's counter (zframe.replacing). scripts/test-pager-core.py gained --survey and --grow. List 111 -> 221 vmmemory tests; just check passes.

Commit 2, cold copies and the give-back: a store trap's copy of a root's page is cold and pins its origin in the zero-fork queue; the session's give-back, a seal's leaving-out, and an eviction's give-back compare it with the origin (or the volume where the origin went) and put the guest back on the origin. Guards pager-give-back-changed-copy, pager-forget-spill and spill-sparse now carry cores current and zircon and are killed under both. A race found by the list: a store's copy was unlocked before its access was resolved, so an eviction could revoke it in between (invalid resolution); the store holds its copies until it returns. List 234.

Commit 3, isolation over frames: a plan meets a root's page this region's process may not map (another region's private file, or a fork point's lent page) and reaches it with no object lock held: a published page is moved into the file its identity's pages live in, checked against its upload's digest (ErrTampered otherwise), the root holding the copy and the owner's mapping replaced in place; a lent page is copied into the point's fork file, which the child is given; a page that cannot be moved leaves its root and is the owner's own page again. Neighbours a plan cannot map are left to their own faults. A private file's two places give up an idle page (allocateOwn, over arenaFile.frames). Fork files close at endFork. List 243.

Commit 4, serving, handoff and peer backings: ReadResident, Resident, Unpublished and Handoff over the bindings beside the layer; a handed-off region verifies nothing. A peer backing attaches: a page the backing reports another host's is loaded into the region's own file as a Dirty page of its layer under a dirty reservation (the faulting page's from the waiting path, errUnpublishedReservation, as the current core's), the backing is told what was installed, a prefetch leaves such a page to its fault (ProbePrefetchHeld), and a peer store reads its page alone. ErrCoreUnsupported is gone: the zircon core serves every operation; the refusal test became TestTheZirconCoreServesARegionFromAttachToDetach. Also: a fork point that publishes the name it lent leaves its pages in the (now published) root (host TestLocalForkReceivesTheForkPointOverThePages), and an extent held by idle pages of gone regions is given back (simtest TestScatteredStoresCostMappingsPerRunAndNotPerPage). All 261 vmmemory tests that run on the Mac pass under the zircon core in both arena modes.
<!-- SECTION:NOTES:END -->
