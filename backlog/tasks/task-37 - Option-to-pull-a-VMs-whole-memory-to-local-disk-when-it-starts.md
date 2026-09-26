---
id: TASK-37
title: Option to pull a VM's whole memory to local disk when it starts
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 01:37'
updated_date: '2026-09-26 16:52'
labels:
  - embedder
  - performance
dependencies: []
documentation:
  - docs/vm-memory.md
  - docs/hosting.md
priority: medium
ordinal: 43000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Today a started VM reads each page from object storage the first time the guest touches it, and a clean page the pager evicts is read from object storage again. Every cold fault pays object-store latency for the life of the VM. Some VMs should pay that cost once, up front and in the background: when such a VM starts, the host pulls every page its checkpoint holds onto the host's local disk. The pages need not become resident in memory. From then on, a fault on a page that is not resident reads from local disk rather than from object storage. The option applies to every way a VM starts on a host: create, open, fork and a migration's receive.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A start option, in the host API and the CLI, marks a VM to pull its whole memory to local disk
- [x] #2 With the option set, every page of the selected checkpoint is fetched onto local disk in the background after the start, and the guest runs while the fetch runs
- [x] #3 Once the fetch completes, a fault on a page that is not resident, including one evicted earlier, reads local disk and makes no object-store request
- [x] #4 A guest fault on a page the fetch has not reached yet is served at once, not queued behind the fetch
- [x] #5 The local copy never counts as durable: losing the disk or the host loses nothing a checkpoint holds, and a newer checkpoint's pages supersede the cached ones
- [x] #6 The local disk space is bounded and configured, and a VM that does not fit falls back to reading from object storage
- [x] #7 Tests prove the zero object-store reads after the fetch, the fault priority and the fallback; docs/vm-memory.md and docs/hosting.md describe the option
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Give the page cache a disk tier (checkpoint.CacheConfig.Disk, DiskBytes): encoded members and segments on the node disk, keyed by page and segment identity like the memory tier. Space is taken per pull as one region of 4 KiB blocks, sized exactly from the root's recorded bytes; a page another pull already holds is shared by reference, and a region is freed when no pull holds it.
2. Read path: a miss in memory reads the disk tier before object storage (pages and segments). A disk read or an envelope check that fails falls back to the store.
3. checkpoint.Store.Pull(index): admission (refuse when the region does not fit or no disk is configured), then a background fetch of every segment and member in extents, host-wide two at a time, yielding while fault loads are in flight, never joining or blocking a fault's fetch. Close releases the pull's hold.
4. volume.VM.Pull pulls the checkpoint the VM's view inherits. host: AddPullingMachine marks a registration; its run loop pulls while the machine runs and releases on end. Create, open, fork (ForkRequest.Pull on every child's handoff) and receive (Handoff.Pull, which a migration carries from the source) all set it. Receive pulls only the checkpoint; the pages no checkpoint holds already arrive through the post-copy into the pager.
5. API and CLI: pull on host create/open/fork requests, orchestrator create/start/fork, sproutfsctl --pull; SPROUTFS_CACHE_DISK_BYTES; per-VM pull report.
6. Tests: checkpoint-level (zero GETs after pull including evicted pages, fault served while the pull's fetch is blocked, capacity fallback, lost disk falls back, newer checkpoint supersedes), host-level pager eviction with counted GETs, CLI parse.
7. Docs: volumes.md page cache, vm-memory.md, hosting.md budgets and the option.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Design: the page cache (checkpoint.Cache) gains a disk tier (CacheConfig.Disk/DiskBytes; SPROUTFS_CACHE_DISK_BYTES, off by default, 4 GiB in deploy/10-host.yaml with the emptyDir raised to 24Gi). It stores each member's and segment's encoded envelope keyed by page/segment identity, so a disk read is checked by the same envelope and a damaged or lost copy falls back to the store and is forgotten. A pull reserves one region sized exactly from the root's recorded bytes (segment lengths + per-checkpoint read bytes) or is refused whole (ErrDiskFull/ErrNoDisk); pages another pull copied are shared by reference; regions go when the last pull releases them. Priority: the pull uses none of the cache's load slots, joins no flight, waits for no fault load in flight (Cache.quiet) and all pulls share 2 requests. Host: AddPullingMachine marks the registration; the pull runs as a third goroutine of the machine's run loop and is released when the machine ends. Receive pulls the opened checkpoint only; pages no checkpoint holds arrive through post-copy into the pager. Migration carries Handoff.Pull; fork sets it on children. Not covered: the orchestrator does not remember the mark for a recover after host loss; pages a later checkpoint publishes are not pulled; the Linux supervisor wiring (boot/Create/Open/Fork) is compiled and vetted but not run on Firecracker.

Validation: just check passed (gofmt, build and vet for linux and darwin, go test ./..., buf lint, shellcheck, shell suites, rust). New tests: checkpoint/pull_test.go (zero store requests after a pull, reads not queued behind a held pull fetch, pull waits while a fault reads the store, refusal when full or no disk, damaged and failed disk fall back, newer checkpoint supersedes, pulls share one copy); host/pull_test.go (pager evicting 10 of 12 faults with zero object GETs after the pull, fallback when it does not fit, migration carries the mark); orchestrator pull_test.go; sproutfsctl and sproutfs-host config tests. Mutation checks: disabling the disk read fails the host AC3 test with 13 GETs; removing the fault yield fails the priority test.

Firecracker qualification (Lima aarch64, scripts/test-firecracker-lima.sh, SPROUTFS_FIRECRACKER_RUN=^TestPulledGuestsFaultWithoutTheObjectStore$): vmmachine/pull_linux_test.go runs the whole supervisor (host.Start) with a 24 MiB PMEM arena. Create with Pull; the guest writes 40 MiB of random data to its DAX root and stops; Open with Pull; the guest reads the fill twice, evicting at least 16 PMEM pages; zero checkpoint-object GETs. Fork with Pull; the child shares the parent's copy of the fill and reads it twice with zero GETs. Mutation: disabling disk reads gives 51 GETs. Two bugs found and fixed: the supervisor's status listing dropped VM.Pull, and a fork child pulled its parent's checkpoint instead of its own root, which left the parent's republished unpublished pages to the store. The child's pull now waits for volume.VM.Rooted.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added a disk tier to the page cache that a pull fills with every page and segment of a VM's starting checkpoint, keyed by identity, sized exactly from the root and refused whole when it does not fit. Faults read memory, then the disk, then the store; damaged copies fall back. The pull runs behind faults with no shared slots or flights. Pull is a start option on host create/open/fork, carried by migration handoffs, exposed in the orchestrator and sproutfsctl --pull, configured by SPROUTFS_CACHE_DISK_BYTES. Verified by checkpoint, host, orchestrator and CLI tests and just check; docs in hosting.md, volumes.md, vm-memory.md, migration.md, context.md.
<!-- SECTION:FINAL_SUMMARY:END -->
