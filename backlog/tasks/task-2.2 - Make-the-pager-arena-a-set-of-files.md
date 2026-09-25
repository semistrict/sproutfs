---
id: TASK-2.2
title: Make the pager arena a set of files
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-25 23:30'
labels:
  - security
dependencies: []
parent_task_id: TASK-2
priority: high
type: task
ordinal: 38000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 2 of the plan for splitting the pager arena (plans/), behind vmmemory.Config.Arena (ArenaShared|ArenaIsolated) and SPROUTFS_ARENA (shared|isolated). A resident page's slot becomes a file and a slot; still one file in both modes.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The arena interface makes files; the Linux and simulated arenas follow
- [x] #2 Every existing suite passes unchanged, in the simulation, the Linux pager suite and the Firecracker suite in Lima
- [x] #3 The switch exists end to end and shared is the default
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. vmmemory.Config.Arena of type ArenaMode: ArenaShared (zero value, today) and ArenaIsolated (the split), validated in New; isolated documented as running exactly like shared for now.
2. Arena interface makes files: Arena.File(ctx, offsets) returns an ArenaFile that reads, writes and releases its own slots; ZeroFile and EqualFile for the optional zero and in-place compare. Host.New makes file 0 of Config.Offsets() slots.
3. Host keeps its files (arenaFile: the file, its number, its slots.Space and its resident leases). A resident page holds its file and slot (fileSlot); extents belong to a file. Allocation, placement, residency, eviction, spill, settle, probes and Close go through the page's file. Every page is in file 0 in both modes. Settle compares in place only within one file and reads two buffers otherwise.
4. LinuxArena makes one memfd per file (NewFile); AllocatedBytes adds up the files, Close closes them all. The connection sends file 0 as today (one line).
5. Simulation arena (internal/simtest) makes files; every test fixture arena is its own single file. No assertion changes.
6. Plumb the switch: host.SupervisorConfig.Arena -> pagerConfig, SPROUTFS_ARENA in cmd/sproutfs-host/config.go with config tests, deploy/README.md env row, pager log line, a paragraph in docs/vm-memory.md.
7. Prove: go build, GOOS=linux go vet, go test ./... on the Mac; Linux pager suite in Lima; Firecracker selection in Lima; mutation guards pager-zero-new-page and pager-forget-spill.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Resident leases first moved to a map keyed by (file, slot). TestAResidentPageCostsLittleHeap then measured 757 bytes per page against 730 at HEAD, and failed at 789 in one run against its 768 bound: the 16-byte map key cost about 27 bytes per page. Each file now keeps its own map[int] of leases, which the plan's accounting asks for anyway, and the test measures 730-731 again, as at HEAD.
LinuxArena.HugePolicy is now read once from shmem_enabled when the arena is made, rather than per mapping; a kernel without THP has no such file and reads never, as before.
Linux fixture changes (not assertions): NewLinuxArena takes only the page; tests that used the arena as a file now make one with NewFile (a linuxFile helper in offsets_linux_test.go). The empty-arena ErrConfig check is now NewFile(0). Fixture arenas in vmmemory, host, vmmigrate and vmmachine gained a File method returning themselves.
Validation: go test ./... on the Mac passed (simtest campaigns, testdeterminism, vmmemory at both page sizes, probe build of vmmemory). Lima pager suite (scripts/test-vm-memory-lima.sh) passed: 466 PASS lines, no failures. Firecracker suite in Lima passed for TestFirecrackerDAXCaptureRestoreForkAndFence, TestFirecrackerLiveMigration and TestAStarterPlacesTheVMMAndItsDevices. Mutation guards pager-zero-new-page and pager-forget-spill: killed-guard.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The pager's arena is a set of files behind a new switch, shared by default. Arena.File makes an ArenaFile; each file reads, writes, zeroes, compares and releases its own slots and keeps its own held offsets and resident leases. A resident page is (file, slot); every page is in file 0 in both modes. LinuxArena makes a memfd per file; the simulation's arena and every fixture follow. vmmemory.Config.Arena (ArenaShared, ArenaIsolated) is plumbed from host.SupervisorConfig.Arena and SPROUTFS_ARENA; isolated runs exactly as shared for now. Verified by go test ./... on the Mac, the Linux pager suite and a Firecracker selection in Lima, and the two pager mutation guards. Commits 94c578e and caf8b79.
<!-- SECTION:FINAL_SUMMARY:END -->
