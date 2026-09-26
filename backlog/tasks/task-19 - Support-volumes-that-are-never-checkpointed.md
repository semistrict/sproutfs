---
id: TASK-19
title: Support volumes that are never checkpointed
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 02:04'
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
- [x] #1 A volume can be marked never checkpointed
- [x] #2 Simulation invariants: it is never published, it is lost with its host, and it never reaches a fork
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Design: an ephemeral disk is a PMEM volume no checkpoint holds, served by a third pager.
1. Refactor: MemoryRegion.OnInterval names the regions the interval checkpoints; the loss window, the pressure checkpoint, the flush and the disk seal ask it instead of comparing kinds.
2. checkpoint: VolumeSpec.Ephemeral; root Volume.ephemeral (field 7) is the one marker: name, size, page size, never a segment. Publication.Add creates a volume; a page of an ephemeral one fails Commit with ErrEphemeral before any part; decode refuses a segment of one and CheckIndex a part member of one. Index format stays 8.
3. volume: VolumeSpec.Ephemeral and Volume.Ephemeral. Writes, discards and pager sources for it are refused (ErrEphemeral); reads are zeroes. Manager.Fork(..., added) gives a child ephemeral disks; its root adds them. A cold boot may resize one either way.
4. vmmemory: the kind stays the wire kind (PMEM). Config.Ephemeral builds a third pager: dirty budget equals logical budget, no loss window, Seal takes nothing. EphemeralBacking binds a volume to that pager only. Pagers.Ephemeral and Pagers.Of.
5. host and vmmachine: SupervisorConfig.Ephemeral starts the third pager; admission charges it and refuses an ephemeral disk on a host without one; its flush completes at once; CreateRequest.Ephemeral adds the disk through the fork that creates the VM; vmmachine maps it as a second PMEM device on that pager; Prepare skips it; the handoff marks it.
6. Migration carries it (every page is unpublished); forks and CaptureInto leave it out; reopen gets it zeroed.
7. API, orchestrator, CLI (sproutfsctl create --ephemeral), host config (SPROUTFS_EPHEMERAL_BYTES, SPROUTFS_EPHEMERAL_ARENA_BYTES), metrics.
8. simtest: third pager on every host, topologies draw an ephemeral disk, guest models zero it at checkpoints and forks, scenarios for never published, lost with host and at stop, no fork, migration.
9. Docs: context, architecture, volumes, vm-memory, hosting, migration, testing, deploy README.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Step 1 done (commit: MemoryRegion.OnInterval refactor). Design change: ephemeral is not a MemoryRegionKind, because the kind is on the wire (vmwire MemoryRegion frame Flags) and Firecracker sends PMEM. It is a property of a third pager (vmmemory.Config.Ephemeral) and of the volume. Run go test packages one at a time (-p 1): running several at once was killed.

Step 2 done: checkpoint records ephemeral volumes (root field 7, Publication.Add, ErrEphemeral, CheckIndex refusal). Index format stays 8: older builds refuse the new field as unknown, which is the safe direction.

Step 3 done: volume ephemeral disks (ErrEphemeral; reads zero; Manager.Fork(..., added) adds ephemeral disks, which is how a create gives one). Next: vmmemory Config.Ephemeral pager, Pagers.Ephemeral, Seal no-op.

Step 4 done: vmmemory ephemeral pager (Config.Ephemeral, EphemeralBacking, Pagers.Ephemeral/Of, Seal no-op, OnInterval false). Next: host (pagerConfig, admission, flush, supervisor third pager, Create via Fork added), vmmachine plan.

Step 5 done: host and vmmachine run ephemeral disks. Supervisor third pager via SupervisorConfig.Ephemeral (ArenaBytes, DiskBytes); CreateRequest.Ephemeral; handoff carries Ephemeral per region. Next: cmd config and metrics, orchestrator and CLI, then simtest.

Step 6 done: orchestrator and CLI (sproutfsctl create --ephemeral; SPROUTFS_EPHEMERAL_BYTES and SPROUTFS_EPHEMERAL_ARENA_BYTES). Step 7 done in the worktree, not yet recorded in history: simtest third pager, topologies draw ephemeral disks, scenarios in internal/simtest/ephemeral_test.go pass, full simtest passes. Git refuses to run until the Xcode license is accepted (sudo xcodebuild -license) after an OS update.

Validation: go test ./... passes (33 packages). go vet and build pass for linux and darwin (with -buildvcs=false), buf lint passes, just test-shell passes. just check stops at its file listing, because version control needs the Xcode license accepted after the OS update (sudo xcodebuild -license). Not run: Lima and real Firecracker (flock is not installed on this Mac, and the Firecracker submodule is not initialized in this worktree), and check-rust. The new vmmachine plan test cross-compiles for linux/arm64 but was not run.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Ephemeral disks: a PMEM volume no checkpoint holds, for the ephemeral upper layer of an embedder. The root records only its name, size, page size and a marker. Publications, pager seals and overlay writes of it are refused. A third pager (vmmemory.Config.Ephemeral) holds its pages in its own arena and spill file, with a dirty budget equal to its logical budget and no loss window, so it never waits on or counts toward the dirty budget, the loss window or a checkpoint. Given through CreateRequest.Ephemeral, orch.CreateRequest.Ephemeral and sproutfsctl create --ephemeral. Hosts opt in with SPROUTFS_EPHEMERAL_BYTES. Lost with its host and at a stop, zeroed in every fork, carried by a migration. Verified by checkpoint, volume, vmmemory, host and cmd tests, simtest scenarios (internal/simtest/ephemeral_test.go) and generated topologies that draw ephemeral disks. go test ./... passes.
<!-- SECTION:FINAL_SUMMARY:END -->
