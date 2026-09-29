---
id: TASK-68
title: Refuse to start a host whose VMM lacks an API it will use
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-29 23:27'
labels:
  - embedder
  - firecracker
dependencies: []
references:
  - vmmachine/process_linux.go
priority: high
type: feature
ordinal: 76000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A Firecracker build without the `sync_snapshot_files` field booted VMs and failed only at their first capture on stop. That VM lost every write since its last checkpoint. The next change to the fork's API would fail the same way. The field is sent at vmmachine/process_linux.go:939.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A host refuses to start, with an error naming what is missing, when its VMM lacks any managed-memory API field the host will send
- [x] #2 The check runs at host start, before any VM is created or opened
- [ ] #3 A test starts a host against a VMM that reports an older version and sees it refused
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Fork: SPROUTFS_API_REVISION constant (1 with the managed-memory feature, 0 without) and a --sproutfs-api-revision flag that prints it.
2. vmmachine: APIRevision const, Starter.APIRevision method, CheckAPI (exact equality, like the mapping protocol), Firecracker.APIRevision runs the binary with the flag.
3. host.Start calls CheckAPI right after the Starter nil check, before any budget, pager or VM.
4. Tests: CheckAPI and Firecracker.APIRevision on the Mac; host.Start refusal as a Linux test (CI go-linux).
5. Docs: vm-memory.md Firecracker section and hosting.md Running the VMM.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Exact equality chosen: a host and its VMM already deploy in lockstep for the mapping protocol. Starter gains a required APIRevision method, so an embedder's own Starter must answer it — an optional method would let the check be skipped silently, which is the failure this task exists to stop.
The fork change (7fb5f8b62 on semistrict/firecracker@sproutfs) cannot compile on macOS: its seccompiler build dependency needs Linux. It is a const and a println; it needs a GCE build to confirm.
TestStartRefusesAnOlderVMM is linux-only; it runs in the CI go-linux job.
<!-- SECTION:NOTES:END -->
