---
id: TASK-81
title: Defer the index object as an evolution of the index layout
status: Done
assignee:
  - '@claude'
created_date: '2026-10-02 08:40'
updated_date: '2026-10-02 09:48'
labels:
  - checkpoint
dependencies: []
references:
  - docs/volumes.md
ordinal: 88000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
TASK-66 (branch claude/lsvd-log-layout, docs/measurements/layouts-2026-09-28.md there) compared an LSVD-style log layout with the index layout. Its saving came from not rewriting index segments on every checkpoint: one PUT round instead of two, and almost no metadata uploaded (48% of a checkpoint's upload at a 1 s interval, 3% at 60 s). Ramon wants that as a simple evolution of the current format, not a second layout: write the index object only every n checkpoints, and let the checkpoints between carry what changed in their last part. The branch's separate layout (map objects, a whole-VM listing on open, its own reclaim, no pull) is not to be merged.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A checkpoint may defer its index object: its last part carries what an open needs to rebuild its root from the newest index object before it, and that part's PUT, after every earlier part is durable, is the commit
- [x] #2 A store setting says how many checkpoints may pass between index objects; its default writes one every checkpoint, which is today's behaviour and format byte for byte
- [x] #3 The index object a checkpoint writes holds every segment changed since the last one, so the format of an index object and of an open of one is unchanged
- [x] #4 Open, read, locate, compaction, reclamation, pins, forks, pull and the consistency check work on a checkpoint that deferred its index
- [x] #5 A test runs deferring and non-deferring stores in lockstep over overwrites, zeroed pages, a shrink, compaction and a fork, and requires the same bytes and page identities, fresh and reopened
- [x] #6 The layout comparison is rerun against the deferred index and docs record the result
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. PartTable gains an optional DeferredIndex record: the root without segments, the base sequence (the newest index object it rebuilds from), the deferred checkpoints between, and the pages it zeroed. Only the last part of a deferring checkpoint sets it.
2. checkpoint.Config.IndexEvery bounds the checkpoints between index objects; 0 and 1 write one every checkpoint. A VM's first checkpoint and a fork's first always write one.
3. A segment a deferred checkpoint changed is pending: its entry has no address and the index holds it decoded. The next index object writes every pending segment beside the ones it changed.
4. A deferring index names its base and every deferred checkpoint since, so reclamation, pins and DeleteVM spare them unchanged; compaction does not rewrite them.
5. Open reads the index object as now; when there is none it lists the checkpoint's parts, reads the last table, then reads the base index and every deferred table at once and replays them forward.
6. Pull and CheckIndex take a pending segment from memory.
7. Lockstep test against a non-deferring store; the commit-ordering and crash cases; port the comparison and rerun it.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The deferred index rides in PartTable.deferred of a checkpoint's last part (checkpoint/deferred.go). Config.IndexEvery bounds the chain; 0/1 is today's format byte for byte. A deferred index names its base, the chain, and every checkpoint whose index object holds a base segment (Index.holds): the comparison found reclamation deleting an index object a replay still needed when only replays were named; TestDeferredIndexKeepsTheSegmentsItsBaseAddresses covers it. The last part is put only after every earlier part is durable (TestDeferredCommitWaitsForEveryEarlierPart, which fails with the wait removed); the TASK-66 log layout put it first. Not wired into the host. The deployment audit (volume/consistency.go) still classifies an unreached deferred checkpoint as a publication that never finished, because it has no index object.

Open batches the base segments a replay changes into one ranged GET per index object (41 to 12 requests per open at deferred/16). Validation: go test ./checkpoint (TestDeferredIndexMatchesAnIndexEveryCheckpoint, TestDeferredCommitWaitsForEveryEarlierPart, TestDeferredIndexKeepsTheSegmentsItsBaseAddresses, TestIndexObjectHoldsEveryPendingSegment, TestPullOfADeferredIndex, TestDeferredOpen*); every existing checkpoint test passes at the default; go test ./... green except vmmachine TestAdversarialStarters and TestAFailedReleaseIsReportedOnce, which fail identically on main in this container (operation not permitted); go vet linux+darwin; buf lint; determinism. just compare-index ran in 576 s.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Checkpoints may defer their index object (Config.IndexEvery, off by default): the last part carries a DeferredIndex and its PUT, after every earlier part, is the commit; the next index object writes every pending segment, so the index object format and open are unchanged. Open of a deferred checkpoint lists its parts and replays the chain from the base. Measured in docs/measurements/deferred-index-2026-10-02.md: at a 1 s interval the index falls from 48% to 5% of upload and commits halve, for an open of 56 ms instead of 10 ms. Verified by the lockstep, crash-ordering, reclamation, pending-segment and pull tests.
<!-- SECTION:FINAL_SUMMARY:END -->
