---
id: TASK-37
title: Option to pull a VM's whole memory to local disk when it starts
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-26 01:37'
updated_date: '2026-09-26 16:07'
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
- [ ] #1 A start option, in the host API and the CLI, marks a VM to pull its whole memory to local disk
- [ ] #2 With the option set, every page of the selected checkpoint is fetched onto local disk in the background after the start, and the guest runs while the fetch runs
- [ ] #3 Once the fetch completes, a fault on a page that is not resident, including one evicted earlier, reads local disk and makes no object-store request
- [ ] #4 A guest fault on a page the fetch has not reached yet is served at once, not queued behind the fetch
- [ ] #5 The local copy never counts as durable: losing the disk or the host loses nothing a checkpoint holds, and a newer checkpoint's pages supersede the cached ones
- [ ] #6 The local disk space is bounded and configured, and a VM that does not fit falls back to reading from object storage
- [ ] #7 Tests prove the zero object-store reads after the fetch, the fault priority and the fallback; docs/vm-memory.md and docs/hosting.md describe the option
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
