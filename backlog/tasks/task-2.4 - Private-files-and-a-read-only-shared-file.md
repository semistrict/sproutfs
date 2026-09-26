---
id: TASK-2.4
title: Private files and a read-only shared file
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-26 14:16'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: feature
ordinal: 40000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 4 of plans/isolated-arena-2026-09-25.md, in the isolated mode only: a private file per memory region, the shared file sent read-only, the BLAKE3 digest in ReadDirty and the checked move, fork files, and the reach test.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 TestAHostileVMMReachesNoOtherVMsBytes passes in isolated mode and fails in shared mode
- [ ] #2 Every suite passes in both modes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Test mode: internal/testarena reads SPROUTFS_ARENA for every suite's fixtures; Lima scripts pass it through. Tests of shared-mode placement pin shared.
2. Refactor: session-relative file numbers (r.runAt, r.fileNumber); Mapping gains Give/Drop of files; pager-wide resident count instead of per-file; ArenaFile Close for files the pager drops.
3. Isolated placement: a private file of 2N slots per region made at admit, extents fixed at the range's home slots, the other place N+i, giving up a clean page when both are taken; no extents carved in isolated mode.
4. Shared file (one per pager until TASK-2.5), sent read-only (mode 0600, O_RDONLY reopen); loads by identity go there; peer-private loads go to the private file.
5. BLAKE3 digest in ReadDirty (lukechampine.com/blake3, MIT), moved onto the resident at retire; the checked move in bindShared of a page in another region's private file: copy to shared, check digest, revoke owner, ErrTampered + Stats.Tampered on mismatch.
6. Fork files for Share: copy at a child's populate, dropped with the seal; DROP_FILE to children.
7. Detached private files kept for idle pages; allocated-blocks check in Verify and at detach (end session on a private file; punch unheld offsets of the shared file).
8. Sim arena and fixtures multi-file with reach checks; reach test TestAHostileVMMReachesNoOtherVMsBytes; digest tests.
9. Docs; run every suite in both modes (Mac, Lima pager suite, Firecracker suite).
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
WIP 8e851d9: Mapping.GiveFile/DropFile, per-session file numbers (r.runAt/fileNumber), pager-wide held count, LinuxFile 0600 + read-only reopen + Close, internal/testarena (SPROUTFS_ARENA for suites), internal/testpager (shared sim arena/mapping with reach checks) used by simtest, host and vmmigrate tests. Shared mode: go test ./... passes except the obsolete TestAnIsolatedPagerKeepsEveryPageWhereASharedOneDoes (to be replaced). Heap test now drops the fixture's mapping record (678 B/page).

Isolated mode code in progress, uncommitted: git on this Mac now fails because the Xcode license is not accepted (owner must run sudo xcodebuild -license). New vmmemory/isolation.go: private files (2N slots, home and other place, fixed extents), read-only shared file, reach and move with a BLAKE3 check (lukechampine.com/blake3 v1.4.1 MIT, dep klauspost/cpuid/v2 MIT), fork files, countAllocated in Verify, Punch on LinuxFile.

Isolated mode implemented in the worktree (uncommitted; git blocked by the Xcode license). Mac: go test ./... passes in both modes except cmd/sproutfs-host TestOnlyTheCommandsChooseAnAdapter, which fails on the same git/Xcode error; race detector clean in isolated mode for vmmemory, simtest, host, vmmigrate. New vmmemory/isolation_test.go covers private files, the checked move, a tampered page (ErrTampered, Stats.Tampered), fork files, detached private files and the allocated-blocks check. Tests of shared-mode placement are pinned to shared. The reach test TestAHostileVMMReachesNoOtherVMsBytes is NOT written: an automated safety check stopped the session while it was being written, so it needs a human decision.

Rebased onto main (TASK-19 ephemeral pager, TASK-12, TASK-38). The simtest pagers keyed by vmmemory.Host use testpager arenas and SPROUTFS_ARENA for all three pagers, the ephemeral one included. Commits 138baa2a (the isolated arena) and 22c58d11 (docs). Proven: go test ./... passes with SPROUTFS_ARENA=shared and with isolated. just check passes. Lima has no 2 MiB hugepages, so only the 4 KiB Linux tests ran: small-page, hostile session, refused fault, refused command, transparent-page and arena-offset tests. They pass in isolated mode. Shared mode failed once: in the hostile no-descriptor case the host held 18 descriptors after the session and 19 before. It passed 3 of 3 on repeat. Not run: the 2 MiB Linux pager suite, internal/vmtest, the Firecracker suite. The main session is writing the reach test. AC 1 stays unchecked. AC 2 stays unchecked until the Lima suites run.

Reach test written and run in Lima (4 KiB): TestAHostileVMMReachesNoOtherVMsBytes passes isolated; TestASharedArenaHandsEveryVMMItsNeighboursBytes proves the shared arena hands the neighbour's pages over. A mutation sharing one private file between regions is killed. Not covered yet: the /proc/self/fd reopen and fchmod by a jailed helper user. AC2 waits on 2 MiB hugepages in Lima for the 2 MiB, vmtest and Firecracker suites.
<!-- SECTION:NOTES:END -->
