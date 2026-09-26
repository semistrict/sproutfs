---
id: TASK-19
title: Support volumes that are never checkpointed
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 01:45'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 19000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Its boxes with an ephemeral upper layer need a writable disk that no checkpoint holds.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A volume can be marked never checkpointed
- [ ] #2 Simulation invariants: it is never published, it is lost with its host, and it never reaches a fork
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Design: an ephemeral volume is a PMEM disk that no checkpoint holds.
1. Refactor: give vmmemory.MemoryRegionKind predicates (sealed by a capture; checkpointed on the interval) and replace the scattered Ram/Pmem comparisons in host, vmmachine and simtest.
2. checkpoint: VolumeSpec.Ephemeral; root Volume.ephemeral (the one marker: name, size, page size, no segments ever); Publication.Add creates a volume at a cold boot; Dirty on an ephemeral volume fails; decode and consistency refuse segments or members of one. Index format stays 8 (new optional field).
3. volume: VolumeSpec.Ephemeral, Volume.Ephemeral; writes through the overlay and pager sources for it are refused, so it is never published and never in a fork point; Shape.Add adds one at a cold boot and resizes need no grow-only rule for it.
4. vmmemory: third kind Ephemeral with a pager of its own (arena, spill file). Its dirty budget equals its logical budget, so admission by size means a store never waits; Seal of it takes nothing (Firecracker's snapshot seals every session). No loss window.
5. host: kind from the volume, admission refuses an ephemeral disk with no ephemeral pager, checkpointNow/oldestOf/flush exclude it, pagerConfig and supervisor start a third pager when SPROUTFS_EPHEMERAL_BYTES is set; ColdShape adds the disk at create; vmmachine maps it as a PMEM device on that pager; Prepare skips it.
6. Migration carries it (VM keeps running; its pages are served as unpublished). Fork and CaptureInto leave it out: the child gets it zeroed. Reopen after host loss: zeroed at the root's size.
7. API/CLI: hostapi.CreateRequest.Ephemeral, orch.CreateRequest.Ephemeral, orchestrator forwarding, sproutfsctl create --ephemeral SIZE; status reports the ephemeral pager.
8. simtest: third pager; guests' durable model zeroes ephemeral volumes; topologies draw an ephemeral disk; scenarios prove never published, lost with its host, never reaches a fork (local and remote), carried by migration; CheckDeployment refuses any published byte of one.
9. Docs: volumes, hosting, vm-memory, migration, context, architecture. go test ./... and just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Step 1 done (commit: MemoryRegion.OnInterval refactor). Design change: ephemeral is not a MemoryRegionKind, because the kind is on the wire (vmwire MemoryRegion frame Flags) and Firecracker sends PMEM. It is a property of a third pager (vmmemory.Config.Ephemeral) and of the volume. Run go test packages one at a time (-p 1): running several at once was killed.

Step 2 done: checkpoint records ephemeral volumes (root field 7, Publication.Add, ErrEphemeral, CheckIndex refusal). Index format stays 8: older builds refuse the new field as unknown, which is the safe direction.

Step 3 done: volume ephemeral disks (ErrEphemeral; reads zero; Manager.Fork(..., added) adds ephemeral disks, which is how a create gives one). Next: vmmemory Config.Ephemeral pager, Pagers.Ephemeral, Seal no-op.

Step 4 done: vmmemory ephemeral pager (Config.Ephemeral, EphemeralBacking, Pagers.Ephemeral/Of, Seal no-op, OnInterval false). Next: host (pagerConfig, admission, flush, supervisor third pager, Create via Fork added), vmmachine plan.
<!-- SECTION:NOTES:END -->
